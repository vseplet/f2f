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
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// x25519HKDFInfo is the domain-separation tag for deriving the per-identity
// X25519 keypair from the Ed25519 seed. Bump the version suffix if we ever
// change the derivation scheme — the seed on disk stays the same, the new
// version just produces a different X25519 keypair.
const x25519HKDFInfo = "f2f-wg-static-v1"

// Identity is a loaded Ed25519 keypair plus its on-disk location, and the
// X25519 keypair derived from the Ed25519 seed. The X25519 part is used for
// AmneziaWG transport encryption (Noise IK static keys) and is never
// persisted — it's deterministic from priv.key.
type Identity struct {
	dir  string
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey

	x25519Priv [32]byte
	x25519Pub  [32]byte
}

// newIdentity assembles an Identity from raw key material and derives the
// X25519 keypair. All construction paths (Generate, load) go through this
// so x25519Priv/Pub are always populated.
func newIdentity(priv ed25519.PrivateKey, pub ed25519.PublicKey, dir string) *Identity {
	i := &Identity{dir: dir, priv: priv, pub: pub}
	seed := priv[:ed25519.SeedSize]
	reader := hkdf.New(sha256.New, seed, nil, []byte(x25519HKDFInfo))
	if _, err := io.ReadFull(reader, i.x25519Priv[:]); err != nil {
		// HKDF-SHA256 has 8160 bytes of output, reading 32 cannot fail.
		panic(fmt.Sprintf("identity: hkdf x25519: %v", err))
	}
	// X25519 clamp (RFC 7748 §5): zero low 3 bits, set bit 254, clear bit 255.
	i.x25519Priv[0] &= 248
	i.x25519Priv[31] &= 127
	i.x25519Priv[31] |= 64
	curve25519.ScalarBaseMult(&i.x25519Pub, &i.x25519Priv)
	return i
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
	return newIdentity(priv, pub, ""), nil
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
	return newIdentity(ed25519.PrivateKey(priv), ed25519.PublicKey(pub), dir), nil
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

// X25519Pub returns the 32-byte X25519 public key derived from this
// identity's Ed25519 seed. Used as the static pub for AmneziaWG handshakes.
// Stable: same priv.key always produces the same value.
func (i *Identity) X25519Pub() [32]byte { return i.x25519Pub }

// X25519PubHex returns X25519Pub formatted as 64-char hex — the form we
// ship over the wire in hello-packets.
func (i *Identity) X25519PubHex() string { return hex.EncodeToString(i.x25519Pub[:]) }

// X25519Priv returns the 32-byte X25519 private scalar (clamped per
// RFC 7748). Goes into AmneziaWG device's `private_key=` UAPI field.
// Don't log this, don't ship this — it's the transport secret.
func (i *Identity) X25519Priv() [32]byte { return i.x25519Priv }

// helloCanonicalTag is the domain-separation prefix for hello signatures.
// Bumping the suffix invalidates all existing hello signatures while
// keeping priv.key intact — used if the canonical message format ever
// needs to change.
const helloCanonicalTag = "f2f-hello-v1"

// HelloCanonical returns the byte string a hello signature covers.
// Pipe-delimited: tag|name|pub_hex|wg_pub_hex. Name is restricted to
// ASCII printable without '|' (see ValidHelloName) so the delimiter is
// unambiguous; pub/wg_pub are hex and naturally pipe-free.
func HelloCanonical(name, pubHex, wgPubHex string) []byte {
	return []byte(helloCanonicalTag + "|" + name + "|" + pubHex + "|" + wgPubHex)
}

// ValidHelloName reports whether name is acceptable as the `name` field
// of a hello (and therefore can be safely embedded in HelloCanonical
// without ambiguity). Allows printable ASCII except '|', length 1..64.
func ValidHelloName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c > 0x7E || c == '|' {
			return false
		}
	}
	return true
}

// SignHello signs the canonical hello message for this identity's own
// keys + the given display name. Returns the 64-byte Ed25519 signature.
// Caller is responsible for passing a name that satisfies ValidHelloName;
// signing an invalid name is allowed but VerifyHello will reject it.
func (i *Identity) SignHello(name string) []byte {
	return ed25519.Sign(i.priv, HelloCanonical(name, i.PubHex(), i.X25519PubHex()))
}

// VerifyHello checks sig against (name, pubHex, wgPubHex) under the
// canonical hello message. Returns true only when:
//   - name passes ValidHelloName
//   - pubHex decodes to a valid Ed25519 public key
//   - sig is a valid Ed25519 signature of HelloCanonical(name, pubHex, wgPubHex)
//
// Length of sig must be ed25519.SignatureSize (64); anything else → false.
func VerifyHello(name, pubHex, wgPubHex string, sig []byte) bool {
	if !ValidHelloName(name) {
		return false
	}
	if len(sig) != ed25519.SignatureSize {
		return false
	}
	pubBytes, err := hex.DecodeString(pubHex)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pubBytes), HelloCanonical(name, pubHex, wgPubHex), sig)
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
