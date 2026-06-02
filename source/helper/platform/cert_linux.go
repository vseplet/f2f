//go:build linux

package platform

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Linux trust-store management is fragmented:
//   - Debian/Ubuntu/Alpine: copy PEM to /usr/local/share/ca-certificates/
//     and run update-ca-certificates.
//   - RHEL/Fedora/CentOS:    /etc/pki/ca-trust/source/anchors/ +
//                            update-ca-trust extract.
//   - p11-kit (multi-distro): `trust anchor --store <pem>` writes to
//                             the shared trust db (works everywhere
//                             p11-kit is installed).
//   - NixOS:                  the system trust bundle is built from
//                             `security.pki.certificateFiles` at
//                             nixos-rebuild time and the resulting
//                             /etc/ssl/certs/ca-certificates.crt is a
//                             read-only symlink into the Nix store.
//                             Runtime install of system-trusted CAs
//                             is not possible — the user has to add
//                             the cert path declaratively.
//
// We try each strategy in order; the first one whose tool is on PATH
// and whose write+refresh succeeds wins.

const (
	debianAnchorDir = "/usr/local/share/ca-certificates"
	rhelAnchorDir   = "/etc/pki/ca-trust/source/anchors"
)

// TrustStoreContains reports whether sha256HexUpper matches a cert
// we previously installed. We don't try to parse the system bundle
// — Linux distros vary too much. Instead we trust our own marker
// directory under /var/lib/f2f/trusted-cas/.
func TrustStoreContains(sha256HexUpper string) bool {
	if sha256HexUpper == "" {
		return false
	}
	_, err := os.Stat(markerPath(sha256HexUpper))
	return err == nil
}

// TrustStoreAdd installs the PEM at certPath as a trusted root.
// Tries the standard tools in order; returns the first success, or a
// joined error listing what was attempted.
func TrustStoreAdd(certPath string) error {
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", certPath, err)
	}
	sha, err := certSHA256(pemBytes)
	if err != nil {
		return err
	}

	var attempted []string

	// Debian/Ubuntu/Alpine.
	if _, err := exec.LookPath("update-ca-certificates"); err == nil {
		attempted = append(attempted, "update-ca-certificates")
		dst := filepath.Join(debianAnchorDir, anchorFilename(certPath))
		if err := writeAnchor(dst, pemBytes); err == nil {
			if out, err := exec.Command("update-ca-certificates").CombinedOutput(); err == nil {
				_ = out
				return recordMarker(sha)
			}
		}
	}

	// RHEL/Fedora/CentOS.
	if _, err := exec.LookPath("update-ca-trust"); err == nil {
		attempted = append(attempted, "update-ca-trust")
		dst := filepath.Join(rhelAnchorDir, anchorFilename(certPath))
		if err := writeAnchor(dst, pemBytes); err == nil {
			if out, err := exec.Command("update-ca-trust", "extract").CombinedOutput(); err == nil {
				_ = out
				return recordMarker(sha)
			}
		}
	}

	// p11-kit (works on any distro that ships `trust` from p11-kit-bin).
	if _, err := exec.LookPath("trust"); err == nil {
		attempted = append(attempted, "trust (p11-kit)")
		if out, err := exec.Command("trust", "anchor", "--store", certPath).CombinedOutput(); err == nil {
			_ = out
			return recordMarker(sha)
		}
	}

	if len(attempted) == 0 {
		return fmt.Errorf("no system trust store tool found (tried: update-ca-certificates, update-ca-trust, trust); "+
			"on NixOS, add the CA path to security.pki.certificateFiles in configuration.nix: %w", ErrUnsupported)
	}
	return fmt.Errorf("trust store install failed (tried: %s)", strings.Join(attempted, ", "))
}

// TrustStoreRemove deletes any anchor files we wrote, refreshes the
// system bundle, and drops the marker. commonName is currently
// ignored — Linux trust stores key on filename, not subject CN, so
// we sweep both directories for files we control.
func TrustStoreRemove(commonName string) error {
	// Best-effort: walk both anchor dirs and remove anything with our
	// prefix. Real per-cert tracking would need state we don't keep.
	patterns := []string{
		filepath.Join(debianAnchorDir, "f2f-*.crt"),
		filepath.Join(rhelAnchorDir, "f2f-*.crt"),
	}
	removed := false
	for _, pat := range patterns {
		matches, _ := filepath.Glob(pat)
		for _, m := range matches {
			if err := os.Remove(m); err == nil {
				removed = true
			}
		}
	}
	if removed {
		if _, err := exec.LookPath("update-ca-certificates"); err == nil {
			_ = exec.Command("update-ca-certificates").Run()
		}
		if _, err := exec.LookPath("update-ca-trust"); err == nil {
			_ = exec.Command("update-ca-trust", "extract").Run()
		}
	}
	// Drop every marker; we don't know which one matches commonName
	// without parsing each cert, and the markers are cheap.
	if entries, err := os.ReadDir(markerDir()); err == nil {
		for _, e := range entries {
			_ = os.Remove(filepath.Join(markerDir(), e.Name()))
		}
	}
	return nil
}

func markerDir() string { return "/var/lib/f2f/trusted-cas" }

func markerPath(sha256HexUpper string) string {
	return filepath.Join(markerDir(), sha256HexUpper)
}

func recordMarker(sha256HexUpper string) error {
	if err := os.MkdirAll(markerDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(markerPath(sha256HexUpper), nil, 0o644)
}

// anchorFilename derives the on-disk anchor name from the source
// PEM path. We always prefix with `f2f-` so TrustStoreRemove can
// find and clean only what we own, and we force `.crt` because
// update-ca-certificates ignores anything else.
func anchorFilename(certPath string) string {
	base := strings.TrimSuffix(filepath.Base(certPath), filepath.Ext(certPath))
	return "f2f-" + base + ".crt"
}

func writeAnchor(dst string, pem []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, pem, 0o644)
}

// certSHA256 returns the SHA-256 fingerprint of the first PEM block
// as upper-case hex, matching ca.Fingerprint256Hex's format.
func certSHA256(pemBytes []byte) (string, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return "", fmt.Errorf("no PEM block in cert")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse cert: %w", err)
	}
	h := sha256.Sum256(cert.Raw)
	return strings.ToUpper(fmt.Sprintf("%x", h[:])), nil
}
