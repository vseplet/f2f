//go:build darwin

package dns

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

// FlushCache asks macOS to drop its DNS cache and restart the
// resolver. Called by the engine right after WriteResolver so any
// stale negative entries from before f2f-mac was running don't pin
// browsers to NXDOMAIN. Best-effort: failures are logged, not fatal.
func FlushCache() {
	if err := exec.Command("dscacheutil", "-flushcache").Run(); err != nil {
		log.Printf("dns: dscacheutil -flushcache: %v", err)
	}
	if err := exec.Command("killall", "-HUP", "mDNSResponder").Run(); err != nil {
		log.Printf("dns: killall -HUP mDNSResponder: %v", err)
	}
}

const resolverDir = "/etc/resolver"

// resolverPath returns the path of the per-zone resolver file for
// <zone>.f2f. macOS picks the file up automatically when present.
// zone is the human-friendly camp label (post-CampLabel split), kept
// under 63 chars so it fits a DNS label.
func resolverPath(zone string) string {
	return filepath.Join(resolverDir, zone+".f2f")
}

// WriteResolver drops a resolver file pointing at our local DNS server.
// Idempotent — overwrites if the file already exists. Needs the engine
// to be running as root (we do).
func WriteResolver(zone, bindAddr string) error {
	if zone == "" {
		return fmt.Errorf("empty zone")
	}
	host, port, err := splitHostPort(bindAddr)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(resolverDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", resolverDir, err)
	}
	body := fmt.Sprintf("nameserver %s\nport %s\nsearch_order 1\n", host, port)
	return os.WriteFile(resolverPath(zone), []byte(body), 0o644)
}

// ResolverFileExists reports whether /etc/resolver/<camp_id>.f2f is
// currently in place. Cheap (one os.Stat) and used by the UI to flag
// "macOS doesn't actually route our zone here" scenarios.
func ResolverFileExists(zone string) bool {
	if zone == "" {
		return false
	}
	_, err := os.Stat(resolverPath(zone))
	return err == nil
}

// RemoveResolver deletes the file. No-op if it doesn't exist.
func RemoveResolver(zone string) error {
	err := os.Remove(resolverPath(zone))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func splitHostPort(addr string) (string, string, error) {
	// net.SplitHostPort is fine but we avoid an extra import.
	colon := -1
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			colon = i
			break
		}
	}
	if colon < 0 {
		return "", "", fmt.Errorf("dns: invalid bind addr %q", addr)
	}
	return addr[:colon], addr[colon+1:], nil
}
