package identity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"testing"
)

// Two identities built from the same Ed25519 seed must produce the same
// X25519 keypair — otherwise nodes that rotate priv.key on every restart
// would generate fresh transport keys and break sticky peer connections.
func TestX25519DerivationDeterministic(t *testing.T) {
	priv := ed25519Seeded(t, "the same seed every time forever ")
	a := newIdentity(priv, priv.Public().(ed25519.PublicKey), "")
	b := newIdentity(priv, priv.Public().(ed25519.PublicKey), "")
	if a.X25519PubHex() != b.X25519PubHex() {
		t.Fatalf("non-deterministic x25519 derivation:\n  a=%s\n  b=%s", a.X25519PubHex(), b.X25519PubHex())
	}
	if a.X25519Priv() != b.X25519Priv() {
		t.Fatal("non-deterministic x25519 priv")
	}
}

// Different Ed25519 seeds must produce different X25519 keypairs — sanity
// check that the derivation is using the seed, not a constant.
func TestX25519DerivationDistinct(t *testing.T) {
	a := newIdentity(ed25519Seeded(t, "seed-alice                       "), nil, "")
	b := newIdentity(ed25519Seeded(t, "seed-bob                         "), nil, "")
	if a.X25519PubHex() == b.X25519PubHex() {
		t.Fatal("two different seeds produced the same x25519 pub")
	}
}

// X25519 clamp per RFC 7748 §5: low 3 bits zero, bit 254 set, bit 255 clear.
// If we ever lose the clamp, scalar multiplication may land outside the
// safe subgroup and handshakes can fail in subtle ways.
func TestX25519PrivClamped(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	priv := id.X25519Priv()
	if priv[0]&0b111 != 0 {
		t.Errorf("low 3 bits of priv[0] not zero: %#x", priv[0])
	}
	if priv[31]&0b10000000 != 0 {
		t.Errorf("bit 255 of priv[31] not zero: %#x", priv[31])
	}
	if priv[31]&0b01000000 == 0 {
		t.Errorf("bit 254 of priv[31] not set: %#x", priv[31])
	}
}

// X25519 pub must not be the all-zero identity point — that would only
// happen for a degenerate (zero) scalar.
func TestX25519PubNotZero(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	pub := id.X25519Pub()
	var zero [32]byte
	if bytes.Equal(pub[:], zero[:]) {
		t.Fatal("x25519 pub is all-zero")
	}
}

// Sanity: pub hex is 64 lowercase hex chars.
func TestX25519PubHexShape(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	h := id.X25519PubHex()
	if len(h) != 64 {
		t.Errorf("expected 64 chars, got %d (%q)", len(h), h)
	}
	if _, err := hex.DecodeString(h); err != nil {
		t.Errorf("not valid hex: %v", err)
	}
}

// Sign / verify roundtrip: a hello signed by Identity A verifies under
// A's pub. This is the basic crypto correctness check.
func TestHelloSignVerifyRoundtrip(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	sig := id.SignHello("alice")
	if !VerifyHello("alice", id.PubHex(), id.X25519PubHex(), sig) {
		t.Fatal("VerifyHello rejected a freshly-signed hello")
	}
}

// Each canonical-message field is bound by the signature — changing the
// name, ed pub, or wg pub at verify time invalidates it. This is what
// stops camp from MITM'ing by replacing wg_pub.
func TestHelloVerifyRejectsFieldSwap(t *testing.T) {
	id, _ := Generate()
	sig := id.SignHello("alice")
	other, _ := Generate()
	cases := []struct {
		label                 string
		name, pubHex, wgPubHex string
	}{
		{"wrong name", "bob", id.PubHex(), id.X25519PubHex()},
		{"wrong ed pub", "alice", other.PubHex(), id.X25519PubHex()},
		{"wrong wg pub", "alice", id.PubHex(), other.X25519PubHex()},
	}
	for _, c := range cases {
		if VerifyHello(c.name, c.pubHex, c.wgPubHex, sig) {
			t.Errorf("%s: accepted swap", c.label)
		}
	}
}

// Bad inputs (non-hex pub, wrong-size sig) must be rejected without
// panicking — these reach Verify from the network and must be safe.
func TestHelloVerifyRejectsMalformed(t *testing.T) {
	id, _ := Generate()
	sig := id.SignHello("alice")
	if VerifyHello("alice", "not-hex-pub", id.X25519PubHex(), sig) {
		t.Error("accepted non-hex pub")
	}
	if VerifyHello("alice", id.PubHex(), id.X25519PubHex(), []byte("too short")) {
		t.Error("accepted short sig")
	}
}

// The signed message must contain its domain-separation tag — without
// it, a hello signature could be replayed in another context that also
// signs over name|pub|wg_pub. Lock the format down with a literal.
func TestHelloCanonicalFormat(t *testing.T) {
	got := string(HelloCanonical("alice", "abc", "def"))
	want := "f2f-hello-v1|alice|abc|def"
	if got != want {
		t.Errorf("HelloCanonical = %q, want %q", got, want)
	}
}

// Names that could break the pipe-delimited canonical form (containing
// '|', control chars, empty, oversized) must be rejected.
func TestValidHelloName(t *testing.T) {
	good := []string{"alice", "user.name_42", "a-b-c", "x"}
	for _, n := range good {
		if !ValidHelloName(n) {
			t.Errorf("rejected good name %q", n)
		}
	}
	bad := []string{"", "with|pipe", "with\ttab", "with\x00null", string(make([]byte, 65))}
	for _, n := range bad {
		if ValidHelloName(n) {
			t.Errorf("accepted bad name %q", n)
		}
	}
}

// ed25519Seeded returns a PrivateKey deterministically from a 32-byte
// seed string. Pads/truncates to ed25519.SeedSize.
func ed25519Seeded(t *testing.T, seed string) ed25519.PrivateKey {
	t.Helper()
	s := make([]byte, ed25519.SeedSize)
	copy(s, seed)
	return ed25519.NewKeyFromSeed(s)
}
