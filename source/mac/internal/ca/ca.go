//go:build darwin

// Package ca generates and manages a per-camp local Certificate
// Authority. The CA is name-constrained to the camp's <camp_id>.f2f
// zone via the RFC 5280 NameConstraints extension, so even if the CA
// is added to a peer's trust store it can only sign certs under that
// zone — never bank.com, never another camp's zone.
//
// Leaf certs are issued on demand for SNI names and cached in memory.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 365 * 24 * time.Hour
)

// CA is a name-constrained certificate authority. Cert + Key are the
// authority's own self-signed root; PEM fields are precomputed for
// disk/transport. leafCache memoises issued server certs per hostname.
//
// Algorithm choice: ECDSA P-256. ed25519 would be smaller/faster but
// macOS's `security` tool refuses to import ed25519 CAs into the
// system keychain ("Unknown format in import"). P-256 is universally
// supported by macOS keychain, libssl, Chrome, Firefox, Safari.
type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
	KeyPEM  []byte

	mu        sync.Mutex
	leafCache map[string]*tls.Certificate
}

// Generate creates a new CA whose only permitted DNS subtree is
// "<campID>.f2f" (i.e. it can sign any "*.foo.<campID>.f2f").
func Generate(campID string) (*CA, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ca: keygen: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("ca: serial: %w", err)
	}
	// Leading dot makes this a subtree: e.g. ".beer.f2f" matches
	// "gitlab.beer.f2f", "x.gitlab.beer.f2f", etc.
	zone := "." + strings.ToLower(strings.TrimSpace(campID)) + ".f2f"
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "f2f Local CA · " + campID,
			Organization: []string{"f2f"},
		},
		NotBefore:                   time.Now().Add(-time.Hour),
		NotAfter:                    time.Now().Add(caValidity),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLen:                  0,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:                 []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{zone},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("ca: sign self: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("ca: parse self: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("ca: marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	return &CA{
		Cert:      cert,
		Key:       priv,
		CertPEM:   certPEM,
		KeyPEM:    keyPEM,
		leafCache: map[string]*tls.Certificate{},
	}, nil
}

// IssueLeaf returns a tls.Certificate for hostname, signed by this CA.
// Calls are idempotent — same hostname returns the same cached cert.
func (ca *CA) IssueLeaf(hostname string) (*tls.Certificate, error) {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	ca.mu.Lock()
	if c, ok := ca.leafCache[hostname]; ok {
		ca.mu.Unlock()
		return c, nil
	}
	ca.mu.Unlock()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{hostname},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.Cert, &priv.PublicKey, ca.Key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	tc := &tls.Certificate{
		Certificate: [][]byte{der, ca.Cert.Raw},
		PrivateKey:  priv,
		Leaf:        leaf,
	}
	ca.mu.Lock()
	ca.leafCache[hostname] = tc
	ca.mu.Unlock()
	return tc, nil
}

// Save writes ca.crt and ca.key to dir (0644/0600). Creates dir.
func (ca *CA) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), ca.CertPEM, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), ca.KeyPEM, 0o600); err != nil {
		return err
	}
	return nil
}

// CertPath returns the on-disk path of the cert as written by Save —
// useful for passing to keychain helpers that take a path argument.
func CertPath(dir string) string { return filepath.Join(dir, "ca.crt") }

// Load reads ca.crt/ca.key from dir. Returns (nil, nil) if files
// don't exist (engine treats that as "generate fresh").
func Load(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return nil, err
	}
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, fmt.Errorf("ca: decode cert pem")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, fmt.Errorf("ca: decode key pem")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := keyAny.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("ca: key is not ecdsa")
	}
	return &CA{
		Cert:      cert,
		Key:       key,
		CertPEM:   certPEM,
		KeyPEM:    keyPEM,
		leafCache: map[string]*tls.Certificate{},
	}, nil
}

// MatchesZone returns true iff this CA's permittedDNSDomains contains
// exactly ".<campID>.f2f". Used to detect "user changed camp_id, CA
// needs to be regenerated".
func (ca *CA) MatchesZone(campID string) bool {
	zone := "." + strings.ToLower(strings.TrimSpace(campID)) + ".f2f"
	for _, d := range ca.Cert.PermittedDNSDomains {
		if d == zone {
			return true
		}
	}
	return false
}

// Fingerprint returns a short hex prefix of SHA-256 over the DER cert.
// Used in UI ("trusted: peerA · 1a2b3c4d…").
func (ca *CA) Fingerprint() string {
	h := sha256.Sum256(ca.Cert.Raw)
	return fmt.Sprintf("%x", h[:8])
}

// CommonName returns the CA's CN field.
func (ca *CA) CommonName() string {
	return ca.Cert.Subject.CommonName
}
