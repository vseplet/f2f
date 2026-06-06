package messenger

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPerCampRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	defer s.Close()

	const camp = "abc_xyz"

	// channels (upsert renames)
	if err := s.UpsertChannel(camp, Channel{ID: "general", Name: "general"}); err != nil {
		t.Fatalf("upsert channel: %v", err)
	}
	if err := s.UpsertChannel(camp, Channel{ID: "general", Name: "General"}); err != nil {
		t.Fatalf("upsert channel (rename): %v", err)
	}
	chs, err := s.Channels(camp)
	if err != nil {
		t.Fatalf("channels: %v", err)
	}
	if len(chs) != 1 || chs[0].Name != "General" {
		t.Fatalf("channels = %+v, want 1 named General", chs)
	}

	// messages (oldest-first on read)
	for _, body := range []string{"first", "second", "third"} {
		if _, err := s.AddMessage(camp, Message{Kind: "channel", Peer: "general", AuthorName: "me", Body: body, Mine: true}); err != nil {
			t.Fatalf("add message: %v", err)
		}
	}
	msgs, err := s.Messages(camp, "channel", "general", 0)
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
	if _, err := os.Stat(filepath.Join(dir, camp+".messenger.db")); err != nil {
		t.Fatalf("expected per-camp db file: %v", err)
	}

	// a different camp is fully isolated (separate file)
	if other, _ := s.Messages("other_camp", "channel", "general", 0); len(other) != 0 {
		t.Fatalf("other camp should be empty, got %d", len(other))
	}
}
