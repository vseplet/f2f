package messenger

import "testing"

// newTestService builds a Service without a bus (onMsg/append/Messages
// never touch it) for unit-testing the in-memory ordering and dedup logic.
func newTestService() *Service {
	return &Service{
		self:     func() string { return "self" },
		channels: map[string]*Channel{},
		convs:    map[string][]Message{},
		seen:     map[string]struct{}{},
		subs:     map[chan Message]struct{}{},
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
	if chs := s.Channels(); len(chs) != 1 || len(chs[0].Members) != 2 {
		t.Fatalf("create: channels=%+v", chs)
	}
	// A non-owner's roster must NOT rewrite membership.
	s.onMsg("mallory", mustJSON(Message{ID: "2", Kind: "channel", Peer: id, Type: TypeText, Members: []string{"mallory"}, Body: "hi", TS: 2}))
	if chs := s.Channels(); len(chs[0].Members) != 2 {
		t.Fatalf("non-owner rewrote roster: %+v", chs[0].Members)
	}
	// The owner removing us drops the channel.
	s.onMsg("alice", mustJSON(Message{ID: "3", Kind: "channel", Peer: id, Type: TypeRemove, Members: []string{"alice"}, TS: 3}))
	if chs := s.Channels(); len(chs) != 0 {
		t.Fatalf("owner removal didn't drop channel: %+v", chs)
	}
}
