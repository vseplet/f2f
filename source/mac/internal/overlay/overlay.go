// Package overlay derives deterministic per-peer IPv4 addresses from
// the peer's Ed25519 public key so every node in a camp computes the
// same address for the same identity without any allocator on the
// camp server.
package overlay

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/netip"
)

// V4Subnet is the IPv4 space the overlay carves up for per-peer
// aliases. 100.64.0.0/10 is the RFC 6598 "Shared Address Space"
// (CGNAT), unlikely to clash with anything users run on their LAN.
// Tailscale uses the same range for the same reason.
const V4Subnet = "100.64.0.0/10"

// PubToV4Addr returns a deterministic 100.64.X.Y address for the
// peer. Used as the local v4 utun alias so each mac has a unique
// landing pad for intercept egress (a shared address would collide
// when the egress peer NATs the reply back: dst would be its own
// utun alias, the kernel would deliver locally and the originator
// never sees it).
//
// Address layout fills the host part of 100.64.0.0/10 from the pub
// hash: high 6 bits of the 2nd byte stay 0b01 (kept inside the /10),
// the rest comes from sha256(pub_raw). Birthday collisions for two
// peers in one camp are ~1% at 250 peers — cosmetic; the address
// never crosses the wire, it's just a local routing label.
func PubToV4Addr(pubHex string) (netip.Addr, error) {
	pubRaw, err := hex.DecodeString(pubHex)
	if err != nil {
		return netip.Addr{}, err
	}
	if len(pubRaw) != 32 {
		return netip.Addr{}, errors.New("overlay: pub must be 32 bytes")
	}
	h := sha256.Sum256(pubRaw)
	// 100.64.0.0/10: first byte = 100, top 2 bits of second byte = 01.
	b1 := (h[0] & 0x3f) | 0x40
	return netip.AddrFrom4([4]byte{100, b1, h[1], h[2]}), nil
}
