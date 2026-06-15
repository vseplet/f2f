// Package channels models a camp's channels as blocks. Each channel is a single
// block in its OWN scope "channel:<bid>": content carries the name + member
// list, creator is the owner. Per-channel scopes (not one shared registry) let
// the sync layer gate replication by membership — a non-member never receives
// the channel's metadata scope at all.
//
// Resources of a channel live in their own scopes keyed by the channel bid
// ("note:<bid>", "message:<bid>"); this package only describes the channel.
package channels

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vseplet/f2f/source/helper/db/blocks"
)

// ScopePrefix namespaces each channel's own metadata scope ("channel:<bid>").
const ScopePrefix = "channel:"

// metaScope is the scope holding one channel's block.
func metaScope(bid string) string { return ScopePrefix + bid }

// blockType marks channel blocks within the scope (Type = "block.channel").
const blockType = "channel"

// GeneralBID is the well-known id of the camp-wide channel everyone is in.
// Every peer uses the same literal so they converge on one block.
const GeneralBID = "general"

// DMBID derives the deterministic id of the direct-message channel between two
// pubs, so both sides mint the same channel without coordination.
func DMBID(a, b string) string {
	if a > b {
		a, b = b, a
	}
	sum := sha256.Sum256([]byte(a + "\x00" + b))
	return "dm-" + hex.EncodeToString(sum[:16])
}

// meta is the channel block's content: owner is derived from the creator, so
// only the mutable bits are stored.
type meta struct {
	Name    string   `json:"name"`
	Members []string `json:"members,omitempty"` // member pubs (owner is implicit)
}

// Channel is the folded current state of one channel block.
type Channel struct {
	BID     string   `json:"bid"`
	Name    string   `json:"name"`
	Parent  string   `json:"parent,omitempty"` // parent channel bid ("" = root)
	Pos     string   `json:"pos,omitempty"`
	Owner     string   `json:"owner"`      // creator pub
	Members   []string `json:"members"`    // explicit members (excludes the owner)
	CreatedAt int64    `json:"created_at"` // first version's timestamp
}

// Manager is the channel registry over the block engine.
type Manager struct{ blocks *blocks.Manager }

func New(b *blocks.Manager) *Manager { return &Manager{blocks: b} }

// Create makes a new channel owned by s. parent is the containing channel's
// bid ("" = top level); pos orders it among siblings ("" = unordered).
func (m *Manager) Create(s blocks.Signer, name, parent, pos string) (string, error) {
	// general is the well-known camp-wide channel — you can't recreate it or
	// nest channels under it (its name is also its bid; "general/…" names would
	// build a bogus sidebar tree next to the real # general).
	if name == GeneralBID || strings.HasPrefix(name, GeneralBID+"/") {
		return "", fmt.Errorf("channels: %q is reserved", GeneralBID)
	}
	// Mint the bid first so the channel gets its own scope channel:<bid>.
	bid := blocks.NewBID(s.PubHex())
	c, err := json.Marshal(meta{Name: name})
	if err != nil {
		return "", err
	}
	return bid, m.blocks.Upsert(s, metaScope(bid), bid, blockType, c)
}

// EnsureGeneral creates the well-known general channel if it doesn't exist yet
// (idempotent; a create race just yields harmless variant heads). Returns its
// bid (the constant GeneralBID).
func (m *Manager) EnsureGeneral(s blocks.Signer) (string, error) {
	if m.Get(GeneralBID) != nil {
		return GeneralBID, nil
	}
	c, err := json.Marshal(meta{Name: "general"})
	if err != nil {
		return "", err
	}
	return GeneralBID, m.blocks.Upsert(s, metaScope(GeneralBID), GeneralBID, blockType, c)
}

