//go:build darwin

package platform

import (
	"fmt"
	"os/exec"
	"strings"
)

const systemKeychain = "/Library/Keychains/System.keychain"

// TrustStoreContains reports whether the system trust store holds a
// cert whose SHA-256 fingerprint matches sha256HexUpper. Pure read —
// does NOT trigger Authorization Services (no password prompt).
//
// We match by fingerprint (not Subject CN) because a half-failed
// install in an earlier f2f version might have left a different CA
// with the SAME CN behind — name-based check would falsely report
// "already installed", we'd skip TrustStoreAdd, and the user would
// never get their browser to trust the (now-different) cert.
func TrustStoreContains(sha256HexUpper string) bool {
	if sha256HexUpper == "" {
		return false
	}
	cmd := exec.Command("security", "find-certificate", "-a", "-Z", systemKeychain)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	// `security` prints lines like "SHA-256 hash: <hex_upper>". Substring
	// match is enough because we look for a 64-char hex.
	return strings.Contains(string(out), "SHA-256 hash: "+sha256HexUpper)
}

// TrustStoreAdd installs the PEM cert at certPath as a trusted SSL
// root in the system keychain. After this call, system services
// (Safari, Chrome via macOS trust store, curl with default options,
// Go's crypto/x509 fetching system roots) accept certs signed by
// this CA without warnings.
//
// This call always prompts for the macOS user password — Apple's
// anti-malware guard. Use TrustStoreContains to skip when the cert
// is already trusted.
func TrustStoreAdd(certPath string) error {
	cmd := exec.Command("security",
		"add-trusted-cert",
		"-d",              // add to admin (system) cert store
		"-r", "trustRoot", // trust as a root CA
		"-p", "ssl", // trust policy: SSL
		"-k", systemKeychain, // target keychain
		certPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("security add-trusted-cert: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// TrustStoreRemove deletes a cert from the system trust store by its
// Subject CN. Used when rotating the CA (e.g. camp_id changed).
// Not-found is non-fatal.
func TrustStoreRemove(commonName string) error {
	cmd := exec.Command("security", "delete-certificate", "-c", commonName, systemKeychain)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		// "Unable to delete certificate matching ..." → already gone, fine.
		if strings.Contains(msg, "Unable to delete certificate matching") {
			return nil
		}
		return fmt.Errorf("security delete-certificate: %s: %w", msg, err)
	}
	return nil
}
