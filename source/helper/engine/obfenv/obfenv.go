// Package obfenv implements the obfuscation envelope that wraps every
// f2f control-plane UDP packet (currently: hello-handshake; reserved
// slots for future control types).
//
// On the wire each envelope looks like:
//
//	[4 bytes magic (random in slot's HKDF-derived range)]
//	[12 bytes nonce (random)]
//	[ciphertext (ChaCha20-Poly1305(camp_key, plaintext) with magic as AAD)]
//	[16 bytes Poly1305 tag]
//
// For an outside observer (DPI, ISP) it is indistinguishable from any
// other random-looking UDP — same shape as AmneziaWG transport packets.
//
// All parameters (camp_key, eight magic ranges H1..H8) are derived
// deterministically from camp_id via HKDF-SHA256 with version-tagged
// info labels. Two members of the same camp compute the same values
// without any extra exchange; a member of a different camp gets a
// completely different set.
//
// Slots:
//
//	0..3 → H1..H4 → AmneziaWG packet types (init/response/cookie/transport),
//	                consumed by the AWG device itself, not by Seal/Open here.
//	4    → H5     → Hello-handshake, sealed/opened by this package.
//	5..7 → H6..H8 → reserved for future control types.
package obfenv

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// Slot enumerates the eight magic-header slots. AWG slots are listed
// here for completeness — their ranges are passed to amneziawg-go via
// `h1=`..`h4=` UAPI; this package does NOT seal/open AWG traffic.
type Slot int

const (
	SlotAWGInit      Slot = 0 // H1
	SlotAWGResponse  Slot = 1 // H2
	SlotAWGCookie    Slot = 2 // H3
	SlotAWGTransport Slot = 3 // H4
	SlotHello        Slot = 4 // H5 — this is the one Seal/Open use
	SlotReserved6    Slot = 5 // H6
	SlotReserved7    Slot = 6 // H7
	SlotReserved8    Slot = 7 // H8
	numSlots              = 8
)

// Sizes of the framing fields. Exported so callers can pre-size buffers.
const (
	MagicSize    = 4
	NonceSize    = chacha20poly1305.NonceSize
	TagSize      = chacha20poly1305.Overhead
	OverheadSize = MagicSize + NonceSize + TagSize
)

// Safe band for magic values. Avoids:
//   - IPv4 (version nibble in 0x40..0x4F)
//   - IPv6 (version nibble in 0x60..0x6F)
//   - JSON start byte `{` (0x7B)
//   - 1-byte hole-punch (`0x00` — historical, will disappear once hello rolls out)
//
// Within [magicBandStart, magicBandEnd) the band is split into eight
// equal sub-bands, one per slot, and each slot's 256-wide window is
// placed deterministically inside its sub-band via HKDF.
const (
	magicBandStart  = uint32(0x80000000)
	magicBandEnd    = uint32(0xC0000000)
	magicRangeWidth = 256
)

// Camp holds the precomputed obfuscation parameters for one camp.
// Build once at engine start via NewCamp, reuse for all envelope ops.
type Camp struct {
	campID string
	key    [32]byte
	slots  [numSlots][2]uint32 // slots[i] = {start, end} (half-open)
}

// NewCamp derives all parameters for the given camp_id. Two pieces of
// information are produced:
//
//   - camp_key = HKDF-SHA256(camp_id, info="f2f-control-v1")
//   - per-slot magic ranges (see package docs)
func NewCamp(campID string) *Camp {
	c := &Camp{campID: campID}
	hkdfRead([]byte(campID), "f2f-control-v1", c.key[:])
	subWidth := (magicBandEnd - magicBandStart) / numSlots
	for slot := 0; slot < numSlots; slot++ {
		subStart := magicBandStart + uint32(slot)*subWidth
		var raw [4]byte
		hkdfRead([]byte(campID), fmt.Sprintf("f2f-magic-h%d-v1", slot+1), raw[:])
		// Place a 256-wide window deterministically inside the sub-band,
		// leaving room at the right edge so the window doesn't overflow.
		offset := binary.LittleEndian.Uint32(raw[:]) % (subWidth - magicRangeWidth)
		c.slots[slot][0] = subStart + offset
		c.slots[slot][1] = subStart + offset + magicRangeWidth
	}
	return c
}

