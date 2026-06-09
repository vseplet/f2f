//go:build linux

package platform

import (
	"fmt"
	"log"
	"os/exec"
)

// FlushDNSCache tries to flush the systemd-resolved cache. If
// systemd-resolved is not present (Alpine, plain busybox, etc.) the
// kernel has no central DNS cache to flush — applications do their
// own caching, which is out of our reach. Best-effort.
func FlushDNSCache() error {
	if err := exec.Command("resolvectl", "flush-caches").Run(); err != nil {
		log.Printf("dns: resolvectl flush-caches: %v (no systemd-resolved?)", err)
	}
	return nil
}

// InstallZoneResolver routes queries for <zone>.f2f to our local DNS
// server. The Linux implementation will need to detect what's in use
// (systemd-resolved vs NetworkManager vs plain resolv.conf) and pick
// the right mechanism. Not done yet — stubbed.
func InstallZoneResolver(zone, bindAddr string) error {
	return fmt.Errorf("InstallZoneResolver on linux: %w", ErrUnsupported)
}

// ZoneResolverInstalled mirrors InstallZoneResolver. Returns false
// until a Linux implementation lands.
func ZoneResolverInstalled(zone string) bool { return false }

// RemoveZoneResolver mirrors InstallZoneResolver. No-op while the
// install path is unimplemented.
func RemoveZoneResolver(zone string) error { return nil }

// InstallDomainResolver mirrors the darwin per-domain resolver
// install. Linux needs systemd-resolved routing-domain support —
// stubbed alongside InstallZoneResolver.
func InstallDomainResolver(domain, bindAddr string) error {
	return fmt.Errorf("InstallDomainResolver on linux: %w", ErrUnsupported)
}

// RemoveDomainResolver mirrors InstallDomainResolver. No-op while
// the install path is unimplemented.
func RemoveDomainResolver(domain string) error { return nil }
