package messenger

import (
	"context"
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

// GeneralChannelID is the reserved id of the camp-wide "general" channel every
// peer implicitly belongs to. The "*" owner marks it ownerless: no single peer
// controls its roster (which is the live camp membership), and it can't be
// created, deleted or left — it simply always exists.
const GeneralChannelID = "*/general"

func isGeneral(id string) bool { return id == GeneralChannelID }

// maxPerConv bounds the in-memory ring per conversation. Without a DB wired
// in, there's no recall beyond this window.
const maxPerConv = 1000

// message Type values.
const (
	TypeText      = "text"
	TypeCreate    = "create"
	TypeAdd       = "add"
	TypeRemove    = "remove"
	TypeDelete    = "delete" // owner tore the channel down — recipients drop it
	TypeLeave     = "leave"  // a member left — the owner ratifies with a remove
	TypeCallStart = "call_start"
	TypeCallEnd   = "call_end"
)

// busAPI is the slice of bus.Service the messenger uses — an interface so
// tests can fake the transport.
type busAPI interface {
	Request(ctx context.Context, pub, typ string, payload []byte) ([]byte, error)
	Handle(typ string, fn bus.HandlerFunc)
	Label(pub string) string
}

// Service owns the in-memory message store and the bus transport, backed by
// a per-camp SQLite Store for durability.
type Service struct {
	bus   busAPI
	self  func() string   // our identity pub, from the engine
	store *Store          // durable backing; nil disables persistence
	camp  func() string   // current camp id, for the per-camp database
	peers func() []string // current camp peer pubs (excl. self), for general fanout

	mu       sync.Mutex
	channels map[string]*Channel  // id → materialised channel (fold of its messages)
	convs    map[string][]Message // conversation key → ring buffer
	seen     map[string]struct{}  // message IDs already accepted, for dedup

	subMu sync.Mutex
	subs  map[chan Message]struct{}
}

// NewService builds the messaging service. selfFn returns our identity pub;
// campFn the active camp id (for the per-camp database). store may be nil to
// run purely in memory. Both funcs are called per operation so a camp switch
// is picked up without re-wiring; call LoadCamp after a switch to hydrate.
func NewService(b *bus.Service, selfFn func() string, store *Store, campFn func() string, peersFn func() []string) *Service {
	return &Service{
		bus:      b,
		self:     selfFn,
		store:    store,
		camp:     campFn,
		peers:    peersFn,
		channels: map[string]*Channel{},
		convs:    map[string][]Message{},
		seen:     map[string]struct{}{},
		subs:     map[chan Message]struct{}{},
	}
}

// generalRoster is the live membership of the camp-wide general channel: every
// known camp peer plus ourselves. Computed on demand — general has no stored
// roster — so it tracks the camp as peers come and go.
func (s *Service) generalRoster() []string {
	var r []string
	if s.peers != nil {
		r = s.peers()
	}
	if self := s.self(); self != "" {
		r = append(r, self)
	}
	return dedupe(r)
}

// LoadCamp clears in-memory state and hydrates it from the durable store for
// the current camp. Call when a camp becomes active (engine OnStarted) so
// channels and history survive restarts.
func (s *Service) LoadCamp() {
	if s.store == nil {
		return
	}
	camp := s.camp()
	chans, _ := s.store.Channels(camp)
	msgs, _ := s.store.AllMessages(camp)
	selfPub := s.self()
	s.mu.Lock()
	s.channels = map[string]*Channel{}
	s.convs = map[string][]Message{}
	s.seen = map[string]struct{}{}
	for i := range chans {
		c := chans[i]
		s.channels[c.ID] = &c
	}
	// general always exists — materialise it if the store didn't carry it.
	if s.channels[GeneralChannelID] == nil {
		s.channels[GeneralChannelID] = &Channel{ID: GeneralChannelID, Name: "general", Owner: "*", CreatedAt: nowMs()}
	}
	for _, m := range msgs {
		s.seen[m.ID] = struct{}{}
		k := convKey(m, selfPub)
		s.convs[k] = append(s.convs[k], m)
	}
	s.mu.Unlock()
}

// Register wires the bus handler and starts the redelivery loop. Call once
// after constructing the bus.
func (s *Service) Register() {
	s.bus.Handle(typeMsg, s.onMsg)
	go s.flushLoop()
}

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
	// Anything inbound proves the peer is reachable — push them whatever
	// we still owe (cheap no-op when the outbox has nothing for them).
	go s.flush(fromPub)
	if m.ID == "" || !s.markSeen(m.ID) {
		return nil, nil // duplicate (retransmit / future relay) or id-less — drop
	}
	// Control events aren't conversation content — they mutate channel state
	// and are not stored as messages.
	if m.Kind == "channel" {
		owner, _ := SplitChannelID(m.Peer)
		switch m.Type {
		case TypeDelete:
			if m.From == owner { // only the owner can tear a channel down
				s.dropChannel(m.Peer)
				s.publish(m) // let UIs refresh the channel list
			}
			return nil, nil
		case TypeLeave:
			if selfPub == owner { // ratify a member's departure authoritatively
				_, _ = s.RemoveMembers(m.Peer, []string{m.From})
			}
			return nil, nil
		}
		s.reconcileChannel(m, selfPub)
	}
	s.append(convKey(m, selfPub), m)
	s.publish(m)
	return nil, nil
}

