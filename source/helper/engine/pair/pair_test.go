package pair

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/vseplet/f2f/source/helper/identity"
)

// Round-trip: Build then Parse must yield the same fields back, and
// signature must verify.
func TestBuildParseReqRoundtrip(t *testing.T) {
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := BuildReq(id, "alice", 1735000000123)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := ParseReq(raw)
	if !ok {
		t.Fatal("ParseReq rejected freshly-built pair_req")
	}
	if p.T != TypeReq {
		t.Errorf("T = %q", p.T)
	}
	if p.Name != "alice" {
		t.Errorf("Name = %q", p.Name)
	}
	if p.Pub != id.PubHex() {
		t.Errorf("Pub mismatch")
	}
	if p.WGPub != id.X25519PubHex() {
		t.Errorf("WGPub mismatch")
	}
	if p.SentMs != 1735000000123 {
		t.Errorf("SentMs = %d", p.SentMs)
	}
}

func TestBuildParseResRoundtrip(t *testing.T) {
	id, _ := identity.Generate()
	raw, err := BuildRes(id, "bob", 1735000000456, 1735000000123)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := ParseRes(raw)
	if !ok {
		t.Fatal("ParseRes rejected freshly-built pair_res")
	}
	if p.SentMs != 1735000000456 {
		t.Errorf("SentMs = %d", p.SentMs)
	}
	if p.EchoMs != 1735000000123 {
		t.Errorf("EchoMs = %d — this is the RTT-measurement field, MUST roundtrip exactly", p.EchoMs)
	}
}

// Type discriminator must work without committing to a full parse,
// since the engine uses it to route to the right handler.
func TestTypeDiscrimination(t *testing.T) {
	id, _ := identity.Generate()
	req, _ := BuildReq(id, "alice", 1)
	res, _ := BuildRes(id, "alice", 1, 0)
	if got := Type(req); got != TypeReq {
		t.Errorf("Type(req) = %q, want %q", got, TypeReq)
	}
	if got := Type(res); got != TypeRes {
		t.Errorf("Type(res) = %q, want %q", got, TypeRes)
	}
	if got := Type([]byte("not json")); got != "" {
		t.Errorf("Type(garbage) = %q, want \"\"", got)
	}
	if got := Type([]byte(`{"x":1}`)); got != "" {
		t.Errorf("Type(no-t) = %q, want \"\"", got)
	}
}

// Calling the wrong Parse for the packet type must fail — pair_req
// JSON should not be accepted by ParseRes and vice versa, even if the
// underlying signature is otherwise valid.
func TestParseRejectsTypeMismatch(t *testing.T) {
	id, _ := identity.Generate()
	req, _ := BuildReq(id, "alice", 1)
	res, _ := BuildRes(id, "alice", 1, 0)
	if _, ok := ParseRes(req); ok {
		t.Error("ParseRes accepted a pair_req")
	}
	if _, ok := ParseReq(res); ok {
		t.Error("ParseReq accepted a pair_res")
	}
}

// Domain separation: a Req signature must not validate against the
// Res canonical message, even though they share fields. Without
// distinct tags an attacker could replay a captured Req as a Res.
func TestDomainSeparationReqVsRes(t *testing.T) {
	id, _ := identity.Generate()
	req, _ := BuildReq(id, "alice", 1)
	// Surgically rewrite "pair_req" → "pair_res" in the JSON, keeping
	// the same signature. ParseRes should reject because the Res
	// canonical form differs from what the sig covers.
	tampered := bytes.Replace(req, []byte(`"`+TypeReq+`"`), []byte(`"`+TypeRes+`"`), 1)
	if _, ok := ParseRes(tampered); ok {
		t.Fatal("ParseRes accepted a Req signature after t-relabel — domain tags not separating")
	}
}

