//go:build linux

package platform

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"

	"github.com/vseplet/f2f/source/helper/clog"
)

// Linux DNS integration routes the camp's .f2f zone (and any intercept
// domains) to f2f's local resolver via systemd-resolved — the default stub
// resolver on Ubuntu/Debian/Fedora. We attach a per-link DNS server plus
// routing-only domains to the f2f overlay interface. All state is runtime-only
// and re-applied on every dns.Start, so a resolver port that changes across
// restarts needs no persistence.
//
// Requires systemd-resolved ≥ 247 for the IP:PORT DNS-server syntax (our
// resolver listens on a loopback high port, not :53). On older/other resolvers
// resolvectl is absent or rejects the port and the calls return an error, which
// the dns service logs as a non-fatal warning (.f2f names just won't resolve).

var (
	dnsMu      sync.Mutex
	dnsServer  string              // "127.0.0.1:<port>" — f2f's resolver
	dnsZone    string              // "<label>.f2f" routing domain (no ~ prefix)
	dnsDomains = map[string]bool{} // intercept domains routed to us
)

// FlushDNSCache tries to flush the systemd-resolved cache. If systemd-resolved
// is not present (Alpine, plain busybox, etc.) the kernel has no central DNS
// cache to flush — applications do their own caching, out of our reach.
// Best-effort.
func FlushDNSCache() error {
	if err := exec.Command("resolvectl", "flush-caches").Run(); err != nil {
		clog.Info("dns", "resolvectl flush-caches: %v (no systemd-resolved?)", err)
	}
	return nil
}

// f2fLink returns the name of the f2f overlay interface ("f2f0", "f2f1", …),
// created by tun_linux with the "f2f%d" name pattern. Empty when it isn't up.
func f2fLink() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifaces {
		if strings.HasPrefix(ifc.Name, "f2f") {
			return ifc.Name
		}
	}
	return ""
}

// applyResolvedLocked re-issues the complete resolved config for the f2f link:
// one DNS server and every routing domain (the camp zone + intercepts). It sets
// ALL domains in one call because `resolvectl domain` replaces the link's domain
// list wholesale — an incremental add would drop the others. Caller holds dnsMu.
func applyResolvedLocked() error {
	if dnsServer == "" {
		return nil // nothing installed
	}
	link := f2fLink()
	if link == "" {
		return fmt.Errorf("f2f interface not up")
	}
	if _, err := exec.LookPath("resolvectl"); err != nil {
		return fmt.Errorf("resolvectl not found (systemd-resolved required): %w", err)
	}
	if out, err := exec.Command("resolvectl", "dns", link, dnsServer).CombinedOutput(); err != nil {
		return fmt.Errorf("resolvectl dns %s %s: %v: %s", link, dnsServer, err, strings.TrimSpace(string(out)))
	}
	// A leading '~' makes each entry a routing-only domain: queries for it and
	// its subdomains go to our server, and it is NOT added as a search suffix.
	args := []string{"domain", link}
	if dnsZone != "" {
		args = append(args, "~"+dnsZone)
	}
	for d := range dnsDomains {
		args = append(args, "~"+d)
	}
	if out, err := exec.Command("resolvectl", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("resolvectl domain %s: %v: %s", link, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// revertLinkLocked drops all f2f resolved config from the link once nothing is
// routed to us anymore. Caller holds dnsMu.
func revertLinkLocked() {
	dnsServer = ""
	if link := f2fLink(); link != "" {
		_ = exec.Command("resolvectl", "revert", link).Run()
	}
}

// InstallZoneResolver points systemd-resolved at our local DNS for the whole
// <zone>.f2f zone (and its subdomains). Re-applied on every dns.Start.
func InstallZoneResolver(zone, bindAddr string) error {
	if zone == "" {
		return fmt.Errorf("empty zone")
	}
	if _, _, err := net.SplitHostPort(bindAddr); err != nil {
		return fmt.Errorf("split bind addr %q: %w", bindAddr, err)
	}
	dnsMu.Lock()
	defer dnsMu.Unlock()
	dnsServer = bindAddr
	dnsZone = zone + ".f2f"
	return applyResolvedLocked()
}

// ZoneResolverInstalled reports whether we currently route the zone to our
// resolver. In-process state (re-applied each Start), matched against the link
// actually being up.
func ZoneResolverInstalled(zone string) bool {
	dnsMu.Lock()
	defer dnsMu.Unlock()
	return dnsZone == zone+".f2f" && dnsZone != "" && f2fLink() != ""
}

// RemoveZoneResolver stops routing the zone. If no intercept domains remain,
// the link's resolved config is reverted entirely.
func RemoveZoneResolver(zone string) error {
	dnsMu.Lock()
	defer dnsMu.Unlock()
	dnsZone = ""
	if len(dnsDomains) == 0 {
		revertLinkLocked()
		return nil
	}
	return applyResolvedLocked()
}

// InstallDomainResolver routes one exact domain (and its subdomains) to our
// resolver — used for intercept domains resolved on the exit peer.
func InstallDomainResolver(domain, bindAddr string) error {
	if domain == "" || strings.ContainsAny(domain, "/\\ ") {
		return fmt.Errorf("bad resolver domain %q", domain)
	}
	if _, _, err := net.SplitHostPort(bindAddr); err != nil {
		return fmt.Errorf("split bind addr %q: %w", bindAddr, err)
	}
	dnsMu.Lock()
	defer dnsMu.Unlock()
	dnsServer = bindAddr
	dnsDomains[domain] = true
	return applyResolvedLocked()
}

// RemoveDomainResolver stops routing one intercept domain. Reverts the link
// when nothing (zone or domains) is left routed to us.
func RemoveDomainResolver(domain string) error {
	dnsMu.Lock()
	defer dnsMu.Unlock()
	if !dnsDomains[domain] {
		return nil
	}
	delete(dnsDomains, domain)
	if dnsZone == "" && len(dnsDomains) == 0 {
		revertLinkLocked()
		return nil
	}
	return applyResolvedLocked()
}
