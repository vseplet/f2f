package messenger

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestPerCampRoundTrip(t *testing.T) {
	dir := t.TempDir()
	campDir := func(id string) string {
		d := filepath.Join(dir, id)
		_ = os.MkdirAll(d, 0o755)
		return d
	}
	s := NewStore(campDir)
	defer s.Close()

	const camp = "abc_xyz"
	const chanID = "alice/general"

	// channels (upsert updates the roster)
	if err := s.UpsertChannel(camp, Channel{ID: chanID, Name: "general", Owner: "alice", Members: []string{"alice"}}); err != nil {
		t.Fatalf("upsert channel: %v", err)
	}
	if err := s.UpsertChannel(camp, Channel{ID: chanID, Name: "general", Owner: "alice", Members: []string{"alice", "bob"}}); err != nil {
		t.Fatalf("upsert channel (members): %v", err)
	}
	chs, err := s.Channels(camp)
	if err != nil {
		t.Fatalf("channels: %v", err)
	}
	if len(chs) != 1 || len(chs[0].Members) != 2 {
		t.Fatalf("channels = %+v, want 1 with 2 members", chs)
	}

	// messages (oldest-first on read)
	for i, body := range []string{"first", "second", "third"} {
		m := Message{ID: "m" + strconv.Itoa(i), Kind: "channel", Peer: chanID, Type: TypeText, From: "me", Body: body, Mine: true, TS: int64(i + 1)}
		if err := s.AddMessage(camp, m); err != nil {
			t.Fatalf("add message: %v", err)
		}
	}
	msgs, err := s.Messages(camp, "channel", chanID, 0)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(msgs) != 3 || msgs[0].Body != "first" || msgs[2].Body != "third" {
		t.Fatalf("messages = %+v, want first..third oldest-first", msgs)
	}
	if !msgs[0].Mine || msgs[0].TS == 0 {
		t.Fatalf("message flags not persisted: %+v", msgs[0])
	}

	// per-camp file actually exists and is named by camp_id
	if _, err := os.Stat(filepath.Join(dir, camp, "messenger.db")); err != nil {
		t.Fatalf("expected per-camp db file: %v", err)
	}

	// a different camp is fully isolated (separate file)
	if other, _ := s.Messages("other_camp", "channel", chanID, 0); len(other) != 0 {
		t.Fatalf("other camp should be empty, got %d", len(other))
	}
}
