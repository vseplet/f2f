package messenger

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vseplet/f2f/source/helper/clog"
	"github.com/vseplet/f2f/source/helper/mesh/bus"
)

// typeMsg is the single bus message type. Text, channel creation, and
// membership changes are all messages — distinguished by Message.Type — so
// there's nothing else to send: a channel is the fold of its messages.
const typeMsg = "chat.msg"

// maxPerConv bounds the in-memory ring per conversation. Without a DB wired
// in, there's no recall beyond this window.
const maxPerConv = 1000

// message Type values.
const (
	TypeText   = "text"
	TypeCreate = "create"
	TypeAdd    = "add"
	TypeRemove = "remove"
)

// Service owns the in-memory message store and the bus transport. It holds
// no DB (the Store is dormant); messages live only for the process lifetime.
type Service struct {
	bus  *bus.Service
	self func() string // our identity pub, from the engine

	mu       sync.Mutex
	channels map[string]*Channel  // id → materialised channel (fold of its messages)
	convs    map[string][]Message // conversation key → ring buffer
	seen     map[string]struct{}  // message IDs already accepted, for dedup

	subMu sync.Mutex
	subs  map[chan Message]struct{}
}

// NewService builds the messaging service. selfFn returns our identity pub
// and display name (engine IdentityPub + CampName); it's called per
// operation so a camp switch is picked up without re-wiring.
func NewService(b *bus.Service, selfFn func() string) *Service {
	return &Service{
		bus:      b,
		self:     selfFn,
		channels: map[string]*Channel{},
		convs:    map[string][]Message{},
		seen:     map[string]struct{}{},
		subs:     map[chan Message]struct{}{},
	}
}

// Register wires the bus handler. Call once after constructing the bus.
func (s *Service) Register() { s.bus.Handle(typeMsg, s.onMsg) }

// SplitChannelID splits a channel ID ("<owner_pub>/<name>") into its owner
// pub and name. Returns ("","") if it isn't a channel ID.
func SplitChannelID(id string) (owner, name string) {
	if i := strings.IndexByte(id, '/'); i > 0 {
		return id[:i], id[i+1:]
	}
	return "", ""
}

// --- inbound ---

func (s *Service) onMsg(fromPub string, payload []byte) ([]byte, error) {
	var m Message
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, fmt.Errorf("chat: bad msg: %w", err)
	}
	selfPub := s.self()
	m.From = fromPub // trust the bus-attested sender over the claimed field
	m.Mine = false
	if m.ID == "" || !s.markSeen(m.ID) {
		return nil, nil // duplicate (retransmit / future relay) or id-less — drop
	}
	if m.Kind == "channel" {
		s.reconcileChannel(m, selfPub)
	}
	s.append(convKey(m, selfPub), m)
	s.publish(m)
	return nil, nil
}

// markSeen records a message ID and reports whether it was new (false = a
// duplicate we've already accepted). The set is bounded — when it grows
// large it's cleared, which at worst lets a very old duplicate through once.
func (s *Service) markSeen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.seen[id]; dup {
		return false
	}
	if len(s.seen) > 16384 {
		s.seen = map[string]struct{}{}
	}
	s.seen[id] = struct{}{}
	return true
}

// reconcileChannel materialises/updates a channel from a channel message.
// First sighting creates the room (trust-on-first-use) so it appears; the
// roster is only refreshed from the owner's own messages — and if the
// owner's roster no longer lists us, we were removed: drop the channel.
func (s *Service) reconcileChannel(m Message, selfPub string) {
	owner, name := SplitChannelID(m.Peer)
	if owner == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing := s.channels[m.Peer]
	if existing == nil {
		s.channels[m.Peer] = &Channel{
			ID: m.Peer, Name: name, Owner: owner,
			Members: append([]string(nil), m.Members...), CreatedAt: m.TS,
		}
		return
	}
	if m.From != owner { // only the owner may change the roster
		return
	}
	if !contains(m.Members, selfPub) {
		delete(s.channels, m.Peer) // owner removed us
		return
	}
	existing.Members = append([]string(nil), m.Members...)
}

// --- outbound ---