// dropChannel removes a channel and its conversation from memory and the
// store. Used by delete (owner) and leave (self).
func (s *Service) dropChannel(id string) {
	s.mu.Lock()
	delete(s.channels, id)
	delete(s.convs, id)
	s.mu.Unlock()
	if s.store != nil {
		if err := s.store.DeleteChannel(s.camp(), id); err != nil {
			clog.Warn("chat", "delete channel %q: %v", id, err)
		}
	}
}

// persistMsg writes one message to the durable store (best-effort).
func (s *Service) persistMsg(m Message) {
	if s.store == nil {
		return
	}
	if err := s.store.AddMessage(s.camp(), m); err != nil {
		clog.Debug("chat", "persist message: %v", err)
	}
}

// persistChannel writes a channel descriptor to the durable store.
func (s *Service) persistChannel(c *Channel) {
	if s.store == nil || c == nil {
		return
	}
	if err := s.store.UpsertChannel(s.camp(), *c); err != nil {
		clog.Debug("chat", "persist channel: %v", err)
	}
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
	existing := s.channels[m.Peer]
	if existing == nil {
		ch := &Channel{
			ID: m.Peer, Name: name, Owner: owner,
			Members: append([]string(nil), m.Members...), CreatedAt: m.TS,
		}
		s.channels[m.Peer] = ch
		s.mu.Unlock()
		s.persistChannel(ch)
		return
	}
	if m.From != owner { // only the owner may change the roster
		s.mu.Unlock()
		return
	}
	if !contains(m.Members, selfPub) {
		s.mu.Unlock()
		s.dropChannel(m.Peer) // owner removed us
		return
	}
	existing.Members = append([]string(nil), m.Members...)
	cp := *existing
	s.mu.Unlock()
	s.persistChannel(&cp)
}

// --- delivery ---
//
// Delivery is at-least-once with sender-side retry: every send is a bus
// Request, and the recipient's (deduped) handler reply is the ACK. A failed
// send lands in the outbox (SQLite — survives restarts) and is retried by
// flushLoop, plus immediately when the recipient shows signs of life
// (anything inbound from them). Only the AUTHOR retries its own messages —
// nobody relays anyone else's — which is what keeps the no-signature trust
// model sound.

// deliverTimeout bounds one delivery attempt. Generous enough for a cold
// AWG+QUIC handshake chain, short enough not to pile up goroutines.
const deliverTimeout = 10 * time.Second

// deliver sends wire to pub and returns whether it was ACKed. On failure
// the item is queued for redelivery.
func (s *Service) deliver(msgID, pub string, wire []byte) bool {
	ctx, cancel := context.WithTimeout(context.Background(), deliverTimeout)
	_, err := s.bus.Request(ctx, pub, typeMsg, wire)
	cancel()
	if err == nil {
		return true
	}
	clog.Debug("chat", "deliver %s to %s: %v (queued)", msgID, s.bus.Label(pub), err)
	if s.store != nil {
		if qerr := s.store.AddOutbox(s.camp(), OutboxItem{MsgID: msgID, Recipient: pub, Payload: wire, TS: nowMs()}); qerr != nil {
			clog.Warn("chat", "outbox enqueue: %v", qerr)
		}
	}
	return false
}

// outboxTTL is how long an undelivered message waits for its recipient.
const outboxTTL = 14 * 24 * time.Hour

