// Package packet parses just enough of an IP header to produce a one-line
// log summary. It is intentionally not a full decoder — no TCP/UDP port
// extraction, no option handling — to keep the hot path cheap.
package packet

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// ExtractDst returns the destination address from a raw IPv4 or IPv6 packet.
// Returns the zero Addr if the buffer is malformed or the version is not
// recognized — callers should treat that as "unknown, do not filter".
func ExtractDst(buf []byte) netip.Addr {
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

// Summary returns a human-readable description of the first IP packet in buf,
// or a short diagnostic if the buffer is malformed.
func Summary(buf []byte) string {
	if len(buf) == 0 {
		return "empty"
	}
	switch buf[0] >> 4 {
	case 4:
		return summaryV4(buf)
	case 6:
		return summaryV6(buf)
	default:
		return fmt.Sprintf("unknown IP version 0x%x (%d bytes)", buf[0]>>4, len(buf))
	}
}

func summaryV4(buf []byte) string {
	if len(buf) < 20 {
		return fmt.Sprintf("IPv4 truncated (%d bytes)", len(buf))
	}
	totalLen := binary.BigEndian.Uint16(buf[2:4])
	proto := buf[9]
	src, _ := netip.AddrFromSlice(buf[12:16])
	dst, _ := netip.AddrFromSlice(buf[16:20])
	return fmt.Sprintf("IPv4 %s → %s %s len=%d", src, dst, protoName(proto), totalLen)
}

func summaryV6(buf []byte) string {
	if len(buf) < 40 {
		return fmt.Sprintf("IPv6 truncated (%d bytes)", len(buf))
	}
	payloadLen := binary.BigEndian.Uint16(buf[4:6])
	next := buf[6]
	src, _ := netip.AddrFromSlice(buf[8:24])
	dst, _ := netip.AddrFromSlice(buf[24:40])
	return fmt.Sprintf("IPv6 %s → %s %s payload=%d", src, dst, protoName(next), payloadLen)
}

func protoName(p byte) string {
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
