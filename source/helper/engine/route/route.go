//go:build darwin

// Package route adds host/CIDR routes through a specific interface and tracks
// what was added so the manager can roll them back on shutdown.
//
// Two route shapes are supported:
//
//   - interface routes: packets to the destination get sent into the named
//     interface (utun). This is the primary path — most intercepts go here.
//   - reject routes (Manager.AddReject): the kernel returns "network
//     unreachable" / ECONNREFUSED to senders. Used for IPv6 intercepts so
//     applications fall back to IPv4 immediately instead of timing out.
package route

import (
	"fmt"
	"net/netip"
	"os/exec"
	"sync"
)

type entry struct {
	prefix netip.Prefix
	reject bool // false = interface-bound, true = -reject
}

// Manager owns the routes that have been added through a given interface.
type Manager struct {
	iface string

	mu    sync.Mutex
	added []entry
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
	m.added = append(m.added, entry{prefix: p, reject: false})
	m.mu.Unlock()
	return nil
}

// AddReject installs a -reject route. The kernel will respond with an ICMP
// unreachable / ECONNREFUSED to any local sender. Use for destinations we
// want to "appear unreachable" to apps (e.g. IPv6 targets so browsers'
// Happy Eyeballs falls back to IPv4 instantly).
func (m *Manager) AddReject(p netip.Prefix) error {
	out, err := exec.Command("/sbin/route", routeRejectArgs("add", p)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("route add -reject %s: %w: %s", p, err, out)
	}
	m.mu.Lock()
	m.added = append(m.added, entry{prefix: p, reject: true})
	m.mu.Unlock()
	return nil
}

// Remove deletes a previously added route, regardless of which Add* variant
// added it. Idempotent for entries not currently tracked.
func (m *Manager) Remove(p netip.Prefix) error {
	args := m.deleteArgsFor(p)
	out, err := exec.Command("/sbin/route", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("route delete %s: %w: %s", p, err, out)
	}
	m.mu.Lock()
	for i, e := range m.added {
		if e.prefix == p {
			m.added = append(m.added[:i], m.added[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	return nil
}

// Cleanup removes every route this manager added. It always attempts every
// entry; the returned slice collects per-entry failures.
func (m *Manager) Cleanup() []error {
	m.mu.Lock()
	entries := m.added
	m.added = nil
	m.mu.Unlock()

	var errs []error
	for _, e := range entries {
		var args []string
		if e.reject {
			args = routeRejectArgs("delete", e.prefix)
		} else {
			args = routeArgs("delete", e.prefix, m.iface)
		}
		if out, err := exec.Command("/sbin/route", args...).CombinedOutput(); err != nil {
			errs = append(errs, fmt.Errorf("route delete %s: %w: %s", e.prefix, err, out))
		}
	}
	return errs
}

// deleteArgsFor picks the correct delete syntax based on how the entry was
// added. Falls back to interface-bound delete for prefixes we don't track.
func (m *Manager) deleteArgsFor(p netip.Prefix) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.added {
		if e.prefix == p && e.reject {
			return routeRejectArgs("delete", p)
		}
	}
	return routeArgs("delete", p, m.iface)
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

// routeRejectArgs builds the argv for installing or removing a -reject
// route. macOS's `route` requires a syntactic gateway even for -reject
// (otherwise it errors with "Invalid argument") — we pass the loopback
// address (::1 / 127.0.0.1) which is irrelevant to the rejected route's
// semantics (the kernel returns ICMP unreachable, packets never actually
// go anywhere). The delete form does not need either the gateway or the
// -reject flag; destination + family is enough for the kernel to find
// the route.
func routeRejectArgs(action string, p netip.Prefix) []string {
	family := "-inet"
	if p.Addr().Is6() {
		family = "-inet6"
	}
	args := []string{"-n", action, family}
	if p.Bits() == p.Addr().BitLen() {
		args = append(args, "-host", p.Addr().String())
	} else {
		args = append(args, "-net", p.String())
	}
	if action == "add" {
		gw := "127.0.0.1"
		if p.Addr().Is6() {
			gw = "::1"
		}
		args = append(args, gw, "-reject")
	}
	return args
}
