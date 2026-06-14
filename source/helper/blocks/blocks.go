// Package blocks is the universal block model over the db log: chat
// messages, document blocks and tasks are all blocks. A block is an atom
// with a stable BID; its content is a hash-DAG of immutable versions
// (entries). The current value is the DAG heads — one head = resolved,
// many = unresolved variants (tabs). Nothing is edited or deleted in
// place: "update"/"delete"/"merge" are new versions. See docs/BLOCKS.md.
//
// Mapping onto db.Frame: Scope = channel, Author = writer, Type =
// "block.<blockType>" (cleartext, indexed for search), Payload = the op
// (bid, op, parents, pos, content). The version-DAG (op.parents = entry
// IDs) is distinct from the per-author frame chain (Frame.Prev).
package blocks

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/vseplet/f2f/source/helper/db"
)

// op kinds.
const (
	OpCreate = "create"
	OpUpdate = "update"
	OpMove   = "move"
	OpDelete = "delete"
	OpMerge  = "merge"
)

const typePrefix = "block."

// Signer authors entries (satisfied by *identity.Identity).
type Signer interface {
	Sign(msg []byte) []byte
	PubHex() string
}

// op is the payload stored inside an entry for a block operation.
type op struct {
	BID     string          `json:"bid"`
	Op      string          `json:"op"`
	Parents []string        `json:"parents,omitempty"` // entry IDs this version supersedes/merges
	Pos     string          `json:"pos,omitempty"`     // fractional index within container
	Parent  string          `json:"parent,omitempty"`  // containing block bid ("" = root)
	Content json.RawMessage `json:"content,omitempty"`
}

// Version is one node of a block's version-DAG (one entry).
type Version struct {
	EntryID string          `json:"entry_id"`
	Author  string          `json:"author"`
	Lamport uint64          `json:"lamport"`
	TS      int64           `json:"ts"`
	Op      string          `json:"op"`
	Content json.RawMessage `json:"content,omitempty"`
}

// Block is the folded current state of one atom: its heads (>1 = variants
// to show as tabs) plus position/parent from the latest head.
type Block struct {
	BID     string    `json:"bid"`
	Type    string    `json:"type"`    // e.g. "text", "task", "msg"
	Channel string    `json:"channel"` // = scope
	Parent  string    `json:"parent,omitempty"`
	Pos     string    `json:"pos,omitempty"`
	Heads   []Version `json:"heads"`   // current value(s); len>1 ⇒ unresolved variants
	History []Version `json:"history"` // all versions, oldest→newest (immutable log)
	Deleted bool      `json:"deleted"`
}

// Manager is the block engine over a db.Service. It keeps an incremental fold
// cache per scope so reads don't re-scan the whole log every time: on each
// read it detects new entries via the version vector and folds only the delta.
type Manager struct {
	db    *db.Service
	mu    sync.Mutex
	cache map[string]*scopeFold
}

func New(d *db.Service) *Manager { return &Manager{db: d, cache: map[string]*scopeFold{}} }

// acc accumulates one block's versions while folding a scope.
type acc struct {
	blockType  string
	versions   []Version
	parents    map[string]bool // entry IDs referenced as parents
	pos        string          // from the latest op carrying a pos (create/move)
	posLamport uint64
	parent     string // containing block, from the create op
}

// scopeFold is the cached, incrementally-maintained fold of one scope.
type scopeFold struct {
	vec    db.VersionVector // author→maxSeq we've folded (change detection)
	seen   map[string]bool  // folded entry IDs (dedupe; Lamport isn't unique)
	by     map[string]*acc  // bid → accumulator
	blocks []*Block         // last built output (rebuilt only when by changes)
}

// Create writes a new block into channel and returns its BID. parent/pos
// place it within a container/order ("" for both = root, unordered). The BID
// is namespaced by the creator (NewBID) so independent authors never collide
// without coordination (Yjs actorId-style); use Upsert for well-known/derived
// BIDs (general, DMs) that several peers must agree on.
func (m *Manager) Create(s Signer, channel, blockType string, content json.RawMessage, parent, pos string) (string, error) {
	bid := NewBID(s.PubHex())
	if err := m.commit(s, channel, blockType, op{BID: bid, Op: OpCreate, Parent: parent, Pos: pos, Content: content}); err != nil {
		return "", err
	}
	return bid, nil
}

