// Package overlay derives per-camp IPv6 addresses from (camp_id, pub)
// so peers compute the same address for the same identity without any
// allocator on the camp server. Layout (RFC 4193 ULA):
//
//	byte 0:     0xfd                              (ULA marker)
//	bytes 1-5:  first 5 bytes of sha256(camp_id)  (per-camp /48 prefix)
//	bytes 6-7:  0x00 0x00                         (subnet ID, reserved)
//	bytes 8-15: first 8 bytes of sha256(pub_raw)  (per-pub host part)
//
// Host part = identity.FingerprintHex(pubHex) by design — eyeballing
// the v6 address and the fp pill in the UI is a sanity check.
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

// PubToAddr returns the overlay IPv6 address for (campID, pubHex).
// pubHex must be 64 hex chars (32-byte Ed25519 pub).
func PubToAddr(campID, pubHex string) (netip.Addr, error) {
	pubRaw, err := hex.DecodeString(pubHex)
	if err != nil {
		return netip.Addr{}, err
	}
	if len(pubRaw) != 32 {
		return netip.Addr{}, errors.New("overlay: pub must be 32 bytes")
	}
	campHash := sha256.Sum256([]byte(campID))
	pubHash := sha256.Sum256(pubRaw)
	var b [16]byte
	b[0] = 0xfd
	copy(b[1:6], campHash[:5])
	// b[6:8] = 0, reserved subnet ID.
	copy(b[8:16], pubHash[:8])
	return netip.AddrFrom16(b), nil
}

// CampPrefix returns the /48 ULA prefix that every peer in this camp
// shares. Useful for route installs once we move the utun off v4.
func CampPrefix(campID string) netip.Prefix {
	campHash := sha256.Sum256([]byte(campID))
	var b [16]byte
	b[0] = 0xfd
	copy(b[1:6], campHash[:5])
	return netip.PrefixFrom(netip.AddrFrom16(b), 48)
}
