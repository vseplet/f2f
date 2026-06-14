package db

import (
	"testing"

	"github.com/vseplet/f2f/source/helper/identity"
)

func newSvc(t *testing.T) *Service {
	t.Helper()
	s := New(NewMemStore())
	s.now = func() int64 { return 1 } // deterministic TS in tests
	return s
}

func mustID(t *testing.T) *identity.Identity {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	return id
}

// TestCommitChain checks local commits build a verified per-author chain.
func TestCommitChain(t *testing.T) {
	svc := newSvc(t)
	id := mustID(t)

	e1, err := svc.Commit(id, "doc:1", "block.create", []byte("hello"))
	if err != nil {
		t.Fatalf("commit1: %v", err)
	}
	e2, err := svc.Commit(id, "doc:1", "block.update", []byte("world"))
	if err != nil {
		t.Fatalf("commit2: %v", err)
	}
	if e1.Seq != 1 || e2.Seq != 2 {
		t.Fatalf("seq: %d %d", e1.Seq, e2.Seq)
	}
	if e2.Prev != e1.ID {
		t.Fatalf("chain broken: prev=%s id1=%s", e2.Prev, e1.ID)
	}
	if e2.Lamport <= e1.Lamport {
		t.Fatalf("lamport not advancing: %d %d", e1.Lamport, e2.Lamport)
	}
	if err := e1.verify(); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if vv := svc.Vector("doc:1"); vv[id.PubHex()] != 2 {
		t.Fatalf("vector: %v", vv)
	}
}

// TestLamportSurvivesRestart: a fresh Service over a populated store must not
// stamp new writes below persisted Lamports (the in-memory clock resets to 0
// on restart) — else the new edit folds as "older" than the version it
// supersedes and the UI shows stale content until reload.
func TestLamportSurvivesRestart(t *testing.T) {
	store := NewMemStore()
	id := mustID(t)

	svc := New(store)
	svc.now = func() int64 { return 1 }
	var last uint64
	for i := 0; i < 5; i++ {
		e, err := svc.Commit(id, "doc:1", "block.update", []byte("x"))
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
		last = e.Lamport
	}

	// Simulate a restart: brand-new Service (lamport=0) over the same store.
	svc2 := New(store)
	svc2.now = func() int64 { return 1 }
	e, err := svc2.Commit(id, "doc:1", "block.update", []byte("y"))
	if err != nil {
		t.Fatalf("post-restart commit: %v", err)
	}
	if e.Lamport <= last {
		t.Fatalf("lamport regressed after restart: had %d, got %d", last, e.Lamport)
	}
}

// TestReplication: B catches up from A via Since(have) + Apply, converging.
func TestReplication(t *testing.T) {
	a := newSvc(t)
	b := newSvc(t)
	id := mustID(t)

	for _, body := range []string{"a", "b", "c"} {
		if _, err := a.Commit(id, "chan:x", "chat.msg", []byte(body)); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	// B asks "I have nothing" → A sends everything → B applies.
	missing := a.Since("chan:x", b.Vector("chan:x"))
	if len(missing) != 3 {
		t.Fatalf("missing = %d, want 3", len(missing))
	}
	for _, e := range missing {
		if err := b.Apply(e); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
	// Converged: same vector, same ordered entries.
	if b.Vector("chan:x")[id.PubHex()] != 3 {
		t.Fatalf("b vector: %v", b.Vector("chan:x"))
	}
	ae, be := a.Frames("chan:x"), b.Frames("chan:x")
	if len(ae) != len(be) {
		t.Fatalf("len mismatch %d %d", len(ae), len(be))
	}
	for i := range ae {
		if ae[i].ID != be[i].ID {
			t.Fatalf("order/content diverged at %d", i)
		}
	}
	// A second sync is a no-op (B already current).
	if again := a.Since("chan:x", b.Vector("chan:x")); len(again) != 0 {
		t.Fatalf("expected nothing new, got %d", len(again))
	}
}

// TestDumpImport: whole-DB dump shared into a fresh service.
func TestDumpImport(t *testing.T) {
	a := newSvc(t)
	id := mustID(t)
	a.Commit(id, "doc:1", "block.create", []byte("x"))
	a.Commit(id, "doc:1", "block.update", []byte("y"))
	a.Commit(id, "tasks:1", "block.create", []byte("t"))

	blob, err := a.Dump()
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	b := newSvc(t)
	n, err := b.Import(blob)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if n != 3 {
		t.Fatalf("imported %d, want 3", n)
	}
	if b.Vector("doc:1")[id.PubHex()] != 2 || b.Vector("tasks:1")[id.PubHex()] != 1 {
		t.Fatalf("vectors wrong: %v %v", b.Vector("doc:1"), b.Vector("tasks:1"))
	}
	// Re-import is idempotent.
	n2, _ := b.Import(blob)
	if n2 != 0 {
		t.Fatalf("re-import added %d, want 0", n2)
	}
}

// TestTamperRejected: a flipped payload fails verification on Append.
func TestTamperRejected(t *testing.T) {
	svc := newSvc(t)
	id := mustID(t)
	e, _ := svc.Commit(id, "doc:1", "block.create", []byte("orig"))

	tampered := *e
	tampered.Payload = []byte("evil") // ID/sig no longer match
	if err := NewMemStore().Append(&tampered); err == nil {
		t.Fatal("tampered entry was accepted")
	}
}

// TestSeqGapRejected: applying seq 2 before seq 1 is refused.
func TestSeqGapRejected(t *testing.T) {
	a := newSvc(t)
	id := mustID(t)
	a.Commit(id, "doc:1", "block.create", nil)
	e2, _ := a.Commit(id, "doc:1", "block.update", nil)

	b := newSvc(t)
	if err := b.Apply(e2); err == nil {
		t.Fatal("seq gap (applied 2 before 1) was accepted")
	}
}
