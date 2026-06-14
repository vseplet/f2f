package blocks

import (
	"encoding/json"
	"testing"

	"github.com/vseplet/f2f/source/helper/db"
	"github.com/vseplet/f2f/source/helper/identity"
)

func newMgr(t *testing.T) *Manager {
	t.Helper()
	return New(db.New(db.NewMemStore()))
}

func id(t *testing.T) *identity.Identity {
	t.Helper()
	i, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestCreateUpdateFold(t *testing.T) {
	m := newMgr(t)
	a := id(t)
	bid, err := m.Create(a, "doc:1", "text", json.RawMessage(`{"md":"hello"}`), "", "a")
	if err != nil {
		t.Fatal(err)
	}
	b := m.Block("doc:1", bid)
	if b == nil || b.Type != "text" || b.Channel != "doc:1" || len(b.Heads) != 1 {
		t.Fatalf("create fold wrong: %+v", b)
	}
	if string(b.Heads[0].Content) != `{"md":"hello"}` || b.Heads[0].Author != a.PubHex() {
		t.Fatalf("head wrong: %+v", b.Heads[0])
	}
	// update supersedes → still one head, new content
	if err := m.Update(a, "doc:1", bid, json.RawMessage(`{"md":"world"}`), nil); err != nil {
		t.Fatal(err)
	}
	b = m.Block("doc:1", bid)
	if len(b.Heads) != 1 || string(b.Heads[0].Content) != `{"md":"world"}` {
		t.Fatalf("update fold wrong: %+v", b.Heads)
	}
}

func TestConcurrentVariantsAndMerge(t *testing.T) {
	m := newMgr(t)
	a, c := id(t), id(t)
	bid, _ := m.Create(a, "doc:1", "text", json.RawMessage(`{"md":"base"}`), "", "a")
	v1 := m.Block("doc:1", bid).Heads[0].EntryID

	// two authors branch from v1 → two heads (variants/tabs)
	if err := m.Update(a, "doc:1", bid, json.RawMessage(`{"md":"A"}`), []string{v1}); err != nil {
		t.Fatal(err)
	}
	if err := m.Update(c, "doc:1", bid, json.RawMessage(`{"md":"C"}`), []string{v1}); err != nil {
		t.Fatal(err)
	}
	b := m.Block("doc:1", bid)
	if len(b.Heads) != 2 {
		t.Fatalf("want 2 variants, got %d: %+v", len(b.Heads), b.Heads)
	}

	// merge collapses to one head with chosen content
	if err := m.Merge(a, "doc:1", bid, json.RawMessage(`{"md":"merged"}`)); err != nil {
		t.Fatal(err)
	}
	b = m.Block("doc:1", bid)
	if len(b.Heads) != 1 || string(b.Heads[0].Content) != `{"md":"merged"}` {
		t.Fatalf("merge fold wrong: %+v", b.Heads)
	}
}

func TestDeleteTombstone(t *testing.T) {
	m := newMgr(t)
	a := id(t)
	bid, _ := m.Create(a, "doc:1", "task", json.RawMessage(`{"text":"do it"}`), "", "a")
	if err := m.Delete(a, "doc:1", bid, nil); err != nil {
		t.Fatal(err)
	}
	b := m.Block("doc:1", bid)
	if b == nil || !b.Deleted {
		t.Fatalf("expected deleted block, got %+v", b)
	}
}

func TestOrderingByPos(t *testing.T) {
	m := newMgr(t)
	a := id(t)
	m.Create(a, "doc:1", "text", json.RawMessage(`{}`), "", "b")
	m.Create(a, "doc:1", "text", json.RawMessage(`{}`), "", "a")
	bs := m.Blocks("doc:1")
	if len(bs) != 2 || bs[0].Pos != "a" || bs[1].Pos != "b" {
		t.Fatalf("ordering wrong: %v", []string{bs[0].Pos, bs[1].Pos})
	}
}
