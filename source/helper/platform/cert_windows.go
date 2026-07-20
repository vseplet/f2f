//go:build windows

package platform

import (
	"fmt"
	"os/exec"
	"strings"
)

// Windows keeps trusted roots in the LocalMachine\Root certificate store.
// certutil adds and removes; enumeration goes through PowerShell because the
// store is indexed by SHA-1 thumbprint while our callers identify a CA by its
// SHA-256 fingerprint — see TrustStoreContains for why that matters.
//
// All three need Administrator, which f2f already has (creating the tunnel
// adapter requires it).

const machineRootStore = "Root"

// TrustStoreContains reports whether the machine root store holds a cert whose
// SHA-256 fingerprint matches sha256HexUpper.
//
// We match on the fingerprint rather than the subject CN deliberately: a
// half-failed install from an earlier run can leave a DIFFERENT CA behind under
// the same CN, and a name-based check would then report "already trusted", skip
// the install, and leave the browser rejecting the CA actually in use.
//
// certutil prints only the SHA-1 thumbprint, so hash each cert's raw DER here.
func TrustStoreContains(sha256HexUpper string) bool {
	if sha256HexUpper == "" {
		return false
	}
	script := `$t=$false; Get-ChildItem Cert:\LocalMachine\` + machineRootStore + ` | ForEach-Object {` +
		`$h=[BitConverter]::ToString([Security.Cryptography.SHA256]::Create().ComputeHash($_.RawData)) -replace '-','';` +
		`if ($h -eq '` + strings.ToUpper(sha256HexUpper) + `') { $t=$true } };` +
		`if ($t) { 'present' }`
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "present")
}

// TrustStoreAdd installs the PEM at certPath as a trusted machine root, so
// browsers and anything else using the Windows trust store accept certs this CA
// signs. -f overwrites an existing entry instead of failing.
func TrustStoreAdd(certPath string) error {
	out, err := exec.Command("certutil", "-addstore", "-f", machineRootStore, certPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("certutil -addstore %s: %s: %w", machineRootStore, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// TrustStoreRemove deletes a cert from the machine root store by subject CN,
// used when rotating the CA (camp_id changed). certutil matches the name as a
// substring; "not found" is not an error for our purposes.
func TrustStoreRemove(commonName string) error {
	out, err := exec.Command("certutil", "-delstore", machineRootStore, commonName).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		// 0x80092004 (CRYPT_E_NOT_FOUND) — nothing matched, already gone.
		if strings.Contains(msg, "0x80092004") || strings.Contains(strings.ToLower(msg), "not found") {
			return nil
		}
		return fmt.Errorf("certutil -delstore %s: %s: %w", machineRootStore, msg, err)
	}
	return nil
}