// flushLoop retries the outbox forever (the service lives for the process).
func (s *Service) flushLoop() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		s.flush("")
	}
}

// flush retries undelivered items — all of them, or only those owed to one
// recipient (onlyPub != ""). Per recipient it stops at the first failure:
// if the oldest message doesn't get through, the peer is still unreachable
// and hammering the rest just burns timeouts.
func (s *Service) flush(onlyPub string) {
	if s.store == nil {
		return
	}
	camp := s.camp()
	_ = s.store.PruneOutbox(camp, nowMs()-outboxTTL.Milliseconds())
	items, err := s.store.Outbox(camp)
	if err != nil || len(items) == 0 {
		return
	}
	dead := map[string]bool{}
	for _, it := range items {
		if onlyPub != "" && it.Recipient != onlyPub {
			continue
		}
		if dead[it.Recipient] {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), deliverTimeout)
		_, err := s.bus.Request(ctx, it.Recipient, typeMsg, it.Payload)
		cancel()
		if err != nil {
			dead[it.Recipient] = true
			continue
		}
		clog.Debug("chat", "redelivered %s to %s", it.MsgID, s.bus.Label(it.Recipient))
		_ = s.store.DeleteOutbox(camp, it.MsgID, it.Recipient)
	}
}

// --- outbound ---

// SendDM delivers a direct message to peerPub and records our own copy. file
// may be nil; when set it rides inline as an attachment.
func (s *Service) SendDM(peerPub, body string, file *Attachment) (Message, error) {
	selfPub := s.self()
	if selfPub == "" {
		return Message{}, fmt.Errorf("chat: no identity")
	}
	m := Message{
		ID: newID(), Kind: "dm", Peer: peerPub, Type: TypeText,
		From: selfPub, To: peerPub, Body: body, File: file, TS: nowMs(),
	}
	local := m
	local.Mine = true
	s.append(peerPub, local)
	s.publish(local)
	go s.deliver(m.ID, peerPub, mustJSON(m))
	return local, nil
}

// Post sends a text message to a channel we belong to: stored locally and
// fanned out to every member, carrying the current roster snapshot. file may
// be nil; when set it rides inline as an attachment.
func (s *Service) Post(chanID, body string, file *Attachment) (Message, error) {
	return s.emit(chanID, TypeText, body, nil, file)
}

// SendEvent posts a contentful-less system event (call started/ended, …)
// into a conversation. It travels, persists and dedups exactly like a text
// message — events ARE messages — and renders as a system line.
func (s *Service) SendEvent(kind, key, typ string) (Message, error) {
	if kind == "channel" {
		return s.emit(key, typ, "", nil, nil)
	}
	selfPub := s.self()
	if selfPub == "" {
		return Message{}, fmt.Errorf("chat: no identity")
	}
	m := Message{
		ID: newID(), Kind: "dm", Peer: key, Type: typ,
		From: selfPub, To: key, TS: nowMs(),
	} // events carry no attachment
	local := m
	local.Mine = true
	s.append(key, local)
	s.publish(local)
	go s.deliver(m.ID, key, mustJSON(m))
	return local, nil
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
	ch := &Channel{ID: id, Name: name, Owner: selfPub, Members: roster, CreatedAt: nowMs()}
	s.mu.Lock()
	s.channels[id] = ch
	s.mu.Unlock()
	s.persistChannel(ch)
	if _, err := s.emit(id, TypeCreate, "", roster, nil); err != nil {
		return Channel{}, err
	}
	return *s.channelCopy(id), nil
}

// DeleteChannel tears a channel down (owner only): tells the members, then
// drops it locally and from the store.
func (s *Service) DeleteChannel(chanID string) error {
	if isGeneral(chanID) {
		return fmt.Errorf("chat: the general channel can't be deleted")
	}
	selfPub := s.self()
	s.mu.Lock()
	ch := s.channels[chanID]
	if ch == nil {
		s.mu.Unlock()
		return fmt.Errorf("chat: unknown channel %q", chanID)
	}
	if ch.Owner != selfPub {
		s.mu.Unlock()
		return fmt.Errorf("chat: only the owner can delete the channel")
	}
	members := append([]string(nil), ch.Members...)
	s.mu.Unlock()
	s.sendControl(chanID, TypeDelete, members)
	s.dropChannel(chanID)
	s.publish(Message{Kind: "channel", Peer: chanID, Type: TypeDelete, From: selfPub, TS: nowMs(), Mine: true})
	return nil
}

