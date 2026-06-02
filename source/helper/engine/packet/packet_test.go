package packet

import (
	"encoding/binary"
	"strings"
	"testing"
)

func TestSummary_IPv4(t *testing.T) {
	cases := []struct {
		name     string
		build    func() []byte
		contains []string
	}{
		{
			name: "ICMP",
			build: func() []byte {
				p := make([]byte, 28)
				p[0] = 0x45 // version 4, IHL 5
				binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
				p[9] = 1 // ICMP
				copy(p[12:16], []byte{10, 0, 0, 1})
				copy(p[16:20], []byte{1, 1, 1, 1})
				return p
			},
			contains: []string{"IPv4", "10.0.0.1", "1.1.1.1", "ICMP", "len=28"},
		},
		{
			name: "TCP",
			build: func() []byte {
				p := make([]byte, 40)
				p[0] = 0x45
				binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
				p[9] = 6 // TCP
				copy(p[12:16], []byte{192, 168, 1, 5})
				copy(p[16:20], []byte{142, 250, 0, 1})
				return p
			},
			contains: []string{"IPv4", "192.168.1.5", "142.250.0.1", "TCP"},
		},
		{
			name: "UDP",
			build: func() []byte {
				p := make([]byte, 28)
				p[0] = 0x45
				binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
				p[9] = 17 // UDP
				copy(p[12:16], []byte{10, 0, 0, 2})
				copy(p[16:20], []byte{8, 8, 8, 8})
				return p
			},
			contains: []string{"IPv4", "10.0.0.2", "8.8.8.8", "UDP"},
		},
		{
			name: "unknown proto",
			build: func() []byte {
				p := make([]byte, 28)
				p[0] = 0x45
				binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
				p[9] = 99
				copy(p[12:16], []byte{1, 2, 3, 4})
				copy(p[16:20], []byte{5, 6, 7, 8})
				return p
			},
			contains: []string{"IPv4", "proto=99"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Summary(c.build())
			for _, sub := range c.contains {
				if !strings.Contains(got, sub) {
					t.Errorf("missing %q in %q", sub, got)
				}
			}
		})
	}
}

func TestSummary_IPv6(t *testing.T) {
	p := make([]byte, 48)
	p[0] = 0x60 // version 6
	binary.BigEndian.PutUint16(p[4:6], 8)
	p[6] = 58 // ICMPv6
	copy(p[8:24], []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x1})
	copy(p[24:40], []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x2})

	got := Summary(p)
	for _, sub := range []string{"IPv6", "2001:db8::1", "2001:db8::2", "ICMPv6"} {
		if !strings.Contains(got, sub) {
			t.Errorf("missing %q in %q", sub, got)
		}
	}
}

func TestExtractDst(t *testing.T) {
	v4 := make([]byte, 28)
	v4[0] = 0x45
	binary.BigEndian.PutUint16(v4[2:4], 28)
	v4[9] = 6
	copy(v4[12:16], []byte{10, 0, 0, 1})
	copy(v4[16:20], []byte{8, 8, 8, 8})
	if got := ExtractDst(v4).String(); got != "8.8.8.8" {
		t.Errorf("v4 dst = %q, want 8.8.8.8", got)
	}

	v6 := make([]byte, 48)
	v6[0] = 0x60
	v6[6] = 58
	copy(v6[8:24], []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x1})
	copy(v6[24:40], []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x2})
	if got := ExtractDst(v6).String(); got != "2001:db8::2" {
		t.Errorf("v6 dst = %q, want 2001:db8::2", got)
	}

	if ExtractDst(nil).IsValid() {
		t.Error("ExtractDst(nil) should be invalid")
	}
	if ExtractDst([]byte{0x45}).IsValid() {
		t.Error("ExtractDst(truncated v4) should be invalid")
	}
}

func TestSummary_EdgeCases(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
		want string
	}{
		{"empty", []byte{}, "empty"},
		{"ipv4-truncated", []byte{0x45, 0x00}, "IPv4 truncated"},
		{"ipv6-truncated", []byte{0x60, 0x00}, "IPv6 truncated"},
		{"unknown-version", []byte{0x70, 0x00, 0x00, 0x00}, "unknown IP version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Summary(c.buf)
			if !strings.Contains(got, c.want) {
				t.Errorf("Summary(%v) = %q, want substring %q", c.buf, got, c.want)
			}
		})
	}
}
