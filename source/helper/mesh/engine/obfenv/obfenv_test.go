package obfenv

import (
	"bytes"
	"testing"
)

func TestSealOpenRoundtrip(t *testing.T) {
	c := NewCamp("test-camp-id")
	want := []byte(`{"t":"hello","name":"alice"}`)
	sealed, err := c.Seal(SlotHello, want)
	if err != nil {
		t.Fatal(err)
	}
	got, slot, ok := c.Open(sealed)
	if !ok {
		t.Fatal("Open ok=false on valid envelope")
	}
	if slot != SlotHello {
		t.Errorf("slot = %d, want %d", slot, SlotHello)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("plaintext mismatch:\n got %q\nwant %q", got, want)
	}
}

// Different camp_ids derive different keys, so an envelope sealed by
// camp A cannot be opened by camp B even if magic happens to coincide.
func TestOpenRejectsForeignKey(t *testing.T) {
	a := NewCamp("camp-alpha")
	b := NewCamp("camp-beta")
	sealed, err := a.Seal(SlotHello, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := b.Open(sealed); ok {
		t.Fatal("Open accepted envelope from foreign camp_id")
	}
}

// Tampering anywhere in the ciphertext must break the auth tag. This is
// the difference between AEAD and plain stream-cipher obfuscation —
// without the tag, an attacker with camp_key could mutate hello bytes
// in transit (e.g. swap wg_pub) and the receiver wouldn't notice.
func TestOpenRejectsTampering(t *testing.T) {
	c := NewCamp("test")
	sealed, err := c.Seal(SlotHello, []byte("hello there"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), sealed...)
	tampered[OverheadSize-1] ^= 0x01 // flip a tag bit
	if _, _, ok := c.Open(tampered); ok {
		t.Fatal("Open accepted tampered envelope")
	}
}

// Random garbage from the network must drop without spending AEAD CPU
// in the magic-out-of-range case.
func TestOpenRejectsShortAndGarbage(t *testing.T) {
	c := NewCamp("test")
	if _, _, ok := c.Open([]byte("x")); ok {
		t.Fatal("Open accepted 1-byte packet")
	}
	garbage := make([]byte, OverheadSize+10)
	// magic = 0 — way outside our 0x80000000..0xC0000000 band
	if _, _, ok := c.Open(garbage); ok {
		t.Fatal("Open accepted zero-magic packet")
	}
}

// Sealing into an AWG slot is a usage error — AWG handles its own
// crypto, we must not double-wrap.
func TestSealRejectsAWGSlots(t *testing.T) {
	c := NewCamp("test")
	for _, slot := range []Slot{SlotAWGInit, SlotAWGResponse, SlotAWGCookie, SlotAWGTransport} {
		if _, err := c.Seal(slot, []byte("x")); err == nil {
			t.Errorf("Seal slot=%d returned no error, want one", slot)
		}
	}
}

// Even if an attacker forges a packet whose magic lands in an AWG slot,
// Open must refuse — AWG packets are not our envelope, the AEAD key is
// different.
func TestOpenRejectsAWGSlots(t *testing.T) {
	c := NewCamp("test")
	pkt := make([]byte, OverheadSize+10)
	// craft magic in AWG slot 0 range
	start, _ := c.SlotRange(SlotAWGInit)
	pkt[0] = byte(start)
	pkt[1] = byte(start >> 8)
	pkt[2] = byte(start >> 16)
	pkt[3] = byte(start >> 24)
	if _, _, ok := c.Open(pkt); ok {
		t.Fatal("Open accepted packet with AWG-slot magic")
	}
}

// Two Camp instances with the same camp_id must produce the same key
// and ranges. This is the property that lets two peers compute the
// same parameters independently without an extra exchange.
func TestNewCampDeterministic(t *testing.T) {
	a := NewCamp("same-id")
	b := NewCamp("same-id")
	if a.key != b.key {
		t.Fatal("camp_key non-deterministic")
	}
	for i := 0; i < numSlots; i++ {
		if a.slots[i] != b.slots[i] {
			t.Errorf("slot %d ranges differ: %v vs %v", i, a.slots[i], b.slots[i])
		}
	}
}

// Different camp_ids must produce different keys (sanity check that
// HKDF is actually using the IKM).
func TestNewCampVariesByID(t *testing.T) {
	a := NewCamp("camp-a")
	b := NewCamp("camp-b")
	if a.key == b.key {
		t.Fatal("camp_key same for different camp_ids")
	}
}

// Slot ranges must never overlap — that's what makes the multiplex
// dispatch unambiguous on receive.
func TestSlotRangesDoNotOverlap(t *testing.T) {
	c := NewCamp("test-camp")
	for i := 0; i < numSlots; i++ {
		for j := i + 1; j < numSlots; j++ {
			si, ei := c.slots[i][0], c.slots[i][1]
			sj, ej := c.slots[j][0], c.slots[j][1]
			if si < ej && sj < ei {
				t.Errorf("slot %d [%x,%x) overlaps slot %d [%x,%x)", i, si, ei, j, sj, ej)
			}
		}
	}
}

// All ranges must sit inside the safe band, away from IP/JSON discriminators.
func TestSlotRangesInsideSafeBand(t *testing.T) {
	c := NewCamp("test")
	for i := 0; i < numSlots; i++ {
		s, e := c.slots[i][0], c.slots[i][1]
		if s < magicBandStart || e > magicBandEnd {
			t.Errorf("slot %d [%x,%x) outside band [%x,%x)", i, s, e, magicBandStart, magicBandEnd)
		}
	}
}

// SlotFor must round-trip with Seal's chosen magic — every sealed
// envelope is recognised by SlotFor as the slot we asked for.
func TestSlotForRoundtripsRandomMagic(t *testing.T) {
	c := NewCamp("test")
	for trial := 0; trial < 1000; trial++ {
		want := SlotHello
		sealed, err := c.Seal(want, []byte("x"))
		if err != nil {
			t.Fatal(err)
		}
		magic := uint32(sealed[0]) | uint32(sealed[1])<<8 | uint32(sealed[2])<<16 | uint32(sealed[3])<<24
		if got := c.SlotFor(magic); got != want {
			t.Fatalf("trial %d: SlotFor(%x) = %d, want %d", trial, magic, got, want)
		}
	}
}

// Magic outside all slot ranges → SlotFor returns -1.
func TestSlotForReturnsNegativeOnMiss(t *testing.T) {
	c := NewCamp("test")
	if got := c.SlotFor(0); got != -1 {
		t.Errorf("SlotFor(0) = %d, want -1", got)
	}
	// 0xFFFFFFFF is past the safe band
	if got := c.SlotFor(0xFFFFFFFF); got != -1 {
		t.Errorf("SlotFor(0xFFFFFFFF) = %d, want -1", got)
	}
}
