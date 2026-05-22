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
// <camp_id>.f2f. macOS picks the file up automatically when present.
func resolverPath(campID string) string {
	return filepath.Join(resolverDir, campID+".f2f")
}

// WriteResolver drops a resolver file pointing at our local DNS server.
// Idempotent — overwrites if the file already exists. Needs the engine
// to be running as root (we do).
func WriteResolver(campID, bindAddr string) error {
	if campID == "" {
		return fmt.Errorf("empty camp id")
	}
	host, port, err := splitHostPort(bindAddr)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(resolverDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", resolverDir, err)
	}
	body := fmt.Sprintf("nameserver %s\nport %s\nsearch_order 1\n", host, port)
	return os.WriteFile(resolverPath(campID), []byte(body), 0o644)
}

// RemoveResolver deletes the file. No-op if it doesn't exist.
func RemoveResolver(campID string) error {
	err := os.Remove(resolverPath(campID))
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
