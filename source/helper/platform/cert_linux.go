//go:build linux

package platform

// Linux trust-store management is fragmented across:
//   - system trust: copy PEM to /usr/local/share/ca-certificates/
//     and run update-ca-certificates (Debian/Ubuntu). RHEL/Fedora
//     uses /etc/pki/ca-trust/source/anchors/ + update-ca-trust.
//   - per-app NSS DB: Firefox/Chrome have their own (~/.pki/nssdb)
//     and need `certutil -A` to recognise the root.
//   - p11-kit shares the system trust where available.
//
// First-pass on OrbStack will likely skip system trust entirely and
// rely on the user accepting the self-signed cert in their browser.
// A real implementation goes here.

func TrustStoreContains(sha256HexUpper string) bool { return false }
func TrustStoreAdd(certPath string) error           { return ErrUnsupported }
func TrustStoreRemove(commonName string) error      { return ErrUnsupported }
