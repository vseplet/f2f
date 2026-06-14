package messenger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vseplet/f2f/source/helper/mesh/bus"
)

// fakeBus simulates the transport: deliveries fail while down, succeed (and
// are counted) while up.
type fakeBus struct {
	down atomic.Bool
	sent atomic.Int64
}

func (f *fakeBus) Request(_ context.Context, _, _ string, _ []byte) ([]byte, error) {
	if f.down.Load() {
		return nil, fmt.Errorf("peer unreachable")
	}
	f.sent.Add(1)
	return nil, nil
}
func (f *fakeBus) Handle(string, bus.HandlerFunc) {}
func (f *fakeBus) Label(pub string) string        { return pub }

// newTestService builds a Service without a bus (onMsg/append/Messages
// never touch it) for unit-testing the in-memory ordering and dedup logic.
// store/camp are nil so persistence is a no-op unless overridden.
func newTestService() *Service {
	return &Service{
		self:     func() string { return "self" },
		channels: map[string]*Channel{},
		convs:    map[string][]Message{},
		notes:    map[string]NoteDoc{},
		seen:     map[string]struct{}{},
		subs:     map[chan Message]struct{}{},
	}
}

// userChannels returns the user-created channels, excluding the always-present
// camp-wide general channel — so tests can assert on the channels they made.
func userChannels(s *Service) []Channel {
	var out []Channel
	for _, c := range s.Channels() {
		if c.ID != GeneralChannelID {
			out = append(out, c)
		}
	}
	return out
}

func TestRedelivery(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(func(id string) string {
		d := filepath.Join(dir, id)
		_ = os.MkdirAll(d, 0o755)
		return d
	})
	defer st.Close()
	fb := &fakeBus{}
	s := newTestService()
	s.bus = fb
	s.store = st
	s.camp = func() string { return "c1" }

	// Peer down: the DM fails and must land in the outbox.
	fb.down.Store(true)
	if !s.deliver("m1", "bob", []byte(`{"id":"m1"}`)) {
		// expected — verify it's queued
	}
	items, _ := st.Outbox("c1")
	if len(items) != 1 || items[0].MsgID != "m1" || items[0].Recipient != "bob" {
		t.Fatalf("outbox after failed delivery = %+v", items)
	}

	// Peer back: flush drains the outbox and the wire was actually sent.
	fb.down.Store(false)
	s.flush("bob")
	if items, _ = st.Outbox("c1"); len(items) != 0 {
		t.Fatalf("outbox after flush = %+v, want empty", items)
	}
	if fb.sent.Load() != 1 {
		t.Fatalf("sent = %d, want 1", fb.sent.Load())
	}
}

// TestRedeliveryIntegration exercises the REAL production path: SendDM while
// the peer is down must enqueue (async), survive a "restart" (fresh service
// hydrating the same store), and flush once the peer is back.
func TestRedeliveryIntegration(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(func(id string) string {
		d := filepath.Join(dir, id)
		_ = os.MkdirAll(d, 0o755)
		return d
	})
	defer st.Close()
	fb := &fakeBus{}
	fb.down.Store(true) // bob is unreachable

	s := newTestService()
	s.bus, s.store, s.camp = fb, st, func() string { return "c1" }

	// Real send path — deliver runs in a goroutine and enqueues on failure.
	if _, err := s.SendDM("bob", "hi", nil, "", "", ""); err != nil {
		t.Fatalf("SendDM: %v", err)
	}
	// Wait for the async deliver to fail and queue the item.
	if !waitFor(func() bool { it, _ := st.Outbox("c1"); return len(it) == 1 }) {
		it, _ := st.Outbox("c1")
		t.Fatalf("message not queued after send to down peer: %+v", it)
	}

	// "Restart": a fresh service on the same store. The outbox must survive and
	// be retried by this new instance's flush.
	s2 := newTestService()
	s2.bus, s2.store, s2.camp = fb, st, func() string { return "c1" }
	s2.LoadCamp()
	fb.down.Store(false) // bob is back

	s2.flush("")
	if it, _ := st.Outbox("c1"); len(it) != 0 {
		t.Fatalf("outbox not drained after restart+flush: %+v", it)
	}
	if fb.sent.Load() != 1 {
		t.Fatalf("redelivered count = %d, want 1", fb.sent.Load())
	}
}

