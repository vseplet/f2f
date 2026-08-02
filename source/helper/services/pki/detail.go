package pki

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vseplet/f2f/source/helper/platform"
)

// CertDetail is the full, human-readable view of one CA certificate, surfaced
// to the portal's certificate viewer. It exists so a user can see exactly what
// a CA is scoped to BEFORE (or after) trusting it — in particular PermittedDNS,
// the RFC 5280 name constraints that cap which domains the CA may vouch for.
type CertDetail struct {
	CommonName        string   `json:"common_name"`
	Subject           string   `json:"subject"`
	Issuer            string   `json:"issuer"`
	SelfSigned        bool     `json:"self_signed"`
	Serial            string   `json:"serial"`
	KeyType           string   `json:"key_type"`
	NotBefore         int64    `json:"not_before"`
	NotAfter          int64    `json:"not_after"`
	PermittedDNS      []string `json:"permitted_dns"`
	PermittedCritical bool     `json:"permitted_critical"`
	SANs              []string `json:"sans"`
	IsCA              bool     `json:"is_ca"`
	Fingerprint       string   `json:"fingerprint"` // short 8-byte key used across the UI
	SHA256            string   `json:"sha256"`      // full SHA-256, colon-separated hex
	Installed         bool     `json:"installed"`
	Mine              bool     `json:"mine"`
}

// CertDetail returns the parsed detail for the CA with the given short
// fingerprint — our own CA or any discovered peer CA. ok=false if unknown.
func (s *Service) CertDetail(fp string) (*CertDetail, bool) {
	s.mu.Lock()
	myCA := s.myCA
	entry, isPeer := s.peers[fp]
	dir := s.peerCAsDir()
	s.mu.Unlock()

	if myCA != nil && myCA.Fingerprint() == fp {
		return buildCertDetail(myCA.Cert, trustStoreHas(myCA.Cert), true), true
	}
	if !isPeer {
		return nil, false
	}
	body, err := os.ReadFile(filepath.Join(dir, entry.PeerName+".crt"))
	if err != nil {
		return nil, false
	}
	cert, err := parseCACert(body)
	if err != nil {
		return nil, false
	}
	return buildCertDetail(cert, entry.Installed, false), true
}

func trustStoreHas(cert *x509.Certificate) bool {
	return platform.TrustStoreContains(strings.ToUpper(fmt.Sprintf("%x", sha256.Sum256(cert.Raw))))
}

func buildCertDetail(cert *x509.Certificate, installed, mine bool) *CertDetail {
	full := sha256.Sum256(cert.Raw)
	return &CertDetail{
		CommonName:        cert.Subject.CommonName,
		Subject:           cert.Subject.String(),
		Issuer:            cert.Issuer.String(),
		SelfSigned:        cert.Subject.String() == cert.Issuer.String(),
		Serial:            fmt.Sprintf("%X", cert.SerialNumber),
		KeyType:           keyType(cert),
		NotBefore:         cert.NotBefore.Unix(),
		NotAfter:          cert.NotAfter.Unix(),
		PermittedDNS:      cert.PermittedDNSDomains,
		PermittedCritical: cert.PermittedDNSDomainsCritical,
		SANs:              cert.DNSNames,
		IsCA:              cert.IsCA,
		Fingerprint:       certFingerprint(cert),
		SHA256:            colonHex(full[:]),
		Installed:         installed,
		Mine:              mine,
	}
}

func keyType(cert *x509.Certificate) string {
	switch k := cert.PublicKey.(type) {
	case *rsa.PublicKey:
		return fmt.Sprintf("RSA %d", k.N.BitLen())
	case *ecdsa.PublicKey:
		return "ECDSA " + k.Curve.Params().Name
	case ed25519.PublicKey:
		return "Ed25519"
	default:
		return cert.PublicKeyAlgorithm.String()
	}
}

func colonHex(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("%02X", x)
	}
	return strings.Join(parts, ":")
}
