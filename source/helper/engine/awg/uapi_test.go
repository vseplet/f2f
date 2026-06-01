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
