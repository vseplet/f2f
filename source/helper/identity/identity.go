// Package identity owns the per-camp Ed25519 keypair used to prove
// ownership of a tunnel_ip to the camp server. Each camp gets its own
// keypair under /var/lib/f2f/identity/<camp_id>/ so different camps
// can't correlate the same operator across sessions, and "leaving" a
// camp is as physical as removing that directory.
//
// The camp server (re)binds tunnel_ip stickily by pubkey, so the
// keypair must persist for an operator to keep their address across
// reconnects. Display alias (`--name`) is independent and mutable.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Identity is a loaded Ed25519 keypair plus its on-disk location.
type Identity struct {
	dir  string
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// LoadOrGenerate returns the identity for `dir`, creating one if the
// directory is empty. dir is created with 0700; priv.key is written
// 0600 and pub.key 0644. Caller is responsible for picking the dir
// (typically /var/lib/f2f/identity/<camp_id>).
func LoadOrGenerate(dir string) (*Identity, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	id, err := load(dir)
	if err == nil {
		return id, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	id, err = Generate()
	if err != nil {
		return nil, err
	}
	if err := id.Save(dir); err != nil {
		return nil, err
	}
	return id, nil
}

// Generate returns a fresh keypair in memory, not persisted yet. Used
// when the caller needs the pub *before* deciding where to save the
// identity — e.g. when the camp_id is being constructed from the pub.
// Save() commits to disk afterwards.
func Generate() (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate: %w", err)
	}
	return &Identity{priv: priv, pub: pub}, nil
}

// Save writes the keypair to dir (priv.key 0600, pub.key 0644) and
// remembers dir for Dir(). Idempotent: overwrites if files exist.
func (i *Identity) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "priv.key"), i.priv, 0o600); err != nil {
		return fmt.Errorf("identity: write priv: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pub.key"), i.pub, 0o644); err != nil {
		return fmt.Errorf("identity: write pub: %w", err)
	}
	i.dir = dir
	return nil
}

func load(dir string) (*Identity, error) {
	privPath := filepath.Join(dir, "priv.key")
	pubPath := filepath.Join(dir, "pub.key")
	priv, err := os.ReadFile(privPath)
	if err != nil {
		return nil, err
	}
	pub, err := os.ReadFile(pubPath)
	if err != nil {
		return nil, err
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("identity: %s has %d bytes, expected %d", privPath, len(priv), ed25519.PrivateKeySize)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("identity: %s has %d bytes, expected %d", pubPath, len(pub), ed25519.PublicKeySize)
	}
	return &Identity{dir: dir, priv: ed25519.PrivateKey(priv), pub: ed25519.PublicKey(pub)}, nil
}

// CampLabel returns the human-friendly suffix of a camp_id. New-format
// camp_ids look like "<64-hex-pub>_<label>"; legacy ones are free-form
// strings without that structure. We split on the underscore only when
// the prefix is exactly 64 hex chars — otherwise the whole id is the
// label (covers legacy camps like "12345").
func CampLabel(campID string) string {
	if len(campID) > 65 && campID[64] == '_' && isHex64(campID[:64]) {
		return campID[65:]
	}
	return campID
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < 64; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// PubHex returns the 64-char hex of the 32-byte Ed25519 public key.
// This is the identifier the camp server uses for sticky bindings.
func (i *Identity) PubHex() string {
	return hex.EncodeToString(i.pub)
}

// Fingerprint returns the first 8 bytes of SHA-256(pub) as 16 hex
// chars — short enough for the UI, wide enough to make collisions
// effectively zero in a single camp.
func (i *Identity) Fingerprint() string {
	h := sha256.Sum256(i.pub)
	return hex.EncodeToString(h[:8])
}

// FingerprintHex computes the same fingerprint from a hex-encoded
// pubkey. Used to render peer identities the engine only knows by
// pub hex (e.g. via camp's PeerInfo). Returns "" on bad input.
func FingerprintHex(pubHex string) string {
	if pubHex == "" {
		return ""
	}
	raw, err := hex.DecodeString(pubHex)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:8])
}

// Sign returns an Ed25519 signature over msg.
func (i *Identity) Sign(msg []byte) []byte {
	return ed25519.Sign(i.priv, msg)
}

// Dir returns the directory the keypair lives in (for diagnostics).
func (i *Identity) Dir() string {
	return i.dir
}

// InviteBody mirrors the camp-side InviteBody — same JSON shape, kept
// in sync by convention. Owner signs over inviteSigMsg(body) and ships
// the base64-encoded {body, sig} token to the invitee out-of-band.
type InviteBody struct {
	CampID    string `json:"camp_id"`
	InviteID  string `json:"invite_id"`
	ExpiresAt int64  `json:"expires_at"` // unix ms
}

type inviteToken struct {
	Body InviteBody `json:"body"`
	Sig  string     `json:"sig"`
}

// InviteSigMsg returns the canonical bytes an invite signature
// covers. Exported so tests / server can build the same string and
// verify.
func InviteSigMsg(campID, inviteID string, expiresAt int64) string {
	return fmt.Sprintf("invite|%s|%s|%d", campID, inviteID, expiresAt)
}

// GenerateInvite produces an owner-signed bearer invite token (base64
// JSON). campID is bound into the body; ttl sets expiry from now.
// invite_id is 16 random hex chars — both the entropy source for
// uniqueness and the key the camp uses to mark single-use.
//
// Caller must be the camp owner for the token to be accepted by the
// server. We don't check ownership here — that's a server-side fact.
func (i *Identity) GenerateInvite(campID string, ttl time.Duration) (string, error) {
	if campID == "" {
		return "", fmt.Errorf("invite: camp_id required")
	}
	if ttl <= 0 {
		return "", fmt.Errorf("invite: ttl must be positive")
	}
	var idBytes [8]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return "", fmt.Errorf("invite: rand: %w", err)
	}
	body := InviteBody{
		CampID:    campID,
		InviteID:  hex.EncodeToString(idBytes[:]),
		ExpiresAt: time.Now().Add(ttl).UnixMilli(),
	}
	sig := i.Sign([]byte(InviteSigMsg(body.CampID, body.InviteID, body.ExpiresAt)))
	wire := inviteToken{Body: body, Sig: hex.EncodeToString(sig)}
	raw, err := json.Marshal(wire)
	if err != nil {
		return "", fmt.Errorf("invite: marshal: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}
