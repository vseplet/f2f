// Package attach is the shared attachment primitive for the block engine: a
// file carried by a block. The bytes live independently of any block — small
// files inline (Data, base64 over the wire), large ones content-addressed by
// the drop/torrent transport (InfoHash/Magnet). Both messages (a field on the
// message block) and notes/docs (a standalone block.file) reuse this one type,
// upload path and renderer, so there's a single place attachments are defined.
package attach

import "fmt"

// Max caps an inline attachment's raw size. Bigger files ride the torrent
// transport via the drop. Mirrors the bus frame budget.
const Max = 8 << 20 // 8 MiB

// Attachment describes one file. Either Data (inline bytes) or InfoHash/Magnet
// (torrent) is set, never both: small files inline, large ones by torrent.
type Attachment struct {
	Name     string `json:"name"`
	Mime     string `json:"mime"`
	Size     int    `json:"size"`
	Data     []byte `json:"data,omitempty"`
	InfoHash string `json:"info_hash,omitempty"`
	Magnet   string `json:"magnet,omitempty"`
}

// Check validates an inline attachment: an empty one is dropped (returns nil),
// an oversized one errors, and a valid one gets its Size stamped from Data.
// Torrent attachments (no Data) pass through untouched.
func Check(f *Attachment) (*Attachment, error) {
	if f == nil || (len(f.Data) == 0 && f.InfoHash == "") {
		return nil, nil
	}
	if len(f.Data) > Max {
		return nil, fmt.Errorf("attachment too large (max %d MiB)", Max>>20)
	}
	if len(f.Data) > 0 {
		f.Size = len(f.Data)
	}
	return f, nil
}
