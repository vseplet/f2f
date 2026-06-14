package db

import (
	"testing"

	"github.com/vseplet/f2f/source/helper/identity"
)

func TestSQLitePersistence(t *testing.T) {
	dir := t.TempDir()
	dirFn := func() string { return dir }
	id, _ := identity.Generate()

	// write 3 entries, then drop the service/store entirely
	svc1 := New(NewSQLiteStore(dirFn))
	for _, b := range []string{"a", "b", "c"} {
		if _, err := svc1.Commit(id, "doc:1", "block.text", []byte(b)); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	// reopen the same dir with a fresh store → data persisted
	svc2 := New(NewSQLiteStore(dirFn))
	es := svc2.Entries("doc:1")
	if len(es) != 3 {
		t.Fatalf("after reopen got %d entries, want 3", len(es))
	}
	if vv := svc2.Vector("doc:1"); vv[id.PubHex()] != 3 {
		t.Fatalf("vector after reopen: %v", vv)
	}
	if e := svc2.store.Head("doc:1", id.PubHex()); e == nil || e.Seq != 3 {
		t.Fatalf("head wrong after reopen: %+v", e)
	}
	// chain still intact: committing a 4th continues from seq 3
	e4, err := svc2.Commit(id, "doc:1", "block.text", []byte("d"))
	if err != nil || e4.Seq != 4 {
		t.Fatalf("continue chain: seq=%d err=%v", e4.Seq, err)
	}
	// tamper rejected by the persistent store too
	bad := *e4
	bad.Payload = []byte("evil")
	if err := svc2.store.Append(&bad); err == nil {
		t.Fatal("tampered entry accepted by sqlite store")
	}
}
