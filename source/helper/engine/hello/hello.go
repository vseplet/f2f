// Package hello implements the peer-to-peer handshake packet that
// carries authenticated identity (Ed25519) and transport pub (X25519)
// between two camp members.
//
// On the wire each hello is a JSON object:
//
//	{"t":"hello","name":"alice","pub":"<ed_hex>","wg_pub":"<x_hex>","sig":"<sig_hex>"}
//
// The Ed25519 signature covers identity.HelloCanonical(name, pub, wg_pub).
// Bytes outside the signed fields (JSON whitespace, field order) are not
// authenticated — they can change without breaking verification.
//
// Hello is always wrapped in an obfenv.Camp envelope (SlotHello) before
// hitting the network; the JSON here is just the envelope's plaintext.
// Package hello does not know about envelopes — it produces and consumes
// the inner payload only.
//
// Wire role: hello replaces the legacy 1-byte (0x00) hole-punch packet.
// Sending a hello opens NAT on both ends AND attests identity+WGPub in
// one round trip — see ARCHITECTURE.md "Hello, NAT и AWG handshake".
package hello

import (
	"encoding/hex"
	"encoding/json"

	"github.com/vseplet/f2f/source/helper/identity"
)

// Type is the t-field value for hello packets. Distinguishes hello from
// other JSON-shaped control payloads we might add later (ping, etc.).
const Type = "hello"

// Packet is the parsed inner JSON of a hello. All fields are present
// after a successful Parse — partially-populated packets cannot make it
// past signature verification.
type Packet struct {
	T     string `json:"t"`      // always Type = "hello"
	Name  string `json:"name"`   // display alias, ValidHelloName-shaped
	Pub   string `json:"pub"`    // Ed25519 pub hex (64 chars)
	WGPub string `json:"wg_pub"` // X25519 pub hex (64 chars)
	Sig   string `json:"sig"`    // Ed25519 signature hex (128 chars)
}

// Build constructs a hello, signs it with id, and returns the JSON
// bytes ready to be passed to obfenv.Camp.Seal(obfenv.SlotHello, ...).
// name must satisfy identity.ValidHelloName — callers should validate
// at the source (e.g. on camp join), but Build also bails out here so
// no unsignable hello ever leaves the box.
func Build(id *identity.Identity, name string) ([]byte, error) {
	if !identity.ValidHelloName(name) {
		return nil, &nameError{name: name}
	}
	pkt := Packet{
		T:     Type,
		Name:  name,
		Pub:   id.PubHex(),
		WGPub: id.X25519PubHex(),
		Sig:   hex.EncodeToString(id.SignHello(name)),
	}
	return json.Marshal(pkt)
}

// Parse decodes hello JSON and verifies the signature. ok=false if:
//   - JSON parse fails
//   - "t" field is not "hello"
//   - name does not satisfy ValidHelloName
//   - pub/wg_pub are not 64-char hex
//   - sig is not valid hex of length 128
//   - the signature does not verify against pub
//
// On ok=true, every field of Packet is well-formed AND the packet was
// signed by the holder of the Ed25519 private key matching Packet.Pub.
//
// Parse does NOT check that Packet.Pub is a member of the camp — that's
// the engine's job (lookup in e.peers by pub). Parse only proves
// cryptographic authenticity, not authorization.
func Parse(plaintext []byte) (Packet, bool) {
	var pkt Packet
	if err := json.Unmarshal(plaintext, &pkt); err != nil {
		return Packet{}, false
	}
	if pkt.T != Type {
		return Packet{}, false
	}
	if !identity.ValidHelloName(pkt.Name) {
		return Packet{}, false
	}
	if len(pkt.Pub) != 64 || len(pkt.WGPub) != 64 {
		return Packet{}, false
	}
	sig, err := hex.DecodeString(pkt.Sig)
	if err != nil {
		return Packet{}, false
	}
	if !identity.VerifyHello(pkt.Name, pkt.Pub, pkt.WGPub, sig) {
		return Packet{}, false
	}
	return pkt, true
}

type nameError struct{ name string }

func (e *nameError) Error() string {
	return "hello: invalid name (must be 1..64 printable ASCII, no '|'): " + e.name
}
