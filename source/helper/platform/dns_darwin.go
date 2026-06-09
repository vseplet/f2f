//go:build darwin

package platform

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const resolverDir = "/etc/resolver"

// FlushDNSCache asks macOS to drop its DNS cache and restart the
// resolver. Called after editing /etc/resolver so stale negative
// entries don't pin browsers to NXDOMAIN. Best-effort: failures are
// logged, not fatal.
func FlushDNSCache() error {
	if err := exec.Command("dscacheutil", "-flushcache").Run(); err != nil {
		log.Printf("dns: dscacheutil -flushcache: %v", err)
	}
	if err := exec.Command("killall", "-HUP", "mDNSResponder").Run(); err != nil {
		log.Printf("dns: killall -HUP mDNSResponder: %v", err)
	}
	return nil
}

// InstallZoneResolver drops a /etc/resolver/<zone>.f2f file pointing
// macOS's stub resolver at our local DNS server. macOS picks the
// file up automatically when present. zone is the human-friendly
// camp label, kept under 63 chars so it fits a DNS label.
func InstallZoneResolver(zone, bindAddr string) error {
	if zone == "" {
		return fmt.Errorf("empty zone")
	}
	host, port, err := net.SplitHostPort(bindAddr)
	if err != nil {
		return fmt.Errorf("split bind addr %q: %w", bindAddr, err)
	}
	if err := os.MkdirAll(resolverDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", resolverDir, err)
	}
	body := fmt.Sprintf("nameserver %s\nport %s\nsearch_order 1\n", host, port)
	return os.WriteFile(zoneResolverPath(zone), []byte(body), 0o644)
}

// ZoneResolverInstalled reports whether the per-zone resolver file
// is currently in place. Cheap (one os.Stat) — used by the UI to
// flag "macOS doesn't actually route our zone here" scenarios.
func ZoneResolverInstalled(zone string) bool {
	if zone == "" {
		return false
	}
	_, err := os.Stat(zoneResolverPath(zone))
	return err == nil
}

// RemoveZoneResolver deletes the resolver file. No-op if not
// present.
func RemoveZoneResolver(zone string) error {
	err := os.Remove(zoneResolverPath(zone))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func zoneResolverPath(zone string) string {
	return filepath.Join(resolverDir, zone+".f2f")
}

// InstallDomainResolver drops /etc/resolver/<domain> so macOS routes
// queries for that exact domain (and its subdomains) to our local
// DNS server. Used for intercept domains that must resolve to the
// exit-peer's view of the name instead of public DNS.
func InstallDomainResolver(domain, bindAddr string) error {
	if domain == "" || strings.ContainsAny(domain, "/\\ ") {
		return fmt.Errorf("bad resolver domain %q", domain)
	}
	host, port, err := net.SplitHostPort(bindAddr)
	if err != nil {
		return fmt.Errorf("split bind addr %q: %w", bindAddr, err)
	}
	if err := os.MkdirAll(resolverDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", resolverDir, err)
	}
	body := fmt.Sprintf("nameserver %s\nport %s\nsearch_order 1\n", host, port)
	return os.WriteFile(filepath.Join(resolverDir, domain), []byte(body), 0o644)
}

// RemoveDomainResolver deletes the per-domain resolver file. No-op
// if not present.
func RemoveDomainResolver(domain string) error {
	if domain == "" || strings.ContainsAny(domain, "/\\ ") {
		return nil
	}
	err := os.Remove(filepath.Join(resolverDir, domain))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