// Tampering with sent_ms or echo_ms breaks the signature. These are
// the timing-sensitive fields — if they could be tampered in transit,
// RTT measurements would be unreliable.
func TestParseRejectsTimingTampering(t *testing.T) {
	id, _ := identity.Generate()
	res, _ := BuildRes(id, "alice", 1000, 500)
	// Flip echo_ms from 500 to 501 — any change should break the sig.
	tampered := bytes.Replace(res, []byte(`"echo_ms":500`), []byte(`"echo_ms":501`), 1)
	if _, ok := ParseRes(tampered); ok {
		t.Fatal("ParseRes accepted res with tampered echo_ms")
	}
}

// Tampering with wg_pub must fail — that's the whole point of this
// transport-key attestation. Camp-MITM defense.
func TestParseRejectsWGPubTampering(t *testing.T) {
	a, _ := identity.Generate()
	b, _ := identity.Generate()
	req, _ := BuildReq(a, "alice", 1)
	tampered := bytes.ReplaceAll(req,
		[]byte(`"`+a.X25519PubHex()+`"`),
		[]byte(`"`+b.X25519PubHex()+`"`))
	if _, ok := ParseReq(tampered); ok {
		t.Fatal("ParseReq accepted req with tampered wg_pub")
	}
}

// Invalid names must be refused at Build time — better than shipping
// an unverifiable packet (Parse on the other side would just drop it).
func TestBuildRejectsInvalidName(t *testing.T) {
	id, _ := identity.Generate()
	if _, err := BuildReq(id, "", 1); err == nil {
		t.Error("BuildReq accepted empty name")
	}
	if _, err := BuildReq(id, "with|pipe", 1); err == nil {
		t.Error("BuildReq accepted name with pipe")
	}
	if _, err := BuildRes(id, "with\x00null", 1, 0); err == nil {
		t.Error("BuildRes accepted name with null byte")
	}
}

// Garbage from the wire must produce ok=false without panic.
func TestParseRejectsGarbage(t *testing.T) {
	for _, in := range [][]byte{
		nil,
		[]byte{},
		[]byte("{this is not json"),
		[]byte(`{}`),
		[]byte(`{"t":"pair_req"}`), // missing required fields
		[]byte(`{"t":"pair_req","name":"x","pub":"00","wg_pub":"00","sent_ms":0,"sig":"00"}`),
	} {
		if _, ok := ParseReq(in); ok {
			t.Errorf("ParseReq accepted garbage: %q", in)
		}
	}
}

// Canonical message is part of the wire-format contract — locking it
// down with a literal so future refactors can't silently change what
// gets signed.
func TestCanonicalReqFormat(t *testing.T) {
	got := string(canonReq("alice", "abc", "def", 123))
	want := "f2f-pair-req-v1|alice|abc|def|123"
	if got != want {
		t.Errorf("canonReq = %q, want %q", got, want)
	}
}

func TestCanonicalResFormat(t *testing.T) {
	got := string(canonRes("alice", "abc", "def", 123, 456))
	want := "f2f-pair-res-v1|alice|abc|def|123|456"
	if got != want {
		t.Errorf("canonRes = %q, want %q", got, want)
	}
}

// Sanity: sent_ms with extreme values (negative, max int64) must
// roundtrip — strconv.FormatInt handles both, but make sure nothing
// in our JSON layer drops precision.
func TestSentMsExtremeRoundtrip(t *testing.T) {
	id, _ := identity.Generate()
	for _, ts := range []int64{0, 1, -1, 1 << 62, -(1 << 62)} {
		raw, err := BuildReq(id, "x", ts)
		if err != nil {
			t.Fatal(err)
		}
		p, ok := ParseReq(raw)
		if !ok {
			t.Errorf("ParseReq failed for sent_ms=%d (%s)", ts, strconv.FormatInt(ts, 10))
			continue
		}
		if p.SentMs != ts {
			t.Errorf("sent_ms roundtrip: got %d, want %d", p.SentMs, ts)
		}
	}
}
