package awg

import (
	"regexp"
	"strings"
	"testing"

	"github.com/vseplet/f2f/source/helper/engine/obfenv"
	"github.com/vseplet/f2f/source/helper/identity"
)

// Base config must include the private key as 64-char hex and all four
// magic headers — without those, amneziawg-go either crashes or
// silently misbehaves (e.g. handshakes never complete). Lock the shape
// with regex matches against the rendered text.
func TestBuildSelfConfigShape(t *testing.T) {
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	env := obfenv.NewCamp("test-camp-id_alice")
	got := BuildSelfConfig(id, env)

	required := []*regexp.Regexp{
		regexp.MustCompile(`(?m)^private_key=[0-9a-f]{64}$`),
		regexp.MustCompile(`(?m)^listen_port=0$`),
		// h1..h4 are single uint32 values in v1.0.4 — not ranges.
		regexp.MustCompile(`(?m)^h1=\d+$`),
		regexp.MustCompile(`(?m)^h2=\d+$`),
		regexp.MustCompile(`(?m)^h3=\d+$`),
		regexp.MustCompile(`(?m)^h4=\d+$`),
	}
	for _, re := range required {
		if !re.MatchString(got) {
			t.Errorf("missing %s in:\n%s", re.String(), got)
		}
	}
}

// h1..h4 ranges in the UAPI blob must EXACTLY match obfenv.SlotRange's
// view of the same slots — otherwise amneziawg-go and our multiplex
// would disagree on which packets are AWG, and traffic would silently
// drop at the discriminator.
func TestBuildSelfConfigRangesMatchObfenv(t *testing.T) {
	id, _ := identity.Generate()
	env := obfenv.NewCamp("range-check-camp")
	got := BuildSelfConfig(id, env)

	expectations := map[string]obfenv.Slot{
		"h1": obfenv.SlotAWGInit,
		"h2": obfenv.SlotAWGResponse,
		"h3": obfenv.SlotAWGCookie,
		"h4": obfenv.SlotAWGTransport,
	}
	for key, slot := range expectations {
		start, _ := env.SlotRange(slot)
		// amneziawg-go v1.0.4 takes a single uint32; we use slot start
		// (which is inside the [start, end) window the receiver scans).
		want := key + "=" + itoa(start)
		if !strings.Contains(got, want+"\n") {
			t.Errorf("expected %q in config, got:\n%s", want, got)
		}
	}
}

