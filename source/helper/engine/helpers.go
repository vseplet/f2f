package engine

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
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

// extractDst returns the destination address from a raw IPv4 or IPv6
// packet. Returns the zero Addr if the buffer is malformed or the
// version is not recognized — callers should treat that as "unknown,
// do not filter". Used by the tunToPeerLoop routing decision.
func extractDst(buf []byte) netip.Addr {
	if len(buf) == 0 {
		return netip.Addr{}
	}
	switch buf[0] >> 4 {
	case 4:
		if len(buf) < 20 {
			return netip.Addr{}
		}
		a, _ := netip.AddrFromSlice(buf[16:20])
		return a
	case 6:
		if len(buf) < 40 {
			return netip.Addr{}
		}
		a, _ := netip.AddrFromSlice(buf[24:40])
		return a
	}
	return netip.Addr{}
}

// packetSummary returns a one-line description of the first IP
// packet in buf for log lines. Intentionally not a full decoder — no
// TCP/UDP port extraction, no option handling — to keep the hot path
// cheap.
func packetSummary(buf []byte) string {
	if len(buf) == 0 {
		return "empty"
	}
	switch buf[0] >> 4 {
	case 4:
		return packetSummaryV4(buf)
	case 6:
		return packetSummaryV6(buf)
	default:
		return fmt.Sprintf("unknown IP version 0x%x (%d bytes)", buf[0]>>4, len(buf))
	}
}

func packetSummaryV4(buf []byte) string {
	if len(buf) < 20 {
		return fmt.Sprintf("IPv4 truncated (%d bytes)", len(buf))
	}
	totalLen := binary.BigEndian.Uint16(buf[2:4])
	proto := buf[9]
	src, _ := netip.AddrFromSlice(buf[12:16])
	dst, _ := netip.AddrFromSlice(buf[16:20])
	return fmt.Sprintf("IPv4 %s → %s %s len=%d", src, dst, packetProtoName(proto), totalLen)
}

func packetSummaryV6(buf []byte) string {
	if len(buf) < 40 {
		return fmt.Sprintf("IPv6 truncated (%d bytes)", len(buf))
	}
	payloadLen := binary.BigEndian.Uint16(buf[4:6])
	next := buf[6]
	src, _ := netip.AddrFromSlice(buf[8:24])
	dst, _ := netip.AddrFromSlice(buf[24:40])
	return fmt.Sprintf("IPv6 %s → %s %s payload=%d", src, dst, packetProtoName(next), payloadLen)
}

func packetProtoName(p byte) string {
	switch p {
	case 1:
		return "ICMP"
	case 6:
		return "TCP"
	case 17:
		return "UDP"
	case 58:
		return "ICMPv6"
	default:
		return fmt.Sprintf("proto=%d", p)
	}
}
