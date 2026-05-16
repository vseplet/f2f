//go:build darwin

// Package route adds host/CIDR routes through a specific interface and tracks
// what was added so the manager can roll them back on shutdown.
package route

import (
	"fmt"
	"net/netip"
	"os/exec"
	"sync"
)

// Manager owns the routes that have been added through a given interface.
type Manager struct {
	iface string

	mu    sync.Mutex
	added []netip.Prefix
}

func New(iface string) *Manager {
	return &Manager{iface: iface}
}

// Add installs a route for prefix p, pointing at the manager's interface.
func (m *Manager) Add(p netip.Prefix) error {
	out, err := exec.Command("/sbin/route", routeArgs("add", p, m.iface)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("route add %s: %w: %s", p, err, out)
	}
	m.mu.Lock()
	m.added = append(m.added, p)
	m.mu.Unlock()
	return nil
}

// Cleanup removes every route this manager added. It always attempts every
// entry; the returned slice collects per-entry failures.
func (m *Manager) Cleanup() []error {
	m.mu.Lock()
	prefixes := m.added
	m.added = nil
	m.mu.Unlock()

	var errs []error
	for _, p := range prefixes {
		if out, err := exec.Command("/sbin/route", routeArgs("delete", p, m.iface)...).CombinedOutput(); err != nil {
			errs = append(errs, fmt.Errorf("route delete %s: %w: %s", p, err, out))
		}
	}
	return errs
}

func routeArgs(action string, p netip.Prefix, iface string) []string {
	family := "-inet"
	if p.Addr().Is6() {
		family = "-inet6"
	}
	// macOS `route` uses -host for single addresses and -net for CIDRs.
	if p.Bits() == p.Addr().BitLen() {
		return []string{"-n", action, family, "-host", p.Addr().String(), "-interface", iface}
	}
	return []string{"-n", action, family, "-net", p.String(), "-interface", iface}
}
