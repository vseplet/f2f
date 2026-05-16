// Package icmp builds ICMP responses by rewriting incoming packets in place.
// Only IPv4 ICMP Echo Request → Echo Reply is supported right now.
package icmp

import (
	"encoding/binary"
)

// MakeEchoReply rewrites an IPv4 ICMP Echo Request packet in place into the
// corresponding Echo Reply. Returns true if the packet matched and was
// rewritten, false if it wasn't an IPv4 ICMP Echo Request (in which case pkt
// is left untouched).
func MakeEchoReply(pkt []byte) bool {
	if len(pkt) < 20 {
		return false
	}
	if pkt[0]>>4 != 4 {
		return false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl+8 {
		return false
	}
	if pkt[9] != 1 { // protocol != ICMP
		return false
	}
	icmp := pkt[ihl:]
	if icmp[0] != 8 { // not Echo Request
		return false
	}

	// Swap source and destination IPs. The IPv4 header checksum is a one's-
	// complement sum of the header's 16-bit words; swapping two 32-bit fields
	// just reorders terms in that sum, so the checksum stays valid.
	var tmp [4]byte
	copy(tmp[:], pkt[12:16])
	copy(pkt[12:16], pkt[16:20])
	copy(pkt[16:20], tmp[:])

	// Flip ICMP type Echo Request (8) → Echo Reply (0). Code stays 0.
	// Identifier, sequence number, and payload are preserved verbatim — that
	// is how ping matches the reply to its outstanding request.
	icmp[0] = 0
	icmp[2], icmp[3] = 0, 0
	binary.BigEndian.PutUint16(icmp[2:4], checksum16(icmp))
	return true
}

// checksum16 computes the 16-bit one's-complement Internet checksum (RFC 1071).
func checksum16(buf []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(buf); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(buf[i : i+2]))
	}
	if len(buf)%2 == 1 {
		sum += uint32(buf[len(buf)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
