// Package pair implements the request/response handshake that pairs two
// camp members together. A successful pair gives both sides three
// things in one round-trip:
//
//  1. NAT-traversal — each pair_req is a UDP packet that opens NAT.
//  2. Identity attestation — both packets carry Ed25519-signed
//     (name, pub, wg_pub) so the receiver knows who they're talking to.
//  3. RTT measurement — pair_res echoes the original pair_req's
//     sent_ms; receiver of pair_res computes RTT = now - echo_ms.
//
// A peer is considered "paired" when (a) we've seen a valid pair_req
// from them recently AND (b) our own pair_req has been answered with a
// valid pair_res recently. Both conditions together mean we have
// bidirectional crypto-verified liveness — strictly stronger than the
// old separate "hello received" + "pinger pong received" signals.
//
// Wire format (inside an obfenv control-envelope, never plaintext):
//
//	pair_req: {"t":"pair_req","name":..,"pub":..,"wg_pub":..,"sent_ms":N,"sig":..}
//	pair_res: {"t":"pair_res","name":..,"pub":..,"wg_pub":..,"sent_ms":N,"echo_ms":M,"sig":..}
//
// Cadence:
//   - pair_req shipped from holePunchLoop on the same schedule as the
//     old hole-punch (1Hz burst → 25s keepalive).
//   - pair_res shipped immediately (fire-on-receive) whenever a valid
//     pair_req arrives. Not scheduled — purely reactive.
package pair

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/vseplet/f2f/source/helper/identity"
)

// Wire-level "t" values. Mismatch between expected type and packet
// type is a parse-failure; type also discriminates which canonical
// message form the signature covers.
const (
	TypeReq = "pair_req"
	TypeRes = "pair_res"
)

// Domain-separation tags for the signed canonical messages. Bumping
// the suffix invalidates all in-flight signatures of that variant; the
// counterpart variant (e.g. req while res rotates) keeps working.
const (
	domainReq = "f2f-pair-req-v1"
	domainRes = "f2f-pair-res-v1"
)

// ErrInvalidName is returned by Build* when name fails validation. We
// fail loudly rather than ship an unverifiable packet — caller bug.
var ErrInvalidName = errors.New("pair: invalid name (must satisfy identity.ValidPeerName)")

// Req is the parsed pair_req JSON payload. All fields are guaranteed
// well-formed after a successful ParseReq.
type Req struct {
	T      string `json:"t"`
	Name   string `json:"name"`
	Pub    string `json:"pub"`     // Ed25519 pub hex (64 chars)
	WGPub  string `json:"wg_pub"`  // X25519 pub hex (64 chars)
	SentMs int64  `json:"sent_ms"` // sender's clock at send time
	Sig    string `json:"sig"`     // Ed25519 signature hex (128 chars)
}

// Res is the parsed pair_res JSON payload. EchoMs is the responder's
// echo of the original pair_req's SentMs — used by the original sender
// to compute round-trip time.
type Res struct {
	T      string `json:"t"`
	Name   string `json:"name"`
	Pub    string `json:"pub"`
	WGPub  string `json:"wg_pub"`
	SentMs int64  `json:"sent_ms"` // responder's clock at send time
	EchoMs int64  `json:"echo_ms"` // echo of the triggering pair_req's SentMs
	Sig    string `json:"sig"`
}

// Type peeks at the "t" field of a hello-shaped JSON payload without
// committing to either Req or Res parsing. Callers use it to route to
// the right Parse* function. Returns "" if the payload isn't even
// JSON-shaped or has no "t" field.
func Type(plaintext []byte) string {
	var probe struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal(plaintext, &probe); err != nil {
		return ""
	}
	return probe.T
}

// BuildReq constructs a signed pair_req. sentMs is typically
// time.Now().UnixMilli() at the moment of build. The signed canonical
// form is `f2f-pair-req-v1|name|pub|wg_pub|sent_ms`.
func BuildReq(id *identity.Identity, name string, sentMs int64) ([]byte, error) {
	if !identity.ValidPeerName(name) {
		return nil, ErrInvalidName
	}
	sig := id.Sign(canonReq(name, id.PubHex(), id.X25519PubHex(), sentMs))
	return json.Marshal(Req{
		T:      TypeReq,
		Name:   name,
		Pub:    id.PubHex(),
		WGPub:  id.X25519PubHex(),
		SentMs: sentMs,
		Sig:    hex.EncodeToString(sig),
	})
}