// SendDM delivers a direct message to peerPub and records our own copy.
func (s *Service) SendDM(peerPub, body string) (Message, error) {
	selfPub := s.self()
	if selfPub == "" {
		return Message{}, fmt.Errorf("chat: no identity")
	}
	m := Message{
		ID: newID(), Kind: "dm", Peer: peerPub, Type: TypeText,
		From: selfPub, To: peerPub, Body: body, TS: nowMs(),
	}
	local := m
	local.Mine = true
	s.append(peerPub, local)
	s.publish(local)
	if err := s.bus.Notify(peerPub, typeMsg, mustJSON(m)); err != nil {
		clog.Warn("chat", "dm to %s: %v", s.bus.Label(peerPub), err)
	}
	return local, nil
}

// Post sends a text message to a channel we belong to: stored locally and
// fanned out to every member, carrying the current roster snapshot.
func (s *Service) Post(chanID, body string) (Message, error) {
	return s.emit(chanID, TypeText, body, nil)
}

// CreateChannel makes a named channel owned by us with the given members
// (we're always included) and announces it to them via a create message.
func (s *Service) CreateChannel(name string, members []string) (Channel, error) {
	selfPub := s.self()
	if selfPub == "" {
		return Channel{}, fmt.Errorf("chat: no identity")
	}
	if name == "" || strings.ContainsAny(name, "/ ") {
		return Channel{}, fmt.Errorf("chat: bad channel name %q", name)
	}
	id := selfPub + "/" + name
	roster := dedupe(append(members, selfPub))
	s.mu.Lock()
	s.channels[id] = &Channel{ID: id, Name: name, Owner: selfPub, Members: roster, CreatedAt: nowMs()}
	s.mu.Unlock()
	if _, err := s.emit(id, TypeCreate, "", roster); err != nil {
		return Channel{}, err
	}
	return *s.channelCopy(id), nil
}

// AddMembers adds members to a channel (owner only) and fans the new roster
// to everyone including the newcomers.
func (s *Service) AddMembers(chanID string, add []string) (Channel, error) {
	return s.changeMembers(chanID, add, nil)
}

// RemoveMembers removes members (owner only). The remove message is fanned
// to the union of old and new members, so the removed peers also learn.
func (s *Service) RemoveMembers(chanID string, remove []string) (Channel, error) {
	return s.changeMembers(chanID, nil, remove)
}

func (s *Service) changeMembers(chanID string, add, remove []string) (Channel, error) {
	selfPub := s.self()
	s.mu.Lock()
	ch := s.channels[chanID]
	if ch == nil {
		s.mu.Unlock()
		return Channel{}, fmt.Errorf("chat: unknown channel %q", chanID)
	}
	if ch.Owner != selfPub {
		s.mu.Unlock()
		return Channel{}, fmt.Errorf("chat: only the owner can manage members")
	}
	old := append([]string(nil), ch.Members...)
	roster := dedupe(append(ch.Members, add...))
	if len(remove) > 0 {
		rm := map[string]bool{}
		for _, p := range remove {
			rm[p] = true
		}
		rm[selfPub] = false // owner can't remove self
		kept := roster[:0]
		for _, p := range roster {
			if !rm[p] {
				kept = append(kept, p)
			}
		}
		roster = kept
	}
	ch.Members = roster
	s.mu.Unlock()

	typ, affected := TypeAdd, add
	if len(remove) > 0 {
		typ, affected = TypeRemove, remove
	}
	// Fan the event to the union of old and new members so removed peers
	// hear it too (they're no longer in the post-change roster). affected
	// names who was added/removed, for the human-readable system line.
	if _, err := s.emitTo(chanID, typ, "", roster, affected, union(old, roster)); err != nil {
		return Channel{}, err
	}
	return *s.channelCopy(chanID), nil
}

// emit stores a channel message locally and fans it to the channel's
// current members. roster overrides the carried snapshot when non-nil
// (used by create/add/remove); otherwise the channel's current roster is
// used.
func (s *Service) emit(chanID, typ, body string, roster []string) (Message, error) {
	s.mu.Lock()
	ch := s.channels[chanID]
	s.mu.Unlock()
	if ch == nil {
		return Message{}, fmt.Errorf("chat: unknown channel %q", chanID)
	}
	if roster == nil {
		roster = ch.Members
	}
	return s.emitTo(chanID, typ, body, roster, nil, roster)
}