// EnsureDM creates the direct-message channel between a and b if absent, with
// both as members. Returns its deterministic bid (DMBID).
func (m *Manager) EnsureDM(s blocks.Signer, a, b string) (string, error) {
	bid := DMBID(a, b)
	if m.Get(bid) != nil {
		return bid, nil
	}
	c, err := json.Marshal(meta{Members: []string{a, b}})
	if err != nil {
		return "", err
	}
	return bid, m.blocks.Upsert(s, metaScope(bid), bid, blockType, c)
}

// Rename changes the channel's name (a new version authored by s).
func (m *Manager) Rename(s blocks.Signer, bid, name string) error {
	c := m.Get(bid)
	if c == nil {
		return blockMissing(bid)
	}
	return m.write(s, bid, name, c.Members)
}

// AddMember adds pub to the channel (no-op if already a member or the owner).
func (m *Manager) AddMember(s blocks.Signer, bid, pub string) error {
	c := m.Get(bid)
	if c == nil {
		return blockMissing(bid)
	}
	if pub == c.Owner {
		return nil
	}
	for _, p := range c.Members {
		if p == pub {
			return nil
		}
	}
	return m.write(s, bid, c.Name, append(append([]string(nil), c.Members...), pub))
}

// RemoveMember drops pub from the channel (no-op if absent).
func (m *Manager) RemoveMember(s blocks.Signer, bid, pub string) error {
	c := m.Get(bid)
	if c == nil {
		return blockMissing(bid)
	}
	out := c.Members[:0:0]
	for _, p := range c.Members {
		if p != pub {
			out = append(out, p)
		}
	}
	return m.write(s, bid, c.Name, out)
}

// Delete tombstones the channel block.
func (m *Manager) Delete(s blocks.Signer, bid string) error {
	return m.blocks.Delete(s, metaScope(bid), bid, nil)
}

// Get folds a single channel (nil if unknown/deleted).
func (m *Manager) Get(bid string) *Channel {
	b := m.blocks.Block(metaScope(bid), bid)
	if b == nil || b.Deleted {
		return nil
	}
	return toChannel(b)
}

// List folds every live channel — one per "channel:<bid>" scope.
func (m *Manager) List() []*Channel {
	var out []*Channel
	for _, sc := range m.blocks.Scopes() {
		if !strings.HasPrefix(sc, ScopePrefix) {
			continue
		}
		for _, b := range m.blocks.Blocks(sc) {
			if !b.Deleted {
				out = append(out, toChannel(b))
			}
		}
	}
	return out
}

// IsMember reports whether pub may access the channel (owner or listed member).
func (m *Manager) IsMember(bid, pub string) bool {
	if bid == GeneralBID {
		return true // everyone in the camp is in general
	}
	c := m.Get(bid)
	if c == nil {
		return false
	}
	if c.Owner == pub {
		return true
	}
	for _, p := range c.Members {
		if p == pub {
			return true
		}
	}
	return false
}

// write replaces the channel's content with a fresh version (name + members).
func (m *Manager) write(s blocks.Signer, bid, name string, members []string) error {
	c, err := json.Marshal(meta{Name: name, Members: members})
	if err != nil {
		return err
	}
	return m.blocks.Update(s, metaScope(bid), bid, c, nil)
}

// toChannel folds a block into a Channel: name/members from the latest head,
// owner from the first version, nesting/order from the block.
func toChannel(b *blocks.Block) *Channel {
	var md meta
	if n := len(b.Heads); n > 0 {
		_ = json.Unmarshal(b.Heads[n-1].Content, &md)
	}
	owner, created := "", int64(0)
	if len(b.History) > 0 {
		owner, created = b.History[0].Author, b.History[0].TS
	}
	return &Channel{
		BID: b.BID, Name: md.Name, Members: md.Members,
		Parent: b.Parent, Pos: b.Pos, Owner: owner, CreatedAt: created,
	}
}

func blockMissing(bid string) error {
	return &missingError{bid}
}

type missingError struct{ bid string }

func (e *missingError) Error() string { return "channels: unknown channel " + e.bid }
