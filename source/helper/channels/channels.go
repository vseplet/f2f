// Package channels models a camp's channels as blocks. A channel is just a
// block in the camp-wide "channels" scope: its content carries the name and
// member list, its block Parent gives nesting, its creator is the owner.
// Membership and metadata thus replicate and converge through the same db log
// as everything else — one substrate, one source of truth.
//
// Resources of a channel live in their own scopes keyed by the channel
// (e.g. "note:<channel>"); this package only describes the channel itself.
package channels

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/vseplet/f2f/source/helper/blocks"
)

// Scope is the camp-wide registry every channel block lives in.
const Scope = "channels"

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
	Owner   string   `json:"owner"`   // creator pub
	Members []string `json:"members"` // explicit members (excludes the owner)
}

// Manager is the channel registry over the block engine.
type Manager struct{ blocks *blocks.Manager }

func New(b *blocks.Manager) *Manager { return &Manager{blocks: b} }

// Create makes a new channel owned by s. parent is the containing channel's
// bid ("" = top level); pos orders it among siblings ("" = unordered).
func (m *Manager) Create(s blocks.Signer, name, parent, pos string) (string, error) {
	c, err := json.Marshal(meta{Name: name})
	if err != nil {
		return "", err
	}
	return m.blocks.Create(s, Scope, blockType, c, parent, pos)
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
	return GeneralBID, m.blocks.Upsert(s, Scope, GeneralBID, blockType, c)
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
	return bid, m.blocks.Upsert(s, Scope, bid, blockType, c)
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
	return m.blocks.Delete(s, Scope, bid, nil)
}

// Get folds a single channel (nil if unknown/deleted).
func (m *Manager) Get(bid string) *Channel {
	b := m.blocks.Block(Scope, bid)
	if b == nil || b.Deleted {
		return nil
	}
	return toChannel(b)
}

// List folds every live channel in the camp.
func (m *Manager) List() []*Channel {
	var out []*Channel
	for _, b := range m.blocks.Blocks(Scope) {
		if b.Deleted {
			continue
		}
		out = append(out, toChannel(b))
	}
	return out
}

// IsMember reports whether pub may access the channel (owner or listed member).
func (m *Manager) IsMember(bid, pub string) bool {
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
	return m.blocks.Update(s, Scope, bid, c, nil)
}

// toChannel folds a block into a Channel: name/members from the latest head,
// owner from the first version, nesting/order from the block.
func toChannel(b *blocks.Block) *Channel {
	var md meta
	if n := len(b.Heads); n > 0 {
		_ = json.Unmarshal(b.Heads[n-1].Content, &md)
	}
	owner := ""
	if len(b.History) > 0 {
		owner = b.History[0].Author
	}
	return &Channel{
		BID: b.BID, Name: md.Name, Members: md.Members,
		Parent: b.Parent, Pos: b.Pos, Owner: owner,
	}
}

func blockMissing(bid string) error {
	return &missingError{bid}
}

type missingError struct{ bid string }

func (e *missingError) Error() string { return "channels: unknown channel " + e.bid }