// NewBID mints a creator-namespaced block id: "<fp16>-<randHex16>", where fp16
// is the first 16 hex of the author's pub. The fp prefix guarantees distinct
// authors occupy disjoint id-spaces (no coordination, no collisions); the
// random suffix makes each of an author's blocks unique.
func NewBID(pub string) string {
	fp := pub
	if len(fp) > 16 {
		fp = fp[:16]
	}
	return fp + "-" + randHex(16)
}

// Update writes a new version of bid. parents are the version(s) it builds
// on; if nil, the block's current heads are used (the usual case).
func (m *Manager) Update(s Signer, channel, bid string, content json.RawMessage, parents []string) error {
	bt, parents, err := m.resolve(channel, bid, parents)
	if err != nil {
		return err
	}
	return m.commit(s, channel, bt, op{BID: bid, Op: OpUpdate, Parents: parents, Content: content})
}

// UpdateType writes a new version of bid that also changes its block type
// (the fold takes the type from the latest version) — used for markdown
// retype shortcuts (e.g. "# " → heading, "[] " → todo).
func (m *Manager) UpdateType(s Signer, channel, bid, blockType string, content json.RawMessage) error {
	_, parents, err := m.resolve(channel, bid, nil)
	if err != nil {
		return err
	}
	return m.commit(s, channel, blockType, op{BID: bid, Op: OpUpdate, Parents: parents, Content: content})
}

// Delete tombstones bid (a new version with op=delete superseding heads).
func (m *Manager) Delete(s Signer, channel, bid string, parents []string) error {
	bt, parents, err := m.resolve(channel, bid, parents)
	if err != nil {
		return err
	}
	return m.commit(s, channel, bt, op{BID: bid, Op: OpDelete, Parents: parents})
}

// Move repositions bid (new version with op=move carrying pos).
func (m *Manager) Move(s Signer, channel, bid, pos string) error {
	bt, parents, err := m.resolve(channel, bid, nil)
	if err != nil {
		return err
	}
	return m.commit(s, channel, bt, op{BID: bid, Op: OpMove, Parents: parents, Pos: pos})
}

// Merge resolves variants: a new version with op=merge whose parents are
// all the current heads, carrying the chosen/combined content.
func (m *Manager) Merge(s Signer, channel, bid string, content json.RawMessage) error {
	b := m.Block(channel, bid)
	if b == nil {
		return fmt.Errorf("blocks: unknown block %s", bid)
	}
	parents := make([]string, 0, len(b.Heads))
	for _, h := range b.Heads {
		parents = append(parents, h.EntryID)
	}
	return m.commit(s, channel, b.Type, op{BID: bid, Op: OpMerge, Parents: parents, Content: content})
}

// resolve fills in the block's type and (if parents nil) its current heads.
func (m *Manager) resolve(channel, bid string, parents []string) (string, []string, error) {
	b := m.Block(channel, bid)
	if b == nil {
		return "", nil, fmt.Errorf("blocks: unknown block %s", bid)
	}
	if parents == nil {
		for _, h := range b.Heads {
			parents = append(parents, h.EntryID)
		}
	}
	return b.Type, parents, nil
}

func (m *Manager) commit(s Signer, channel, blockType string, o op) error {
	p, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = m.db.Commit(s, channel, typePrefix+blockType, p)
	return err
}

// Upsert writes content to a block with a caller-chosen stable bid:
// create if absent, else a new version over current heads. Handy for
// singletons (e.g. a conversation's one notes block).
func (m *Manager) Upsert(s Signer, channel, bid, blockType string, content json.RawMessage) error {
	b := m.Block(channel, bid)
	if b == nil {
		return m.commit(s, channel, blockType, op{BID: bid, Op: OpCreate, Content: content})
	}
	var parents []string
	for _, h := range b.Heads {
		parents = append(parents, h.EntryID)
	}
	return m.commit(s, channel, blockType, op{BID: bid, Op: OpUpdate, Parents: parents, Content: content})
}

// Block folds a single block in channel (nil if unknown).
func (m *Manager) Block(channel, bid string) *Block {
	for _, b := range m.Blocks(channel) {
		if b.BID == bid {
			return b
		}
	}
	return nil
}

