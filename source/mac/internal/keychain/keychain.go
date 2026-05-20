//go:build darwin

// Package keychain wraps macOS `security`-tool calls used to install
// and remove our local-CA root in the system keychain. Everything here
// needs root, which the engine already has.
package keychain

import (
	"fmt"
	"os/exec"
	"strings"
)

const systemKeychain = "/Library/Keychains/System.keychain"

// AddTrustedRoot installs the PEM cert at certPath as a trusted SSL
// root in the system keychain. After this call, system services
// (Safari, Chrome via macOS trust store, curl with default options,
// Go's crypto/x509 fetching system roots) accept certs signed by this
// CA without warnings.
//
// Idempotent — re-adding the same cert is a no-op (security tool just
// re-affirms the trust).
func AddTrustedRoot(certPath string) error {
	cmd := exec.Command("security",
		"add-trusted-cert",
		"-d",                 // add to admin (system) cert store
		"-r", "trustRoot",    // trust as a root CA
		"-p", "ssl",          // trust policy: SSL
		"-k", systemKeychain, // target keychain
		certPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("security add-trusted-cert: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// RemoveByCommonName deletes a cert from the system keychain by its
// Subject CN. Used when rotating the CA (e.g. camp_id changed).
// Not-found is non-fatal.
func RemoveByCommonName(cn string) error {
	cmd := exec.Command("security",
		"delete-certificate",
		"-c", cn,
		systemKeychain,
	)
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