// BuildRes constructs a signed pair_res. echoMs MUST be copied verbatim
// from the triggering pair_req's SentMs — that's what lets the original
// requester compute RTT. The signed canonical form is
// `f2f-pair-res-v1|name|pub|wg_pub|sent_ms|echo_ms`.
func BuildRes(id *identity.Identity, name string, sentMs, echoMs int64) ([]byte, error) {
	if !identity.ValidPeerName(name) {
		return nil, ErrInvalidName
	}
	sig := id.Sign(canonRes(name, id.PubHex(), id.X25519PubHex(), sentMs, echoMs))
	return json.Marshal(Res{
		T:      TypeRes,
		Name:   name,
		Pub:    id.PubHex(),
		WGPub:  id.X25519PubHex(),
		SentMs: sentMs,
		EchoMs: echoMs,
		Sig:    hex.EncodeToString(sig),
	})
}

// ParseReq decodes and verifies a pair_req. Returns ok=true only when:
//   - JSON parses cleanly
//   - "t" field equals "pair_req"
//   - name passes identity.ValidPeerName
//   - pub/wg_pub are 64-char hex
//   - sig is 64-byte (128-char hex) Ed25519 signature
//   - signature verifies against canonReq(name, pub, wg_pub, sent_ms)
//
// On ok=true, the holder of Pub's Ed25519 priv key is attested as the
// author of this packet. Membership in camp is the engine's check.
func ParseReq(plaintext []byte) (Req, bool) {
	var p Req
	if err := json.Unmarshal(plaintext, &p); err != nil {
		return Req{}, false
	}
	if p.T != TypeReq {
		return Req{}, false
	}
	if !identity.ValidPeerName(p.Name) {
		return Req{}, false
	}
	if len(p.Pub) != 64 || len(p.WGPub) != 64 {
		return Req{}, false
	}
	sigBytes, err := hex.DecodeString(p.Sig)
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return Req{}, false
	}
	pubBytes, err := hex.DecodeString(p.Pub)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return Req{}, false
	}
	if !ed25519.Verify(ed25519.PublicKey(pubBytes), canonReq(p.Name, p.Pub, p.WGPub, p.SentMs), sigBytes) {
		return Req{}, false
	}
	return p, true
}

// ParseRes decodes and verifies a pair_res. Same guarantees as ParseReq
// plus EchoMs is included in the signed canonical form, so a responder
// cannot stamp a fake echo onto a previously-captured signature.
func ParseRes(plaintext []byte) (Res, bool) {
	var p Res
	if err := json.Unmarshal(plaintext, &p); err != nil {
		return Res{}, false
	}
	if p.T != TypeRes {
		return Res{}, false
	}
	if !identity.ValidPeerName(p.Name) {
		return Res{}, false
	}
	if len(p.Pub) != 64 || len(p.WGPub) != 64 {
		return Res{}, false
	}
	sigBytes, err := hex.DecodeString(p.Sig)
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return Res{}, false
	}
	pubBytes, err := hex.DecodeString(p.Pub)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return Res{}, false
	}
	if !ed25519.Verify(ed25519.PublicKey(pubBytes), canonRes(p.Name, p.Pub, p.WGPub, p.SentMs, p.EchoMs), sigBytes) {
		return Res{}, false
	}
	return p, true
}

func canonReq(name, pubHex, wgPubHex string, sentMs int64) []byte {
	return []byte(domainReq + "|" + name + "|" + pubHex + "|" + wgPubHex + "|" + strconv.FormatInt(sentMs, 10))
}

func canonRes(name, pubHex, wgPubHex string, sentMs, echoMs int64) []byte {
	return []byte(domainRes + "|" + name + "|" + pubHex + "|" + wgPubHex + "|" + strconv.FormatInt(sentMs, 10) + "|" + strconv.FormatInt(echoMs, 10))
}
