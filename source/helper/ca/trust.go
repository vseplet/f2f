package ca

import "github.com/vseplet/f2f/source/helper/platform"

// EnsureSystemTrust installs the CA into the system trust store if
// it isn't there already. certPath is where the CA's PEM was
// previously written by Save — usually ca.CertPath(caDir).
//
// Idempotent: a no-op if a cert with the same SHA-256 fingerprint is
// already trusted, so it's safe to call on every start. The fresh
// install path may prompt the user for their password (macOS), so
// callers should check IsSystemTrusted first if they want to avoid
// the prompt on rotation.
func (ca *CA) EnsureSystemTrust(certPath string) error {
	if ca.IsSystemTrusted() {
		return nil
	}
	return platform.TrustStoreAdd(certPath)
}

// RemoveSystemTrust deletes the CA from the system trust store. Use
// before rotating (camp_id changed → new CA needs a new install).
// Not-found is non-fatal.
func (ca *CA) RemoveSystemTrust() error {
	return platform.TrustStoreRemove(ca.CommonName())
}

// IsSystemTrusted reports whether a cert with this CA's fingerprint
// is in the system trust store. Cheap (no password prompt).
func (ca *CA) IsSystemTrusted() bool {
	return platform.TrustStoreContains(ca.Fingerprint256Hex())
}
