package bus

import (
	"context"
	"errors"
	"testing"
	"time"
)

// staticResolver is a fixed pub↔IP table for loopback tests.
type staticResolver struct {
	self  string
	peers map[string]string // pub → ip
}

func (r staticResolver) AddrForPub(pub string) string { return r.peers[pub] }
func (r staticResolver) PubForIP(ip string) string {
	for pub, addr := range r.peers {
		if addr == ip {
			return pub
		}
	}
	return ""
}
func (r staticResolver) SelfPub() string { return r.self }
func (r staticResolver) Peers() []string {
	out := make([]string, 0, len(r.peers))
	for pub := range r.peers {
		out = append(out, pub)
	}
	return out
}

// TestRequestRoundTrip drives the full path the migrated services use:
// dial, frame, dispatch to a registered handler, read the response.
func TestRequestRoundTrip(t *testing.T) {
	recv, err := New(staticResolver{self: "bb", peers: map[string]string{"aa": "127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := recv.Start("127.0.0.1"); err != nil {
		t.Skipf("bind 127.0.0.1:%s: %v (port busy?)", Port, err)
	}
	defer recv.Stop()
	recv.Handle("echo2", func(fromPub string, payload []byte) ([]byte, error) {
		if fromPub != "aa" {
			t.Errorf("fromPub = %q, want aa", fromPub)
		}
		return append([]byte("re:"), payload...), nil
	})
	recv.Handle("boom", func(string, []byte) ([]byte, error) {
		return nil, errors.New("handler failure")
	})

	// Sender never listens — Request dials out, like a service poll.
	// self "aa" < peer "bb", so connFor dials immediately (no inbound wait).
	snd, err := New(staticResolver{self: "aa", peers: map[string]string{"bb": "127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer snd.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := snd.Request(ctx, "bb", "echo2", []byte("hi"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if string(got) != "re:hi" {
		t.Fatalf("response = %q, want re:hi", got)
	}

	// A handler error must surface as a request error, not hang or
	// come back as an empty success — the services' bus-first fetch
	// relies on this to fall back to HTTP.
	if _, err := snd.Request(ctx, "bb", "boom", nil); err == nil {
		t.Fatal("request to failing handler succeeded, want error")
	}
}