// emitTo stores a channel message and fans it out. roster is the carried
// member snapshot; affected names who a membership event acted on (nil for
// a text post); fanout is the explicit recipient set (may differ from the
// roster, e.g. a removal that must also reach removed peers).
func (s *Service) emitTo(chanID, typ, body string, roster, affected, fanout []string) (Message, error) {
	selfPub := s.self()
	m := Message{
		ID: newID(), Kind: "channel", Peer: chanID, Type: typ,
		From: selfPub, Body: body,
		Members: append([]string(nil), roster...),
		Targets: append([]string(nil), affected...),
		TS:      nowMs(),
	}
	local := m
	local.Mine = true
	s.append(chanID, local)
	s.publish(local)
	wire := mustJSON(m)
	for _, pub := range fanout {
		if pub == selfPub || pub == "" {
			continue
		}
		if err := s.bus.Notify(pub, typeMsg, wire); err != nil {
			clog.Debug("chat", "post to %s: %v", s.bus.Label(pub), err)
		}
	}
	return local, nil
}

// --- reads ---

// Channels returns the channels we currently belong to.
func (s *Service) Channels() []Channel {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Channel, 0, len(s.channels))
	for _, ch := range s.channels {
		out = append(out, *ch)
	}
	return out
}

// Messages returns up to limit recent messages for a conversation. kind is
// "dm" (key = peer pub) or "channel" (key = channel ID).
func (s *Service) Messages(kind, key string, limit int) []Message {
	selfPub := s.self()
	s.mu.Lock()
	src := s.convs[key]
	if limit > 0 && len(src) > limit {
		src = src[len(src)-limit:]
	}
	out := make([]Message, len(src))
	copy(out, src)
	s.mu.Unlock()
	for i := range out {
		out[i].Mine = out[i].From == selfPub
	}
	return out
}

// --- live stream ---

// Subscribe returns a channel of new messages plus an unsubscribe func.
// Slow subscribers drop rather than block delivery.
func (s *Service) Subscribe(buf int) (<-chan Message, func()) {
	ch := make(chan Message, buf)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	return ch, func() {
		s.subMu.Lock()
		delete(s.subs, ch)
		s.subMu.Unlock()
		close(ch)
	}
}

func (s *Service) publish(m Message) {
	s.subMu.Lock()
	for ch := range s.subs {
		select {
		case ch <- m:
		default:
		}
	}
	s.subMu.Unlock()
}

// --- helpers ---

func (s *Service) channelCopy(id string) *Channel {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch := s.channels[id]; ch != nil {
		cp := *ch
		cp.Members = append([]string(nil), ch.Members...)
		return &cp
	}
	return &Channel{ID: id}
}

// convKey is the storage key for a message: the channel ID for a channel
// message, or — for a DM — the OTHER party's pub (each side keys the
// conversation by whom it's with).
func convKey(m Message, selfPub string) string {
	if m.Kind == "channel" {
		return m.Peer
	}
	if m.From == selfPub {
		return m.To
	}
	return m.From
}

func (s *Service) append(key string, m Message) {
	s.mu.Lock()
	c := s.convs[key]
	// Keep the ring ordered by (TS, ID): TS is the sender's send time, ID a
	// UUID for a deterministic tie-break — so every node converges on the
	// SAME order even when messages arrive out of order. Scan from the end
	// since traffic is usually newest-last (near-O(1) in the common case).
	// NOTE: TS is a wall clock, so heavy peer clock skew can still misorder;
	// good enough for chat, revisit with logical clocks if it bites.
	i := len(c)
	for i > 0 && less(m, c[i-1]) {
		i--
	}
	c = append(c, Message{})
	copy(c[i+1:], c[i:])
	c[i] = m
	if len(c) > maxPerConv {
		c = c[len(c)-maxPerConv:]
	}
	s.convs[key] = c
	s.mu.Unlock()
}

// less is the total order over messages: by send timestamp, then by ID.
func less(a, b Message) bool {
	if a.TS != b.TS {
		return a.TS < b.TS
	}
	return a.ID < b.ID
}

// newID returns a fresh random 128-bit message ID (hex). Globally unique
// with no coordinator and survives restarts — the basis for dedup.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is fatal-ish; fall back to a time-based id so
		// we never emit an empty (un-dedupable) ID.
		return "t" + strconv.FormatInt(nowMs(), 10)
	}
	return hex.EncodeToString(b[:])
}

func nowMs() int64 { return time.Now().UnixMilli() }

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func union(a, b []string) []string { return dedupe(append(append([]string(nil), a...), b...)) }

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
