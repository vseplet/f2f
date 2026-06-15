package message

import (
	"testing"

	"github.com/vseplet/f2f/source/helper/db/blocks"
	"github.com/vseplet/f2f/source/helper/db"
	"github.com/vseplet/f2f/source/helper/identity"
)

func newMgr(t *testing.T) *Manager {
	t.Helper()
	return New(blocks.New(db.New(db.NewMemStore())))
}

func id(t *testing.T) *identity.Identity {
	t.Helper()
	i, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestPostAndList(t *testing.T) {
	m := newMgr(t)
	a := id(t)
	ch := "chan1"
	if _, err := m.Post(a, ch, "hello", nil, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Post(a, ch, "world", nil, "", ""); err != nil {
		t.Fatal(err)
	}
	msgs := m.Messages(ch)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Body != "hello" || msgs[1].Body != "world" {
		t.Fatalf("order/content wrong: %+v", msgs)
	}
	if msgs[0].From != a.PubHex() {
		t.Fatalf("from wrong: %q", msgs[0].From)
	}
}

func TestEditKeepsIDMarksEdited(t *testing.T) {
	m := newMgr(t)
	a := id(t)
	ch := "chan1"
	bid, _ := m.Post(a, ch, "typo", nil, "", "")
	if err := m.Edit(a, ch, bid, "fixed", nil); err != nil {
		t.Fatal(err)
	}
	msgs := m.Messages(ch)
	if len(msgs) != 1 {
		t.Fatalf("edit created a new message: %d", len(msgs))
	}
	if msgs[0].ID != bid || msgs[0].Body != "fixed" || !msgs[0].Edited {
		t.Fatalf("edit fold wrong: %+v", msgs[0])
	}
}

func TestReplyThreadDelete(t *testing.T) {
	m := newMgr(t)
	a := id(t)
	ch := "chan1"
	root, _ := m.Post(a, ch, "root", nil, "", "")
	reply, _ := m.Post(a, ch, "re", nil, root, root)
	if got := m.Get(ch, reply); got.ReplyTo != root || got.Thread != root {
		t.Fatalf("reply/thread lost: %+v", got)
	}
	// edit preserves reply/thread links
	if err := m.Edit(a, ch, reply, "re!", nil); err != nil {
		t.Fatal(err)
	}
	if got := m.Get(ch, reply); got.ReplyTo != root || got.Thread != root {
		t.Fatalf("edit dropped links: %+v", got)
	}
	// delete removes it
	if err := m.Delete(a, ch, reply); err != nil {
		t.Fatal(err)
	}
	if len(m.Messages(ch)) != 1 {
		t.Fatal("delete didn't remove the reply")
	}
}
