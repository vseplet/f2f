// Package db is f2f's distributed database: append-only,
// per-(author,scope) signed logs that replicate between peers by
// anti-entropy. It sits ABOVE mesh (replicates over mesh/bus) and BELOW
// all services — one shared store every feature builds on, dumpable and
// shareable as a whole.
//
// It moves OPAQUE signed entries — the meaning of an entry (chat message,
// document block, task) and how concurrent versions reconcile
// (siblings/tabs, folds, CRDT) lives in the apps ABOVE this package.
// See docs/BLOCKS.md and docs/MESSAGING_DESIGN.md.
//
// Layering:
//   - this package: entries (sign/verify/chain), per-scope log store,
//     version vectors, "what am I missing" diff, whole-DB dump/import.
//     Scope-agnostic.
//   - sync (next): anti-entropy over mesh/bus — exchange version vectors,
//     stream missing entries, relay sealed entries store-and-forward.
//   - services (chat/docs/tasks): write typed payloads, fold the log into
//     state, render heads as tabs.
package db

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
)

// Entry is one immutable, signed record in a per-(author,scope) log.
// Nothing is ever mutated or deleted — "edit" and "delete" are new
// entries. State is a deterministic fold over a scope's entries.
type Entry struct {
	// Scope is the log namespace: a channel, document, or task list id.
	Scope string `json:"scope"`
	// Author is the writer's identity (user_id; currently the Ed25519 pub
	// hex). Each (Author,Scope) is a single-writer linear chain.
	Author string `json:"author"`
	// Seq is the 1-based position in this author's chain for this scope.
	Seq uint64 `json:"seq"`
	// Prev is the ID (content hash) of the previous entry by this author
	// in this scope; "" for Seq==1. Forms the per-author hash chain.
	Prev string `json:"prev,omitempty"`
	// Lamport is a logical clock for ordering CONCURRENT entries across
	// authors (causal order comes from app-level parent refs in Payload;
	// this breaks ties deterministically with Author/ID). Never wall-clock.
	Lamport uint64 `json:"lamport"`
	// Type tags the payload for the app layer (e.g. "chat.msg",
	// "block.update", "block.merge").
	Type string `json:"type"`
	// Payload is the opaque app body — may be e2e ciphertext. db never
	// interprets it.
	Payload []byte `json:"payload,omitempty"`
	// TS is wall-clock ms, for human display ONLY — not used for ordering.
	TS int64 `json:"ts,omitempty"`
	// Sig is the Ed25519 signature over the canonical bytes (hex).
	Sig string `json:"sig"`
	// ID is the content hash (hex of SHA-256 over the canonical bytes).
	// Derived, not part of the signed content; set on commit/verify.
	ID string `json:"id"`
}

// signer is the minimal surface needed to author entries — satisfied by
// *identity.Identity (Sign + PubHex).
type signer interface {
	Sign(msg []byte) []byte
	PubHex() string
}

// canonical builds the deterministic byte string the signature and ID
// cover. Excludes Sig and ID. Field separators are newlines; the binary
// payload is base64 so it can't inject a separator.
func (e *Entry) canonical() []byte {
	var b strings.Builder
	b.WriteString("f2f-db-v1\n")
	b.WriteString(e.Scope)
	b.WriteByte('\n')
	b.WriteString(e.Author)
	b.WriteByte('\n')
	b.WriteString(strconv.FormatUint(e.Seq, 10))
	b.WriteByte('\n')
	b.WriteString(e.Prev)
	b.WriteByte('\n')
	b.WriteString(strconv.FormatUint(e.Lamport, 10))
	b.WriteByte('\n')
	b.WriteString(e.Type)
	b.WriteByte('\n')
	b.WriteString(strconv.FormatInt(e.TS, 10))
	b.WriteByte('\n')
	b.WriteString(base64.RawStdEncoding.EncodeToString(e.Payload))
	return []byte(b.String())
}

// computeID returns the content hash (hex SHA-256 of canonical bytes).
func (e *Entry) computeID() string {
	sum := sha256.Sum256(e.canonical())
	return hex.EncodeToString(sum[:])
}

// sign sets Author, Sig and ID using s. Seq/Prev/Lamport/TS must already
// be set by the store.
func (e *Entry) sign(s signer) {
	e.Author = s.PubHex()
	e.Sig = hex.EncodeToString(s.Sign(e.canonical()))
	e.ID = e.computeID()
}

// verify checks the signature against Author and that ID matches the
// content. Returns nil if the entry is authentic and intact.
func (e *Entry) verify() error {
	pub, err := hex.DecodeString(e.Author)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("db: bad author key")
	}
	sig, err := hex.DecodeString(e.Sig)
	if err != nil {
		return errors.New("db: bad signature encoding")
	}
	if !ed25519.Verify(pub, e.canonical(), sig) {
		return errors.New("db: signature mismatch")
	}
	if e.ID != e.computeID() {
		return errors.New("db: id does not match content")
	}
	return nil
}
