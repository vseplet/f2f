package icmp

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestMakeEchoReply(t *testing.T) {
	// IPv4 (20 byte header, no options) + ICMP Echo Request (8 byte header) +
	// 4 bytes of payload.
	pkt := make([]byte, 20+8+4)
	pkt[0] = 0x45 // version 4, IHL 5
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(pkt)))
	pkt[8] = 64 // TTL
	pkt[9] = 1  // protocol = ICMP
	copy(pkt[12:16], []byte{10, 0, 0, 1})
	copy(pkt[16:20], []byte{1, 1, 1, 1})

	icmp := pkt[20:]
	icmp[0] = 8                                  // type = Echo Request
	binary.BigEndian.PutUint16(icmp[4:6], 0xbeef) // identifier
	binary.BigEndian.PutUint16(icmp[6:8], 0x0001) // sequence
	copy(icmp[8:], []byte{'p', 'i', 'n', 'g'})

	if !MakeEchoReply(pkt) {
		t.Fatal("MakeEchoReply returned false for a valid Echo Request")
	}
	if !bytes.Equal(pkt[12:16], []byte{1, 1, 1, 1}) {
		t.Errorf("src not swapped: %v", pkt[12:16])
	}
	if !bytes.Equal(pkt[16:20], []byte{10, 0, 0, 1}) {
		t.Errorf("dst not swapped: %v", pkt[16:20])
	}
	if pkt[20] != 0 {
		t.Errorf("icmp type not flipped: %d", pkt[20])
	}
	// Identifier, sequence, payload are preserved — these are what ping uses
	// to correlate the reply.
	if got := binary.BigEndian.Uint16(pkt[20+4 : 20+6]); got != 0xbeef {
		t.Errorf("identifier mangled: %#x", got)
	}
	if got := binary.BigEndian.Uint16(pkt[20+6 : 20+8]); got != 0x0001 {
		t.Errorf("sequence mangled: %#x", got)
	}
	if !bytes.Equal(pkt[20+8:], []byte{'p', 'i', 'n', 'g'}) {
		t.Errorf("payload mangled: %v", pkt[20+8:])
	}
	// The Internet checksum of a packet with a correct checksum field sums
	// to all-ones (0xffff).
	if s := folded(pkt[20:]); s != 0xffff {
		t.Errorf("ICMP checksum invalid: folded sum = %#x, want 0xffff", s)
	}
}

func TestMakeEchoReplyIgnoresNonEcho(t *testing.T) {
	// IPv4 + TCP-ish (protocol 6) — must not be rewritten.
	pkt := make([]byte, 40)
	pkt[0] = 0x45
	pkt[9] = 6 // TCP
	copy(pkt[12:16], []byte{10, 0, 0, 1})
	copy(pkt[16:20], []byte{1, 1, 1, 1})

	before := append([]byte(nil), pkt...)
	if MakeEchoReply(pkt) {
		t.Fatal("MakeEchoReply returned true for non-ICMP packet")
	}
	if !bytes.Equal(pkt, before) {
		t.Fatal("packet was mutated despite mismatch")
	}
}

func folded(buf []byte) uint32 {
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
	return sum
}
