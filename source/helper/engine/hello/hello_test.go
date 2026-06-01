package hello

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/vseplet/f2f/source/helper/identity"
)

// Build then Parse must yield the same fields back. Basic roundtrip.
func TestBuildParseRoundtrip(t *testing.T) {
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Build(id, "alice")
	if err != nil {
		t.Fatal(err)
	}
	pkt, ok := Parse(raw)
	if !ok {
		t.Fatal("Parse rejected a freshly-built hello")
	}
	if pkt.T != Type {
		t.Errorf("T = %q, want %q", pkt.T, Type)
	}
	if pkt.Name != "alice" {
		t.Errorf("Name = %q", pkt.Name)
	}
	if pkt.Pub != id.PubHex() {
		t.Errorf("Pub mismatch")
	}
	if pkt.WGPub != id.X25519PubHex() {
		t.Errorf("WGPub mismatch")
	}
}

// Build rejects names that can't be safely embedded in the canonical
// signature form — better to fail early than send unverifiable hellos.
func TestBuildRejectsInvalidName(t *testing.T) {
	id, _ := identity.Generate()
	if _, err := Build(id, ""); err == nil {
		t.Error("accepted empty name")
	}
	if _, err := Build(id, "with|pipe"); err == nil {
		t.Error("accepted name with pipe")
	}
}

// Tampering with the name in the JSON breaks signature verification.
// This is the critical property — camp cannot rename a peer's hello.
func TestParseRejectsNameTampering(t *testing.T) {
	id, _ := identity.Generate()
	raw, _ := Build(id, "alice")
	tampered := bytes.ReplaceAll(raw, []byte(`"alice"`), []byte(`"malic"`))
	if _, ok := Parse(tampered); ok {
		t.Fatal("Parse accepted hello with swapped name")
	}
}

// Tampering with wg_pub breaks the signature too — this is what stops
// camp from MITM'ing transport keys.
func TestParseRejectsWGPubTampering(t *testing.T) {
	id, _ := identity.Generate()
	other, _ := identity.Generate()
	raw, _ := Build(id, "alice")
	tampered := bytes.ReplaceAll(raw,
		[]byte(`"`+id.X25519PubHex()+`"`),
		[]byte(`"`+other.X25519PubHex()+`"`))
	if _, ok := Parse(tampered); ok {
		t.Fatal("Parse accepted hello with swapped wg_pub")
	}
}

// Garbage and adjacent JSON shapes (different "t", missing fields) must
// be rejected cleanly without exposing partial Packet state.
func TestParseRejectsBadShapes(t *testing.T) {
	cases := []struct {
		label string
		in    string
	}{
		{"not json", "{this is not json"},
		{"wrong t", `{"t":"ping","name":"x","pub":"00","wg_pub":"00","sig":"00"}`},
		{"empty json", `{}`},
		{"missing sig", `{"t":"hello","name":"a","pub":"00","wg_pub":"00"}`},
	}
	for _, c := range cases {
		if pkt, ok := Parse([]byte(c.in)); ok {
			t.Errorf("%s: accepted (pkt=%+v)", c.label, pkt)
		}
	}
}

// Hello carries the X25519 pubkey — that's the whole point. Make sure
// it survives JSON encoding without truncation/case-mangling.
func TestParseRevealsWGPub(t *testing.T) {
	id, _ := identity.Generate()
	raw, _ := Build(id, "alice")
	pkt, ok := Parse(raw)
	if !ok {
		t.Fatal("Parse failed")
	}
	if len(pkt.WGPub) != 64 {
		t.Fatalf("WGPub len = %d, want 64", len(pkt.WGPub))
	}
	if pkt.WGPub != id.X25519PubHex() {
		t.Fatalf("WGPub = %s, want %s", pkt.WGPub, id.X25519PubHex())
	}
}

// JSON field re-ordering by Marshal across Go versions must not break
// Parse. Build the JSON manually in scrambled order and verify it still
// parses + verifies.
func TestParseAcceptsAnyFieldOrder(t *testing.T) {
	id, _ := identity.Generate()
	// Build canonical first so we have a valid signature
	canon, _ := Build(id, "alice")
	var pkt Packet
	if err := json.Unmarshal(canon, &pkt); err != nil {
		t.Fatal(err)
	}
	// Re-marshal with a custom field order via a different struct
	scrambled, err := json.Marshal(struct {
		Sig   string `json:"sig"`
		WGPub string `json:"wg_pub"`
		Pub   string `json:"pub"`
		Name  string `json:"name"`
		T     string `json:"t"`
	}{pkt.Sig, pkt.WGPub, pkt.Pub, pkt.Name, pkt.T})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := Parse(scrambled); !ok {
		t.Fatal("Parse rejected scrambled-order hello")
	}
}