// Build a single peer block — every required UAPI key must be present
// in expected order (peer-keyed blocks reset on public_key in WG UAPI,
// so public_key MUST come first or the rest gets attached to nothing).
func TestBuildPeerBlockShape(t *testing.T) {
	p := PeerSyncInfo{
		WGPub:        strings.Repeat("a", 64),
		Endpoint:     "1.2.3.4:5678",
		AllowedCIDRs: []string{"100.64.7.42/32"},
	}
	got := BuildPeerBlock(p, 25)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d: %#v", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "public_key=") {
		t.Errorf("first line must be public_key=, got %q", lines[0])
	}
	for _, want := range []string{
		"public_key=" + p.WGPub,
		"endpoint=" + p.Endpoint,
		"allowed_ip=" + p.AllowedCIDRs[0],
		"persistent_keepalive_interval=25",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// Multiple allowed_ip lines for intercepts — every CIDR in
// AllowedCIDRs becomes its own allowed_ip line so AWG's trie covers
// all destinations that route via this peer.
func TestBuildPeerBlockMultipleAllowedIPs(t *testing.T) {
	p := PeerSyncInfo{
		WGPub:    strings.Repeat("a", 64),
		Endpoint: "1.2.3.4:5678",
		AllowedCIDRs: []string{
			"100.64.7.42/32",
			"10.0.0.0/8",
			"8.8.8.8/32",
		},
	}
	got := BuildPeerBlock(p, 25)
	for _, want := range p.AllowedCIDRs {
		if !strings.Contains(got, "allowed_ip="+want+"\n") {
			t.Errorf("missing allowed_ip=%s in:\n%s", want, got)
		}
	}
	// Order matters — first should be the overlay (callers depend on it
	// for IpcGet diagnostics readability).
	idx := strings.Index(got, "allowed_ip=")
	first := got[idx+len("allowed_ip=") : strings.Index(got[idx:], "\n")+idx]
	if first != "100.64.7.42/32" {
		t.Errorf("first allowed_ip should be overlay, got %q", first)
	}
}

// Empty endpoint / CIDR / keepalive are skipped — useful when we know
// the peer's pub but haven't observed its UDP address yet, or when we
// don't want to set a keepalive.
func TestBuildPeerBlockOptionalFields(t *testing.T) {
	p := PeerSyncInfo{WGPub: strings.Repeat("b", 64), AllowedCIDRs: []string{"", ""}}
	got := BuildPeerBlock(p, 0)
	if strings.Contains(got, "endpoint=") {
		t.Error("empty endpoint must be omitted")
	}
	if strings.Contains(got, "allowed_ip=") {
		t.Error("empty CIDR must be omitted")
	}
	if strings.Contains(got, "persistent_keepalive_interval") {
		t.Error("zero keepalive must be omitted")
	}
}

// Peers-block must lead with replace_peers=true so the device atomically
// swaps its peer list. Without it, the new blocks would be UPSERTed
// onto the existing peer set — peers that left the camp would linger.
func TestBuildPeersBlockReplaceFlag(t *testing.T) {
	got := BuildPeersBlock([]PeerSyncInfo{
		{WGPub: strings.Repeat("c", 64), Endpoint: "1.1.1.1:1", AllowedCIDRs: []string{"100.64.0.1/32"}},
	}, 25)
	if !strings.HasPrefix(got, "replace_peers=true\n") {
		t.Fatalf("must start with replace_peers=true, got:\n%s", got)
	}
}

// Zero peers is a valid call — it clears all peers from the device
// (device stays up, just doesn't route anywhere). Used during teardown
// or when the only known peer disconnects.
func TestBuildPeersBlockEmpty(t *testing.T) {
	got := BuildPeersBlock(nil, 25)
	if got != "replace_peers=true\n" {
		t.Errorf("expected exactly replace_peers=true, got: %q", got)
	}
}

// First-time push: prev is empty, curr has peers → blob emits full
// peer blocks (create semantics, no update_only). Drives the "device
// just came up, all peers are new" path.
func TestIncrementalFirstPush(t *testing.T) {
	curr := NormalizePeers([]PeerSyncInfo{
		{
			WGPub:        strings.Repeat("a", 64),
			Endpoint:     "1.1.1.1:1",
			AllowedCIDRs: []string{"100.64.0.1/32"},
		},
		{
			WGPub:        strings.Repeat("b", 64),
			Endpoint:     "2.2.2.2:2",
			AllowedCIDRs: []string{"100.64.0.2/32", "10.0.0.0/8"},
		},
	})
	blob := BuildIncrementalBlock(nil, curr, 25)
	if blob == "" {
		t.Fatal("expected non-empty blob, got empty")
	}
	if strings.Contains(blob, "update_only=true") {
		t.Errorf("first push should not use update_only: %s", blob)
	}
	if strings.Contains(blob, "remove=true") {
		t.Errorf("first push should not have removes: %s", blob)
	}
	// Both peers' public_key must appear.
	for _, p := range curr {
		if !strings.Contains(blob, "public_key="+p.WGPub) {
			t.Errorf("missing public_key=%s in blob:\n%s", p.WGPub, blob)
		}
	}
}

// Identical prev and curr → empty blob → caller skips IpcSet → live
// sessions preserved. This is the optimization that kills the
// every-30s thrash.
func TestIncrementalNoOpWhenUnchanged(t *testing.T) {
	peers := NormalizePeers([]PeerSyncInfo{
		{WGPub: strings.Repeat("a", 64), Endpoint: "1.1.1.1:1", AllowedCIDRs: []string{"100.64.0.1/32"}},
	})
	blob := BuildIncrementalBlock(peers, peers, 25)
	if blob != "" {
		t.Errorf("expected empty blob for identical snapshot, got:\n%s", blob)
	}
}

// Peer removed from camp → emit remove command for that pub. Other
// peers' sessions remain untouched.
func TestIncrementalRemove(t *testing.T) {
	a := PeerSyncInfo{WGPub: strings.Repeat("a", 64), Endpoint: "1.1.1.1:1", AllowedCIDRs: []string{"100.64.0.1/32"}}
	b := PeerSyncInfo{WGPub: strings.Repeat("b", 64), Endpoint: "2.2.2.2:2", AllowedCIDRs: []string{"100.64.0.2/32"}}
	prev := NormalizePeers([]PeerSyncInfo{a, b})
	curr := NormalizePeers([]PeerSyncInfo{a}) // b removed
	blob := BuildIncrementalBlock(prev, curr, 25)
	wantRemove := "public_key=" + b.WGPub + "\nremove=true\n"
	if !strings.Contains(blob, wantRemove) {
		t.Errorf("missing %q in blob:\n%s", wantRemove, blob)
	}
	// Should NOT touch peer a — no public_key=aaa... block (other than
	// the one in the remove for b which is a different pub).
	if strings.Contains(blob, "public_key="+a.WGPub) {
		t.Errorf("blob touches unchanged peer a, should not:\n%s", blob)
	}
}

// Peer endpoint changed (e.g. NAT-rebind picked up via camp poll) →
// emit update_only + new endpoint, no replace_allowed_ips. Session is
// preserved.
func TestIncrementalEndpointChanged(t *testing.T) {
	a := PeerSyncInfo{WGPub: strings.Repeat("a", 64), Endpoint: "1.1.1.1:1", AllowedCIDRs: []string{"100.64.0.1/32"}}
	a2 := a
	a2.Endpoint = "1.1.1.1:9999"
	prev := NormalizePeers([]PeerSyncInfo{a})
	curr := NormalizePeers([]PeerSyncInfo{a2})
	blob := BuildIncrementalBlock(prev, curr, 25)
	if !strings.Contains(blob, "update_only=true") {
		t.Errorf("endpoint change should use update_only:\n%s", blob)
	}
	if !strings.Contains(blob, "endpoint=1.1.1.1:9999") {
		t.Errorf("missing new endpoint:\n%s", blob)
	}
	if strings.Contains(blob, "replace_allowed_ips=true") {
		t.Errorf("endpoint-only change should NOT emit replace_allowed_ips:\n%s", blob)
	}
}

// Peer's AllowedCIDRs changed (intercept added/removed) → emit
// update_only + replace_allowed_ips=true + new IP list. Session is
// preserved; only the routing trie is updated.
func TestIncrementalAllowedCIDRsChanged(t *testing.T) {
	a := PeerSyncInfo{WGPub: strings.Repeat("a", 64), Endpoint: "1.1.1.1:1", AllowedCIDRs: []string{"100.64.0.1/32"}}
	a2 := a
	a2.AllowedCIDRs = []string{"100.64.0.1/32", "10.0.0.0/8"}
	prev := NormalizePeers([]PeerSyncInfo{a})
	curr := NormalizePeers([]PeerSyncInfo{a2})
	blob := BuildIncrementalBlock(prev, curr, 25)
	if !strings.Contains(blob, "update_only=true") {
		t.Errorf("CIDR change should use update_only:\n%s", blob)
	}
	if !strings.Contains(blob, "replace_allowed_ips=true") {
		t.Errorf("CIDR change should use replace_allowed_ips=true:\n%s", blob)
	}
	if !strings.Contains(blob, "allowed_ip=10.0.0.0/8") {
		t.Errorf("missing new CIDR:\n%s", blob)
	}
	if strings.Contains(blob, "endpoint=") {
		t.Errorf("CIDR-only change should NOT emit endpoint line:\n%s", blob)
	}
}

// Order-independence: AllowedCIDRs in different declaration orders
// must be considered equal after NormalizePeers. Otherwise a routine
// camp poll could spuriously re-emit identical configs.
func TestIncrementalOrderIndependent(t *testing.T) {
	prev := NormalizePeers([]PeerSyncInfo{
		{WGPub: strings.Repeat("a", 64), Endpoint: "1:1", AllowedCIDRs: []string{"10.0.0.0/8", "100.64.0.1/32"}},
	})
	curr := NormalizePeers([]PeerSyncInfo{
		{WGPub: strings.Repeat("a", 64), Endpoint: "1:1", AllowedCIDRs: []string{"100.64.0.1/32", "10.0.0.0/8"}},
	})
	if blob := BuildIncrementalBlock(prev, curr, 25); blob != "" {
		t.Errorf("reordered CIDRs should yield empty diff, got:\n%s", blob)
	}
}

func itoa(u uint32) string {
	const digits = "0123456789"
	if u == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for u > 0 {
		i--
		buf[i] = digits[u%10]
		u /= 10
	}
	return string(buf[i:])
}