// LeaveChannel removes us from a channel: drops it locally and asks the owner
// to ratify the departure (which propagates an authoritative remove). The
// owner can't leave their own channel — they delete it instead.
func (s *Service) LeaveChannel(chanID string) error {
	if isGeneral(chanID) {
		return fmt.Errorf("chat: you can't leave the general channel")
	}
	selfPub := s.self()
	owner, _ := SplitChannelID(chanID)
	if owner == selfPub {
		return fmt.Errorf("chat: the owner deletes the channel, not leaves it")
	}
	s.mu.Lock()
	_, ok := s.channels[chanID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("chat: unknown channel %q", chanID)
	}
	s.sendControl(chanID, TypeLeave, []string{owner})
	s.dropChannel(chanID)
	s.publish(Message{Kind: "channel", Peer: chanID, Type: TypeLeave, From: selfPub, TS: nowMs(), Mine: true})
	return nil
}

// sendControl fires a contentless channel event (delete/leave) to the given
// peers. Not stored locally — it mutates state, it isn't conversation.
func (s *Service) sendControl(chanID, typ string, fanout []string) {
	selfPub := s.self()
	m := Message{ID: newID(), Kind: "channel", Peer: chanID, Type: typ, From: selfPub, TS: nowMs()}
	wire := mustJSON(m)
	for _, pub := range fanout {
		if pub == selfPub || pub == "" {
			continue
		}
		go s.deliver(m.ID, pub, wire)
	}
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
	cp := *ch
	s.mu.Unlock()
	s.persistChannel(&cp)

	typ, affected := TypeAdd, add
	if len(remove) > 0 {
		typ, affected = TypeRemove, remove
	}
	// Fan the event to the union of old and new members so removed peers
	// hear it too (they're no longer in the post-change roster). affected
	// names who was added/removed, for the human-readable system line.
	if _, err := s.emitTo(chanID, typ, "", roster, affected, union(old, roster), nil); err != nil {
		return Channel{}, err
	}
	return *s.channelCopy(chanID), nil
}

// emit stores a channel message locally and fans it to the channel's
// current members. roster overrides the carried snapshot when non-nil
// (used by create/add/remove); otherwise the channel's current roster is
// used.
func (s *Service) emit(chanID, typ, body string, roster []string, file *Attachment) (Message, error) {
	// general has no stored channel/roster — fan out to the whole camp.
	if isGeneral(chanID) {
		peers := s.generalRoster()
		return s.emitTo(chanID, typ, body, peers, nil, peers, file)
	}
	s.mu.Lock()
	ch := s.channels[chanID]
	s.mu.Unlock()
	if ch == nil {
		return Message{}, fmt.Errorf("chat: unknown channel %q", chanID)
	}
	if roster == nil {
		roster = ch.Members
	}
	return s.emitTo(chanID, typ, body, roster, nil, roster, file)
}

// emitTo stores a channel message and fans it out. roster is the carried
// member snapshot; affected names who a membership event acted on (nil for
// a text post); fanout is the explicit recipient set (may differ from the
// roster, e.g. a removal that must also reach removed peers); file is an
// optional inline attachment (nil for control events).
func (s *Service) emitTo(chanID, typ, body string, roster, affected, fanout []string, file *Attachment) (Message, error) {
	selfPub := s.self()
	m := Message{
		ID: newID(), Kind: "channel", Peer: chanID, Type: typ,
		From: selfPub, Body: body, File: file,
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
		go s.deliver(m.ID, pub, wire)
	}
	return local, nil
}

// --- reads ---

// Channels returns the channels we currently belong to.
func (s *Service) Channels() []Channel {
	s.mu.Lock()
	out := make([]Channel, 0, len(s.channels)+1)
	hasGeneral := false
	for _, ch := range s.channels {
		if isGeneral(ch.ID) {
			hasGeneral = true
		}
		out = append(out, *ch)
	}
	s.mu.Unlock()
	if !hasGeneral { // surface general even before LoadCamp ran
		out = append(out, Channel{ID: GeneralChannelID, Name: "general", Owner: "*"})
	}
	// general's roster is the live camp membership, not a stored snapshot.
	roster := s.generalRoster()
	for i := range out {
		if isGeneral(out[i].ID) {
			out[i].Members = roster
		}
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
	s.persistMsg(m)
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
