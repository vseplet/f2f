package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/vseplet/f2f/source/helper/identity"
)

// fakeNet wires several fakeNodes so Request/Notify route to the target's
// registered handlers — an in-memory stand-in for mesh/bus.
type fakeNet struct{ nodes map[string]*fakeNode }

type fakeNode struct {
	pub      string
	net      *fakeNet
	handlers map[string]func(string, []byte) ([]byte, error)
}

func (n *fakeNet) node(pub string) *fakeNode {
	fn := &fakeNode{pub: pub, net: n, handlers: map[string]func(string, []byte) ([]byte, error){}}
	n.nodes[pub] = fn
	return fn
}

func (n *fakeNode) Handle(typ string, fn func(string, []byte) ([]byte, error)) { n.handlers[typ] = fn }
func (n *fakeNode) Request(_ context.Context, pub, typ string, payload []byte) ([]byte, error) {
	t := n.net.nodes[pub]
	if t == nil || t.handlers[typ] == nil {
		return nil, fmt.Errorf("no handler %s@%s", typ, pub)
	}
	return t.handlers[typ](n.pub, payload)
}
func (n *fakeNode) Notify(pub, typ string, payload []byte) error {
	if t := n.net.nodes[pub]; t != nil && t.handlers[typ] != nil {
		_, _ = t.handlers[typ](n.pub, payload)
	}
	return nil
}
func (n *fakeNode) Peers() []string {
	var out []string
	for p := range n.net.nodes {
		if p != n.pub {
			out = append(out, p)
		}
	}
	return out
}

func TestSyncPull(t *testing.T) {
	net := &fakeNet{nodes: map[string]*fakeNode{}}
	a, b := net.node("A"), net.node("B")
	sa, sb := New(NewMemStore()), New(NewMemStore())
	NewSync(sa, a).Register()
	syncB := NewSync(sb, b)
	syncB.Register()

	id, _ := identity.Generate()
	for _, body := range []string{"x", "y", "z"} {
		if _, err := sa.Commit(id, "doc:1", "block.text", []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := syncB.PullScope(context.Background(), "A", "doc:1"); err != nil {
		t.Fatalf("pull: %v", err)
	}
	ae, be := sa.Frames("doc:1"), sb.Frames("doc:1")
	if len(be) != 3 || len(ae) != len(be) {
		t.Fatalf("len a=%d b=%d", len(ae), len(be))
	}
	for i := range ae {
		if ae[i].ID != be[i].ID {
			t.Fatalf("diverged at %d", i)
		}
	}
	// idempotent second pull
	if err := syncB.PullScope(context.Background(), "A", "doc:1"); err != nil {
		t.Fatal(err)
	}
	if len(sb.Frames("doc:1")) != 3 {
		t.Fatal("second pull changed count")
	}
}

// TestSyncScopeDiscovery: B joins after A already has a scope and was never
// pushed it. PullAll must discover the scope via db.scopes and pull it in.
func TestSyncScopeDiscovery(t *testing.T) {
	net := &fakeNet{nodes: map[string]*fakeNode{}}
	a, b := net.node("A"), net.node("B")
	sa, sb := New(NewMemStore()), New(NewMemStore())
	NewSync(sa, a).Register()
	syncB := NewSync(sb, b)
	syncB.Register()

	id, _ := identity.Generate()
	// A populates a scope B has never heard of, with no live push to B.
	for _, body := range []string{"a", "b", "c"} {
		if _, err := sa.Commit(id, "note:*/general", "block.text", []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if len(sb.Scopes()) != 0 {
		t.Fatalf("B should know no scopes yet, has %v", sb.Scopes())
	}
	syncB.PullAll(context.Background())
	if got := len(sb.Frames("note:*/general")); got != 3 {
		t.Fatalf("B got %d entries after discovery, want 3", got)
	}
}

// TestSyncMembershipGating: A serves "open" to everyone but withholds "secret"
// from B (not a member). B's PullAll gets open, never secret.
func TestSyncMembershipGating(t *testing.T) {
	net := &fakeNet{nodes: map[string]*fakeNode{}}
	a, b := net.node("A"), net.node("B")
	sa, sb := New(NewMemStore()), New(NewMemStore())
	syncA := NewSync(sa, a)
	syncA.Register()
	syncA.SetMemberCheck(func(scope, peer string) bool {
		return scope != "secret" // B is not a member of "secret"
	})
	syncB := NewSync(sb, b)
	syncB.Register()

	id, _ := identity.Generate()
	if _, err := sa.Commit(id, "open", "block.text", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := sa.Commit(id, "secret", "block.text", []byte("y")); err != nil {
		t.Fatal(err)
	}
	syncB.PullAll(context.Background())
	if got := len(sb.Frames("open")); got != 1 {
		t.Fatalf("B should have the open scope (%d)", got)
	}
	if got := len(sb.Frames("secret")); got != 0 {
		t.Fatalf("B got %d secret frames despite gating", got)
	}
}

func TestSyncPush(t *testing.T) {
	net := &fakeNet{nodes: map[string]*fakeNode{}}
	a, b := net.node("A"), net.node("B")
	sa, sb := New(NewMemStore()), New(NewMemStore())
	syncA := NewSync(sa, a)
	syncA.Register()
	NewSync(sb, b).Register()

	id, _ := identity.Generate()
	for _, body := range []string{"one", "two"} {
		e, err := sa.Commit(id, "chan:x", "chat.msg", []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		syncA.Push(e) // eager fan-out → B's onPush applies
	}
	if got := len(sb.Frames("chan:x")); got != 2 {
		t.Fatalf("B got %d entries via push, want 2", got)
	}
}
