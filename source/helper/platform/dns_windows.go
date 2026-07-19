//go:build windows

package platform

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
)

// Windows split-DNS goes through the NRPT (Name Resolution Policy Table): a rule
// maps a namespace suffix to the servers that must answer for it — exactly the
// per-zone routing /etc/resolver gives us on macOS.
//
// The catch that shapes this file: an NRPT rule takes a bare server IP and always
// queries port 53. There is no "IP:port" form anywhere in the Windows resolver
// stack, which is why DNSBindAddr pins our resolver to :53 instead of using an
// ephemeral port like the Unix platforms do.
//
// NRPT rules are machine-wide and survive the process, so Remove* must actually
// clean up — a leftover rule would blackhole the namespace for the whole box.

// DNSBindAddr pins the resolver to loopback :53; see the note above on why an
// ephemeral port cannot work here.
func DNSBindAddr() string { return "127.0.0.1:53" }

// Installed namespaces are mirrored in memory because ZoneResolverInstalled is
// polled by the UI on every status refresh, and that call runs while the engine
// mutex is held. Asking Windows (a PowerShell round-trip, ~seconds) from there
// stalls every other engine user behind it — the bus resolver included, which
// then can't dial peers at all. Reads must stay allocation-cheap; only the
// mutating calls are allowed to spawn a process.
var (
	nrptMu      sync.Mutex
	nrptZone    string // "<label>.f2f" currently routed to us, "" if none
	nrptDomains = map[string]bool{}
)

// powershell runs a one-liner and returns its trimmed output.
func powershell(script string) (string, error) {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// FlushDNSCache clears the client resolver cache so stale negative answers don't
// pin applications to NXDOMAIN once we start answering for a namespace.
func FlushDNSCache() error {
	_ = exec.Command("ipconfig", "/flushdns").Run()
	return nil
}

// nrptNamespace renders a domain in NRPT form: a leading dot matches the domain
// and everything below it.
func nrptNamespace(domain string) string {
	return "." + strings.Trim(strings.TrimSuffix(domain, "."), ".")
}

// addNrptRule points one namespace at our resolver. bindAddr's port is dropped —
// NRPT is IP-only, and DNSBindAddr guarantees that port is 53.
func addNrptRule(namespace, bindAddr string) error {
	host, _, err := net.SplitHostPort(bindAddr)
	if err != nil {
		return fmt.Errorf("split bind addr %q: %w", bindAddr, err)
	}
	// Replace rather than stack: a restart would otherwise pile up duplicate
	// rules for the same namespace.
	if err := removeNrptRule(namespace); err != nil {
		return err
	}
	if _, err := powershell(fmt.Sprintf(
		`Add-DnsClientNrptRule -Namespace '%s' -NameServers '%s' -ErrorAction Stop`, namespace, host)); err != nil {
		return fmt.Errorf("Add-DnsClientNrptRule %s -> %s: %w", namespace, host, err)
	}
	return nil
}

func removeNrptRule(namespace string) error {
	if _, err := powershell(fmt.Sprintf(
		`Get-DnsClientNrptRule | Where-Object { $_.Namespace -eq '%s' } | `+
			`ForEach-Object { Remove-DnsClientNrptRule -Name $_.Name -Force -ErrorAction Stop }`,
		namespace)); err != nil {
		return fmt.Errorf("Remove-DnsClientNrptRule %s: %w", namespace, err)
	}
	return nil
}

// InstallZoneResolver routes <zone>.f2f and everything under it to our resolver.
func InstallZoneResolver(zone, bindAddr string) error {
	if zone == "" {
		return fmt.Errorf("empty zone")
	}
	if err := addNrptRule(nrptNamespace(zone+".f2f"), bindAddr); err != nil {
		return err
	}
	nrptMu.Lock()
	nrptZone = zone + ".f2f"
	nrptMu.Unlock()
	return FlushDNSCache()
}

// ZoneResolverInstalled answers from the in-memory mirror — see the note on
// nrptZone for why this must not touch Windows.
func ZoneResolverInstalled(zone string) bool {
	if zone == "" {
		return false
	}
	nrptMu.Lock()
	defer nrptMu.Unlock()
	return nrptZone != "" && nrptZone == zone+".f2f"
}

func RemoveZoneResolver(zone string) error {
	if zone == "" {
		return nil
	}
	if err := removeNrptRule(nrptNamespace(zone + ".f2f")); err != nil {
		return err
	}
	nrptMu.Lock()
	nrptZone = ""
	nrptMu.Unlock()
	return FlushDNSCache()
}

// InstallDomainResolver routes one intercept domain (and its subdomains) to our
// resolver, so it answers with the exit peer's view of the name.
func InstallDomainResolver(domain, bindAddr string) error {
	if domain == "" || strings.ContainsAny(domain, `/\ '`) {
		return fmt.Errorf("bad resolver domain %q", domain)
	}
	if err := addNrptRule(nrptNamespace(domain), bindAddr); err != nil {
		return err
	}
	nrptMu.Lock()
	nrptDomains[domain] = true
	nrptMu.Unlock()
	return FlushDNSCache()
}

func RemoveDomainResolver(domain string) error {
	if domain == "" || strings.ContainsAny(domain, `/\ '`) {
		return nil
	}
	if err := removeNrptRule(nrptNamespace(domain)); err != nil {
		return err
	}
	nrptMu.Lock()
	delete(nrptDomains, domain)
	nrptMu.Unlock()
	return FlushDNSCache()
}
