package channels

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

func TestCreateListOwner(t *testing.T) {
	m := newMgr(t)
	owner := id(t)
	bid, err := m.Create(owner, "lobby", "", "")
	if err != nil {
		t.Fatal(err)
	}
	c := m.Get(bid)
	if c == nil || c.Name != "lobby" || c.Owner != owner.PubHex() {
		t.Fatalf("channel wrong: %+v", c)
	}
	if len(m.List()) != 1 {
		t.Fatalf("list = %d, want 1", len(m.List()))
	}
}

// TestGeneralReserved: general can't be recreated nor sub-channeled.
func TestGeneralReserved(t *testing.T) {
	m := newMgr(t)
	owner := id(t)
	if _, err := m.Create(owner, "general", "", ""); err == nil {
		t.Fatal("creating a channel named general should be rejected")
	}
	if _, err := m.Create(owner, "general/rules", "", ""); err == nil {
		t.Fatal("creating a sub-channel under general should be rejected")
	}
}

func TestMembership(t *testing.T) {
	m := newMgr(t)
	owner, bob := id(t), id(t)
	bid, _ := m.Create(owner, "projectx", "", "")

	// owner is always a member; others only when added
	if !m.IsMember(bid, owner.PubHex()) {
		t.Fatal("owner not a member")
	}
	if m.IsMember(bid, bob.PubHex()) {
		t.Fatal("bob a member before add")
	}
	if err := m.AddMember(owner, bid, bob.PubHex()); err != nil {
		t.Fatal(err)
	}
	if !m.IsMember(bid, bob.PubHex()) {
		t.Fatal("bob not a member after add")
	}
	// idempotent add, then remove
	if err := m.AddMember(owner, bid, bob.PubHex()); err != nil {
		t.Fatal(err)
	}
	if got := m.Get(bid).Members; len(got) != 1 {
		t.Fatalf("members = %v, want 1", got)
	}
	if err := m.RemoveMember(owner, bid, bob.PubHex()); err != nil {
		t.Fatal(err)
	}
	if m.IsMember(bid, bob.PubHex()) {
		t.Fatal("bob still a member after remove")
	}
}

func TestRename(t *testing.T) {
	m := newMgr(t)
	owner := id(t)
	bid, _ := m.Create(owner, "backend", "", "")
	if err := m.Rename(owner, bid, "backend-core"); err != nil {
		t.Fatal(err)
	}
	if c := m.Get(bid); c == nil || c.Name != "backend-core" {
		t.Fatalf("rename failed: %+v", c)
	}
}

func TestRegularBIDNamespacedByCreator(t *testing.T) {
	m := newMgr(t)
	owner := id(t)
	bid, _ := m.Create(owner, "x", "", "")
	if want := owner.PubHex()[:16] + "-"; bid[:17] != want {
		t.Fatalf("bid %q not prefixed by creator fp %q", bid, want)
	}
}

func TestGeneralWellKnown(t *testing.T) {
	m := newMgr(t)
	a := id(t)
	bid, err := m.EnsureGeneral(a)
	if err != nil || bid != GeneralBID {
		t.Fatalf("ensure general: %q %v", bid, err)
	}
	// idempotent: second ensure doesn't fork or duplicate
	if _, err := m.EnsureGeneral(a); err != nil {
		t.Fatal(err)
	}
	if n := len(m.List()); n != 1 {
		t.Fatalf("general listed %d times, want 1", n)
	}
}

func TestDMDeterministic(t *testing.T) {
	a, b := id(t), id(t)
	// both sides derive the same bid regardless of argument order
	if DMBID(a.PubHex(), b.PubHex()) != DMBID(b.PubHex(), a.PubHex()) {
		t.Fatal("DM bid not order-independent")
	}
	m := newMgr(t)
	bid, err := m.EnsureDM(a, a.PubHex(), b.PubHex())
	if err != nil || bid != DMBID(a.PubHex(), b.PubHex()) {
		t.Fatalf("ensure dm: %q %v", bid, err)
	}
	if !m.IsMember(bid, a.PubHex()) || !m.IsMember(bid, b.PubHex()) {
		t.Fatal("both pubs should be DM members")
	}
}

func TestDelete(t *testing.T) {
	m := newMgr(t)
	owner := id(t)
	bid, _ := m.Create(owner, "temp", "", "")
	if err := m.Delete(owner, bid); err != nil {
		t.Fatal(err)
	}
	if m.Get(bid) != nil {
		t.Fatal("deleted channel still visible")
	}
	if len(m.List()) != 0 {
		t.Fatal("deleted channel still listed")
	}
}