// CampID returns the camp_id this Camp was built from (for diagnostics).
func (c *Camp) CampID() string { return c.campID }

// SlotRange returns the (start, end) magic-header range for the slot.
// end is exclusive. Used to feed h1..h4 to the AWG device UAPI.
func (c *Camp) SlotRange(slot Slot) (start, end uint32) {
	return c.slots[slot][0], c.slots[slot][1]
}

// SlotFor returns the slot whose range contains v, or -1 if none. O(8),
// trivially cheap — fine to call on every incoming packet for multiplex.
func (c *Camp) SlotFor(v uint32) Slot {
	for slot := 0; slot < numSlots; slot++ {
		if v >= c.slots[slot][0] && v < c.slots[slot][1] {
			return Slot(slot)
		}
	}
	return -1
}

// Seal wraps plaintext into a control-envelope for the given slot. The
// magic header is chosen uniformly at random from the slot's range; the
// nonce is fresh on every call. Returns the byte slice on the wire.
//
// Only slots reserved for our control plane (SlotHello and SlotReserved*)
// are valid here. Passing an AWG slot panics — AWG traffic is encrypted
// by the AWG device, not by us.
func (c *Camp) Seal(slot Slot, plaintext []byte) ([]byte, error) {
	if slot < SlotHello || slot >= numSlots {
		return nil, fmt.Errorf("obfenv: Seal: slot %d is not a control slot", slot)
	}
	aead, err := chacha20poly1305.New(c.key[:])
	if err != nil {
		// chacha20poly1305.New only fails on wrong-sized key — our key is
		// always 32 bytes from HKDF, so this branch is unreachable.
		return nil, fmt.Errorf("obfenv: aead: %w", err)
	}
	out := make([]byte, MagicSize+NonceSize, OverheadSize+len(plaintext))
	binary.LittleEndian.PutUint32(out[:MagicSize], c.randomMagic(slot))
	if _, err := rand.Read(out[MagicSize:]); err != nil {
		return nil, fmt.Errorf("obfenv: rand nonce: %w", err)
	}
	nonce := out[MagicSize : MagicSize+NonceSize]
	aad := out[:MagicSize]
	out = aead.Seal(out, nonce, plaintext, aad)
	return out, nil
}

// Open verifies and decrypts an envelope. Returns the plaintext and the
// slot the magic landed in. ok=false on:
//   - packet too short for framing
//   - magic not in any slot's range
//   - magic in an AWG slot (not our envelope — caller should route to AWG)
//   - bad auth tag
//
// The magic-out-of-range and AWG-slot cases are checked before the AEAD
// call to avoid wasted CPU on every passing packet.
func (c *Camp) Open(packet []byte) (plaintext []byte, slot Slot, ok bool) {
	if len(packet) < OverheadSize {
		return nil, -1, false
	}
	magic := binary.LittleEndian.Uint32(packet[:MagicSize])
	slot = c.SlotFor(magic)
	if slot < SlotHello {
		// Either no match (slot=-1) or an AWG slot we don't own — refuse.
		return nil, slot, false
	}
	aead, err := chacha20poly1305.New(c.key[:])
	if err != nil {
		return nil, slot, false
	}
	nonce := packet[MagicSize : MagicSize+NonceSize]
	ciphertext := packet[MagicSize+NonceSize:]
	plaintext, err = aead.Open(nil, nonce, ciphertext, packet[:MagicSize])
	if err != nil {
		return nil, slot, false
	}
	return plaintext, slot, true
}

// randomMagic picks a uint32 uniformly from slot's range.
func (c *Camp) randomMagic(slot Slot) uint32 {
	start, end := c.slots[slot][0], c.slots[slot][1]
	width := end - start
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand failure is catastrophic and indicates a kernel issue
		// (no entropy source). There is no sane recovery — caller would
		// just get a corrupted packet on the wire.
		panic(fmt.Sprintf("obfenv: rand: %v", err))
	}
	return start + binary.LittleEndian.Uint32(raw[:])%width
}

func hkdfRead(ikm []byte, info string, out []byte) {
	reader := hkdf.New(sha256.New, ikm, nil, []byte(info))
	if _, err := io.ReadFull(reader, out); err != nil {
		// HKDF-SHA256 outputs 8160 bytes — reading <=32 cannot fail.
		panic(fmt.Sprintf("obfenv: hkdf %s: %v", info, err))
	}
}
