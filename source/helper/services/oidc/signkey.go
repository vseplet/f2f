// Per-camp RSA signing key for OIDC tokens. We sign ID/access tokens with
// RS256 rather than EdDSA: real-world OIDC clients (Gitea via
// coreos/go-oidc, and many others) only accept RS256/ES256/PS256 and
// reject EdDSA at verification. The key is generated on first use and
// persisted as oidc_rsa.pem in the camp dir, so the kid (and thus cached
// JWKS in relying parties) stays stable across restarts.
package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// SignKeys loads/generates the camp's RSA signing key under dirFn().
type SignKeys struct {
	dirFn func() string

	mu  sync.Mutex
	dir string // dir the cached key belongs to (detects camp switch)
	key *rsa.PrivateKey
	kid string
}

func NewSignKeys(dirFn func() string) *SignKeys {
	return &SignKeys{dirFn: dirFn}
}

// get returns the camp's RSA private key and its key id, loading from disk
// or generating+persisting on first use. Fails outside a camp.
func (s *SignKeys) get() (*rsa.PrivateKey, string, error) {
	dir := s.dirFn()
	if dir == "" {
		return nil, "", fmt.Errorf("oidc: no camp dir for signing key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key != nil && s.dir == dir {
		return s.key, s.kid, nil
	}

	path := filepath.Join(dir, "oidc_rsa.pem")
	key, err := loadOrGenerateRSA(path)
	if err != nil {
		return nil, "", err
	}
	s.key, s.kid, s.dir = key, rsaKid(&key.PublicKey), dir
	return s.key, s.kid, nil
}

func loadOrGenerateRSA(path string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, fmt.Errorf("oidc: bad rsa pem %s", path)
		}
		return x509.ParsePKCS1PrivateKey(blk.Bytes)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// rsaKid derives a stable key id from the public key (first 16 hex of the
// SHA-256 of its DER), so the same persisted key always yields the same
// kid in the JWKS and token headers.
func rsaKid(pub *rsa.PublicKey) string {
	der := x509.MarshalPKCS1PublicKey(pub)
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:8])
}
