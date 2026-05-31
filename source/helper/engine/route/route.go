// Package route adds host/CIDR routes through a specific interface
// and tracks what was added so the manager can roll them back on
// shutdown.
//
// Two route shapes are supported:
//
//   - interface routes: packets to the destination get sent into the
//     named interface (the tunnel). This is the primary path — most
//     intercepts go here.
//   - reject routes (Manager.AddReject): the kernel returns "network
//     unreachable" / ECONNREFUSED to senders. Used for IPv6
//     intercepts so applications fall back to IPv4 immediately
//     instead of timing out.
package route

import (
	"net/netip"
	"sync"

	"github.com/vseplet/f2f/source/helper/platform"
)

type entry struct {
	prefix netip.Prefix
	reject bool // false = interface-bound, true = -reject
}

// Manager owns the routes that have been added through a given
// interface.
type Manager struct {
	iface string

	mu    sync.Mutex
	added []entry
}

func New(iface string) *Manager {
	return &Manager{iface: iface}
}

// Add installs a route for prefix p pointing at the manager's
// interface.
func (m *Manager) Add(p netip.Prefix) error {
	if err := platform.RouteAddIface(p, m.iface); err != nil {
		return err
	}
	m.mu.Lock()
	m.added = append(m.added, entry{prefix: p, reject: false})
	m.mu.Unlock()
	return nil
}

// AddReject installs a -reject route. Use for destinations we want
// to "appear unreachable" to apps (e.g. IPv6 targets so browsers'
// Happy Eyeballs falls back to IPv4 instantly).
func (m *Manager) AddReject(p netip.Prefix) error {
	if err := platform.RouteAddReject(p); err != nil {
		return err
	}
	m.mu.Lock()
	m.added = append(m.added, entry{prefix: p, reject: true})
	m.mu.Unlock()
	return nil
}

// Remove deletes a previously added route, regardless of which Add*
// variant added it. Idempotent for entries not currently tracked.
func (m *Manager) Remove(p netip.Prefix) error {
	if err := m.removeOSEntry(p, m.lookupReject(p)); err != nil {
		return err
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

// Cleanup removes every route this manager added. It always attempts
// every entry; the returned slice collects per-entry failures.
func (m *Manager) Cleanup() []error {
	m.mu.Lock()
	entries := m.added
	m.added = nil
	m.mu.Unlock()

	var errs []error
	for _, e := range entries {
		if err := m.removeOSEntry(e.prefix, e.reject); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func (m *Manager) removeOSEntry(p netip.Prefix, reject bool) error {
	if reject {
		return platform.RouteDeleteReject(p)
	}
	return platform.RouteDeleteIface(p, m.iface)
}

// lookupReject reports whether a tracked entry for p was added as a
// reject route. Falls back to false (interface-bound delete syntax)
// for untracked prefixes.
func (m *Manager) lookupReject(p netip.Prefix) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.added {
		if e.prefix == p {
			return e.reject
		}
	}
	return false
}