// waitFor polls cond up to ~2s. Async deliver goroutines need a moment.
func waitFor(cond func() bool) bool {
	for i := 0; i < 200; i++ {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func TestPersistAndLoad(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(func(id string) string {
		d := filepath.Join(dir, id)
		_ = os.MkdirAll(d, 0o755)
		return d
	})
	defer st.Close()
	mk := func() *Service {
		s := newTestService()
		s.store = st
		s.camp = func() string { return "c1" }
		return s
	}

	s := mk()
	// Owner "alice" creates a channel that includes us, then posts.
	s.onMsg("alice", mustJSON(Message{ID: "1", Kind: "channel", Peer: "alice/dev", Type: TypeCreate, Members: []string{"alice", "self"}, TS: 1}))
	s.onMsg("alice", mustJSON(Message{ID: "2", Kind: "channel", Peer: "alice/dev", Type: TypeText, Members: []string{"alice", "self"}, Body: "hi", TS: 2}))

	// A fresh service hydrates the same state from the store.
	s2 := mk()
	s2.LoadCamp()
	if chs := userChannels(s2); len(chs) != 1 || chs[0].ID != "alice/dev" {
		t.Fatalf("hydrated channels = %+v", chs)
	}
	msgs := s2.Messages("channel", "alice/dev", 0)
	var gotText bool
	for _, m := range msgs {
		if m.Type == TypeText && m.Body == "hi" {
			gotText = true
		}
	}
	if !gotText {
		t.Fatalf("hydrated messages missing the text post: %+v", msgs)
	}

	// Owner deletes the channel → dropped from memory and store.
	s2.onMsg("alice", mustJSON(Message{ID: "3", Kind: "channel", Peer: "alice/dev", Type: TypeDelete, TS: 3}))
	if chs := userChannels(s2); len(chs) != 0 {
		t.Fatalf("delete left channels: %+v", chs)
	}
	if chs, _ := st.Channels("c1"); len(chs) != 0 {
		t.Fatalf("delete left channel in store: %+v", chs)
	}
}

func TestOrderingAndDedup(t *testing.T) {
	s := newTestService()
	// Arrive out of order; a duplicate ID must be dropped.
	s.onMsg("bob", mustJSON(Message{ID: "b", Kind: "dm", To: "self", Body: "2", TS: 200}))
	s.onMsg("bob", mustJSON(Message{ID: "a", Kind: "dm", To: "self", Body: "1", TS: 100}))
	s.onMsg("bob", mustJSON(Message{ID: "a", Kind: "dm", To: "self", Body: "dup", TS: 100}))

	got := s.Messages("dm", "bob", 0)
	if len(got) != 2 {
		t.Fatalf("want 2 messages after dedup, got %d: %+v", len(got), got)
	}
	if got[0].Body != "1" || got[1].Body != "2" {
		t.Fatalf("want TS order 1,2; got %q,%q", got[0].Body, got[1].Body)
	}
}

func TestChannelMembershipAuthority(t *testing.T) {
	s := newTestService()
	const id = "alice/dev" // owner = "alice"
	// First sighting from the owner creates the channel with us in it.
	s.onMsg("alice", mustJSON(Message{ID: "1", Kind: "channel", Peer: id, Type: TypeCreate, Members: []string{"alice", "self"}, TS: 1}))
	if chs := userChannels(s); len(chs) != 1 || len(chs[0].Members) != 2 {
		t.Fatalf("create: channels=%+v", chs)
	}
	// A non-owner's roster must NOT rewrite membership.
	s.onMsg("mallory", mustJSON(Message{ID: "2", Kind: "channel", Peer: id, Type: TypeText, Members: []string{"mallory"}, Body: "hi", TS: 2}))
	if chs := userChannels(s); len(chs) != 1 || len(chs[0].Members) != 2 {
		t.Fatalf("non-owner rewrote roster: %+v", chs)
	}
	// The owner removing us drops the channel.
	s.onMsg("alice", mustJSON(Message{ID: "3", Kind: "channel", Peer: id, Type: TypeRemove, Members: []string{"alice"}, TS: 3}))
	if chs := userChannels(s); len(chs) != 0 {
		t.Fatalf("owner removal didn't drop channel: %+v", chs)
	}
}

func TestNotesLWW(t *testing.T) {
	s := newTestService()
	const id = "alice/dev"
	s.onMsg("alice", mustJSON(Message{ID: "1", Kind: "channel", Peer: id, Type: TypeCreate, Members: []string{"alice", "self"}, TS: 1}))
	// A notes edit (from any member) is folded into the channel's doc.
	s.onMsg("alice", mustJSON(Message{ID: "n1", Kind: "channel", Peer: id, Type: TypeNotes, Body: "first", TS: 10}))
	if got := s.Notes(id).Body; got != "first" {
		t.Fatalf("notes not applied: %q", got)
	}
	// A newer edit wins.
	s.onMsg("self", mustJSON(Message{ID: "n2", Kind: "channel", Peer: id, Type: TypeNotes, Body: "second", TS: 20}))
	if got := s.Notes(id).Body; got != "second" {
		t.Fatalf("newer notes lost: %q", got)
	}
	// A stale edit (older TS) is ignored.
	s.onMsg("alice", mustJSON(Message{ID: "n3", Kind: "channel", Peer: id, Type: TypeNotes, Body: "stale", TS: 5}))
	if got := s.Notes(id).Body; got != "second" {
		t.Fatalf("stale notes overwrote winner: %q", got)
	}
	// Notes edits don't leak into the conversation feed.
	if msgs := s.Messages("channel", id, 100); len(msgs) != 1 || msgs[0].Type != TypeCreate {
		t.Fatalf("notes polluted the feed: %+v", msgs)
	}
}

// A DM is a channel too — it carries notes, keyed by the peer's pub. An
// inbound DM notes edit from "bob" is stored under scope "bob" (the conv key),
// not leaked into the feed.
func TestNotesDM(t *testing.T) {
	s := newTestService() // self() == "self"
	s.onMsg("bob", mustJSON(Message{ID: "d1", Kind: "dm", Peer: "self", To: "self", From: "bob", Type: TypeNotes, Body: "hello", TS: 10}))
	if got := s.Notes("bob").Body; got != "hello" {
		t.Fatalf("dm notes not applied under peer scope: %q", got)
	}
	if msgs := s.Messages("dm", "bob", 100); len(msgs) != 0 {
		t.Fatalf("dm notes polluted the feed: %+v", msgs)
	}
}

func TestValidChannelName(t *testing.T) {
	ok := []string{"dev", "dev/backend", "a/b/c", "team-1/sub_2"}
	bad := []string{"", "/dev", "dev/", "a//b", "has space", "a/ b", "a/"}
	for _, n := range ok {
		if !validChannelName(n) {
			t.Errorf("want valid: %q", n)
		}
	}
	for _, n := range bad {
		if validChannelName(n) {
			t.Errorf("want invalid: %q", n)
		}
	}
}
