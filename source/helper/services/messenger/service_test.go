package messenger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

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
