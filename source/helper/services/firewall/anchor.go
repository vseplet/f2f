// anchor.go owns the pf-anchor lifecycle: install/replace/teardown
// plus crash-recovery via a state file at /var/run/f2f.firewall.json.
//
// The actual pfctl (Darwin) / nft (Linux) commands live in
// helper/platform — this file is the application-level glue that
// remembers what was installed and sweeps it on the next start if
// the prior process died mid-run.

package firewall

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/vseplet/f2f/source/helper/clog"
	"github.com/vseplet/f2f/source/helper/platform"
)

const statePath = "/var/run/f2f.firewall.json"

// PortRule is one allow entry — the kernel-level shape passed to
// platform.InstallFirewall. UI-side rules (config.Firewall) carry
// extra description/enabled metadata; PortRule is the minimal pair
// the pf layer cares about.
type PortRule struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
}

// anchor is the live pf-anchor handle. Internal — the Service owns
// the only one, callers go through Service.Start/SetUserPorts.
type anchor struct {
	state anchorState
}

type anchorState struct {
	PID      int        `json:"pid"`
	Iface    string     `json:"iface"`
	TunnelIP string     `json:"tunnel_ip"`
	Allowed  []PortRule `json:"allowed"`
}

// openAnchor installs the firewall on iface, scoped to packets
// destined for tunnelIP. Packets to other peers or to public IPs
// being forwarded through egress are unaffected. If a stale state
// file exists from a dead prior process it is swept first.
func openAnchor(iface, tunnelIP string, allowed []PortRule) (*anchor, error) {
	if iface == "" {
		return nil, errors.New("firewall: empty iface")
	}
	if tunnelIP == "" {
		return nil, errors.New("firewall: empty tunnel_ip")
	}
	if err := sweepLeftover(); err != nil {
		clog.Warn("firewall", "pre-flight firewall cleanup: %v", err)
	}
	a := &anchor{
		state: anchorState{
			PID:      os.Getpid(),
			Iface:    iface,
			TunnelIP: tunnelIP,
			Allowed:  dedupRules(allowed),
		},
	}
	if err := platform.InstallFirewall(policyFrom(a.state)); err != nil {
		return nil, fmt.Errorf("install firewall: %w", err)
	}
	if err := writeAnchorState(a.state); err != nil {
		clog.Warn("firewall", "write firewall state file: %v", err)
	}
	return a, nil
}

func (a *anchor) apply(allowed []PortRule) error {
	allowed = dedupRules(allowed)
	a.state.Allowed = allowed
	if err := platform.InstallFirewall(policyFrom(a.state)); err != nil {
		return fmt.Errorf("reinstall firewall: %w", err)
	}
	if err := writeAnchorState(a.state); err != nil {
		clog.Warn("firewall", "write firewall state file: %v", err)
	}
	return nil
}

func (a *anchor) close() error {
	var errs []error
	if err := platform.RemoveFirewall(); err != nil {
		errs = append(errs, fmt.Errorf("remove firewall: %w", err))
	}
	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("remove state file: %w", err))
	}
	return errors.Join(errs...)
}

func policyFrom(s anchorState) platform.FirewallPolicy {
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

// dedupRules removes (port, proto) duplicates, normalises protocol
// to lower case, and drops invalid entries.
func dedupRules(in []PortRule) []PortRule {
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
	s, err := readAnchorState()
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
	clog.Info("firewall", "found stale firewall state from pid %d, rolling back", s.PID)
	if err := platform.RemoveFirewall(); err != nil {
		clog.Warn("firewall", "sweep remove firewall: %v", err)
	}
	return os.Remove(statePath)
}

func readAnchorState() (anchorState, error) {
	var s anchorState
	data, err := os.ReadFile(statePath)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("parse state %s: %w", statePath, err)
	}
	return s, nil
}

func writeAnchorState(s anchorState) error {
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