// Blocks folds every block in channel from the log. Order: by latest head's
// Pos then BID. Tombstoned blocks are included with Deleted=true (callers
// filter as they wish). Incremental: only entries new since the last call are
// folded; an unchanged scope returns the cached slice untouched.
//
// The returned slice is shared with the cache — callers must not mutate it.
func (m *Manager) Blocks(channel string) []*Block {
	m.mu.Lock()
	defer m.mu.Unlock()
	cf := m.cache[channel]
	cur := m.db.Vector(channel)
	if cf == nil || regressed(cur, cf.vec) {
		// First read, or the log shrank/changed under us (e.g. camp switch) —
		// fold from scratch.
		cf = &scopeFold{vec: db.VersionVector{}, seen: map[string]bool{}, by: map[string]*acc{}}
		m.cache[channel] = cf
		foldInto(cf, m.db.Frames(channel))
		cf.blocks = buildBlocks(channel, cf.by)
		cf.vec = cur
		return cf.blocks
	}
	if vecEqual(cur, cf.vec) {
		return cf.blocks // nothing new
	}
	if foldInto(cf, m.db.Since(channel, cf.vec)) {
		cf.blocks = buildBlocks(channel, cf.by)
	}
	cf.vec = cur
	return cf.blocks
}

// foldInto accumulates entries into cf (skipping non-block types and already-
// seen IDs). Returns true if any new block entry was folded.
func foldInto(cf *scopeFold, frames []*db.Frame) bool {
	changed := false
	for _, e := range frames {
		if !strings.HasPrefix(e.Type, typePrefix) || cf.seen[e.ID] {
			continue
		}
		var o op
		if json.Unmarshal(e.Payload, &o) != nil || o.BID == "" {
			continue
		}
		cf.seen[e.ID] = true
		a := cf.by[o.BID]
		if a == nil {
			a = &acc{parents: map[string]bool{}}
			cf.by[o.BID] = a
		}
		a.blockType = strings.TrimPrefix(e.Type, typePrefix)
		a.versions = append(a.versions, Version{
			EntryID: e.ID, Author: e.Author, Lamport: e.Lamport, TS: e.TS,
			Op: o.Op, Content: o.Content,
		})
		for _, p := range o.Parents {
			a.parents[p] = true
		}
		if o.Op == OpCreate {
			a.parent = o.Parent
		}
		if o.Pos != "" && e.Lamport >= a.posLamport { // latest pos wins (create or move)
			a.pos, a.posLamport = o.Pos, e.Lamport
		}
		changed = true
	}
	return changed
}

// buildBlocks materializes the output blocks from accumulated state.
func buildBlocks(channel string, by map[string]*acc) []*Block {
	out := make([]*Block, 0, len(by))
	for bid, a := range by {
		// Heads = versions not superseded by any other version's parents.
		var heads []Version
		for _, v := range a.versions {
			if !a.parents[v.EntryID] {
				heads = append(heads, v)
			}
		}
		sort.Slice(heads, func(i, j int) bool {
			if heads[i].Lamport != heads[j].Lamport {
				return heads[i].Lamport < heads[j].Lamport
			}
			return heads[i].EntryID < heads[j].EntryID
		})
		// Full history oldest→newest (every version is kept — immutable log).
		history := append([]Version(nil), a.versions...)
		sort.Slice(history, func(i, j int) bool {
			if history[i].Lamport != history[j].Lamport {
				return history[i].Lamport < history[j].Lamport
			}
			return history[i].EntryID < history[j].EntryID
		})
		b := &Block{BID: bid, Type: a.blockType, Channel: channel, Heads: heads, History: history, Pos: a.pos, Parent: a.parent}
		if n := len(heads); n > 0 {
			b.Deleted = heads[n-1].Op == OpDelete // latest head
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pos != out[j].Pos {
			return out[i].Pos < out[j].Pos
		}
		return out[i].BID < out[j].BID
	})
	return out
}

// regressed reports whether cur lost ground vs old (an author's seq went
// backwards or vanished) — meaning the underlying store changed and the cache
// must be rebuilt from scratch.
func regressed(cur, old db.VersionVector) bool {
	for a, s := range old {
		if cur[a] < s {
			return true
		}
	}
	return false
}

func vecEqual(a, b db.VersionVector) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("blocks: rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}
