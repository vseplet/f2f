// Package firewall installs an inbound filter on the tunnel
// interface that default-denies traffic to our tunnel_ip and
// explicitly allows a small set of ports — f2f's own (HTTP API,
// proxies, BT) plus user-defined ones from the Tunnel tab UI.
//
// Without this, any service the user has listening on 0.0.0.0
// (sshd, postgres, dev servers) becomes reachable from every camp
// peer at <tunnel_ip>:<port>. The firewall makes the tunnel as
// restrictive as any incoming-from-internet interface by default.
//
// State is persisted at /var/run/f2f-mac.firewall.json so a crashed
// process can be cleaned up by the next invocation. The actual
// pf/nft work is in platform; this package owns the high-level
// lifecycle (sweep, install, persist, teardown).
package firewall

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"syscall"

	"github.com/vseplet/f2f/source/helper/platform"
)

const statePath = "/var/run/f2f-mac.firewall.json"

// PortRule is one allow entry. Protocol is "tcp" or "udp".
type PortRule struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
}

// Firewall holds the live firewall state. Call Close to remove it.
type Firewall struct {
	state state
}

type state struct {
	PID      int        `json:"pid"`
	Iface    string     `json:"iface"`
	TunnelIP string     `json:"tunnel_ip"`
	Allowed  []PortRule `json:"allowed"`
}

// Open installs the firewall on iface, scoped to packets destined
// for tunnelIP. Packets to other peers or to public IPs being
// forwarded through egress are unaffected.
//
// If a stale state file exists from a dead prior process, it is
// cleaned up first.
func Open(iface, tunnelIP string, allowed []PortRule) (*Firewall, error) {
	if iface == "" {
		return nil, errors.New("firewall: empty iface")
	}
	if tunnelIP == "" {
		return nil, errors.New("firewall: empty tunnel_ip")
	}
	if err := sweepLeftover(); err != nil {
		log.Printf("WARN: pre-flight firewall cleanup: %v", err)
	}
	f := &Firewall{
		state: state{
			PID:      os.Getpid(),
			Iface:    iface,
			TunnelIP: tunnelIP,
			Allowed:  dedup(allowed),
		},
	}
	if err := platform.InstallFirewall(policyFrom(f.state)); err != nil {
		return nil, fmt.Errorf("install firewall: %w", err)
	}
	if err := writeState(f.state); err != nil {
		log.Printf("WARN: write firewall state file: %v", err)
	}
	return f, nil
}

// Apply re-installs the firewall with a new allow list. The previous
// ruleset is replaced atomically. iface and tunnelIP are taken from
// the live state — they don't change between calls.
func (f *Firewall) Apply(allowed []PortRule) error {
	allowed = dedup(allowed)
	f.state.Allowed = allowed
	if err := platform.InstallFirewall(policyFrom(f.state)); err != nil {
		return fmt.Errorf("reinstall firewall: %w", err)
	}
	if err := writeState(f.state); err != nil {
		log.Printf("WARN: write firewall state file: %v", err)
	}
	return nil
}

// Close removes the firewall. Idempotent.
func (f *Firewall) Close() error {
	var errs []error
	if err := platform.RemoveFirewall(); err != nil {
		errs = append(errs, fmt.Errorf("remove firewall: %w", err))
	}
	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("remove state file: %w", err))
	}
	return errors.Join(errs...)
}

func policyFrom(s state) platform.FirewallPolicy {
	p := platform.FirewallPolicy{
		Iface:    s.Iface,
		TunnelIP: s.TunnelIP,
	}
	for _, r := range s.Allowed {
		switch r.Protocol {
		case "tcp":
			p.AllowTCP = append(p.AllowTCP, r.Port)
		case "udp":
			p.AllowUDP = append(p.AllowUDP, r.Port)
		}
	}
	return p
}

// dedup removes (port, proto) duplicates, normalises protocol to
// lower case, and drops invalid entries.
func dedup(in []PortRule) []PortRule {
	seen := make(map[string]struct{}, len(in))
	out := make([]PortRule, 0, len(in))
	for _, r := range in {
		proto := strings.ToLower(strings.TrimSpace(r.Protocol))
		if proto != "tcp" && proto != "udp" {
			continue
		}
		if r.Port <= 0 || r.Port > 65535 {
			continue
		}
		key := fmt.Sprintf("%d/%s", r.Port, proto)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, PortRule{Port: r.Port, Protocol: proto})
	}
	return out
}

func sweepLeftover() error {
	s, err := readState()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read state: %w", err)
	}
	if processAlive(s.PID) {
		return fmt.Errorf("state file references running pid %d — refusing to touch; "+
			"another f2f is using the firewall, or remove %s manually", s.PID, statePath)
	}
	log.Printf("found stale firewall state from pid %d, rolling back", s.PID)
	if err := platform.RemoveFirewall(); err != nil {
		log.Printf("WARN: sweep remove firewall: %v", err)
	}
	return os.Remove(statePath)
}

func readState() (state, error) {
	var s state
	data, err := os.ReadFile(statePath)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("parse state %s: %w", statePath, err)
	}
	return s, nil
}

func writeState(s state) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath, data, 0o644)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
