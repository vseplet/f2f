//go:build darwin

// Package egress owns the macOS-side system setup that lets the egress
// instance forward tunnel traffic out to the real internet:
//
//  1. ip.forwarding sysctl flipped to 1 (saved + restored on close)
//  2. pf enabled via a reference-counted token (released on close)
//  3. a NAT rule installed in the `com.apple/f2f-mac` anchor, which
//     macOS's default /etc/pf.conf evaluates via its `nat-anchor
//     "com.apple/*"` wildcard
//
// All changes are persisted to /var/run/f2f-mac.egress.json before they
// happen; on startup any leftover state from a dead prior instance is
// rolled back automatically, and a one-line manual recovery command is
// documented in the README for the case of SIGKILL.
package egress

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
)

const (
	// anchorName lives under the com.apple/* namespace that the default
	// /etc/pf.conf already evaluates — so we don't need to touch pf.conf
	// itself. The name is unique enough to avoid colliding with Apple's
	// own VPN/NetworkExtension entries.
	anchorName = "com.apple/f2f-mac"

	// statePath records what we changed, so a crashed process can be
	// cleaned up by the next invocation.
	statePath = "/var/run/f2f-mac.egress.json"

	sysctlForwarding = "net.inet.ip.forwarding"
)

// state is what we persist between invocations to survive crashes.
type state struct {
	PID           int    `json:"pid"`
	PfToken       string `json:"pf_token"`
	OldForwarding string `json:"old_forwarding"`
	Anchor        string `json:"anchor"`
	EgressIface   string `json:"egress_iface"`
	Subnet        string `json:"subnet"`
}

// Egress holds the live system-setup. Call Close to roll everything back.
type Egress struct {
	state state
}

// Open performs all three setup steps in order. If any step fails, the
// already-applied changes are rolled back before the error returns.
func Open(iface string, subnet netip.Prefix) (*Egress, error) {
	if err := sweepLeftover(); err != nil {
		log.Printf("WARN: pre-flight egress cleanup: %v", err)
	}

	oldFwd, err := getSysctl(sysctlForwarding)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", sysctlForwarding, err)
	}

	e := &Egress{
		state: state{
			PID:           os.Getpid(),
			OldForwarding: oldFwd,
			Anchor:        anchorName,
			EgressIface:   iface,
			Subnet:        subnet.String(),
		},
	}

	if err := setSysctl(sysctlForwarding, "1"); err != nil {
		return nil, fmt.Errorf("enable forwarding: %w", err)
	}

	token, err := pfEnable()
	if err != nil {
		_ = setSysctl(sysctlForwarding, oldFwd)
		return nil, fmt.Errorf("enable pf: %w", err)
	}
	e.state.PfToken = token

	rule := fmt.Sprintf("nat on %s from %s to any -> (%s)\n", iface, subnet, iface)
	if err := pfLoadAnchor(anchorName, rule); err != nil {
		_ = pfDisable(token)
		_ = setSysctl(sysctlForwarding, oldFwd)
		return nil, fmt.Errorf("load pf anchor: %w", err)
	}

	if err := writeState(e.state); err != nil {
		log.Printf("WARN: write egress state file: %v", err)
	}
	return e, nil
}

// Close rolls back everything Open did. It runs every step independently
// so a failure in one does not block the others.
func (e *Egress) Close() error {
	var errs []error
	if err := pfFlushAnchor(e.state.Anchor); err != nil {
		errs = append(errs, fmt.Errorf("flush pf anchor: %w", err))
	}
	if e.state.PfToken != "" {
		if err := pfDisable(e.state.PfToken); err != nil {
			errs = append(errs, fmt.Errorf("pf disable: %w", err))
		}
	}
	if e.state.OldForwarding != "" {
		if err := setSysctl(sysctlForwarding, e.state.OldForwarding); err != nil {
			errs = append(errs, fmt.Errorf("restore forwarding: %w", err))
		}
	}
	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("remove state file: %w", err))
	}
	return errors.Join(errs...)
}

// Anchor returns the pf anchor name this Egress installs into. Exported
// only so tests / callers can reference it in log messages without
// hardcoding the constant.
func (e *Egress) Anchor() string { return e.state.Anchor }

// sweepLeftover handles the case where a prior invocation crashed (or
// was kill -9'd) and never ran Close. Reads the state file, checks the
// pid is gone, and tries to roll back what was recorded.
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
			"another f2f-mac is using egress, or remove %s manually", s.PID, statePath)
	}
	log.Printf("found stale egress state from pid %d, rolling back", s.PID)
	if s.Anchor != "" {
		if err := pfFlushAnchor(s.Anchor); err != nil {
			log.Printf("WARN: sweep flush anchor: %v", err)
		}
	}
	if s.PfToken != "" {
		if err := pfDisable(s.PfToken); err != nil {
			log.Printf("WARN: sweep pf disable: %v", err)
		}
	}
	if s.OldForwarding != "" {
		if err := setSysctl(sysctlForwarding, s.OldForwarding); err != nil {
			log.Printf("WARN: sweep restore forwarding: %v", err)
		}
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

// processAlive returns true if a process with the given pid exists.
// On Unix, sending signal 0 is the canonical "does this pid exist" probe.
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

func getSysctl(key string) (string, error) {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", key).Output()
	if err != nil {
		return "", fmt.Errorf("sysctl -n %s: %w", key, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func setSysctl(key, value string) error {
	out, err := exec.Command("/usr/sbin/sysctl", "-w", key+"="+value).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sysctl -w %s=%s: %w: %s", key, value, err, out)
	}
	return nil
}

// pfTokenRE extracts the numeric token from `pfctl -E` output, which looks
// like "Token : 1234567890" on one of its lines.
var pfTokenRE = regexp.MustCompile(`Token\s*:\s*(\d+)`)

// pfEnable enables pf via reference-counted "-E" semantics: pf stays on
// until every token taken via -E has been released via -X. The returned
// string is the token we need to release later.
func pfEnable() (string, error) {
	out, err := exec.Command("/sbin/pfctl", "-E").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("pfctl -E: %w: %s", err, out)
	}
	m := pfTokenRE.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("pfctl -E: no token in output: %s", out)
	}
	return string(m[1]), nil
}

func pfDisable(token string) error {
	out, err := exec.Command("/sbin/pfctl", "-X", token).CombinedOutput()
	if err != nil {
		return fmt.Errorf("pfctl -X %s: %w: %s", token, err, out)
	}
	return nil
}

func pfLoadAnchor(anchor, rules string) error {
	cmd := exec.Command("/sbin/pfctl", "-a", anchor, "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pfctl -a %s -f -: %w: %s", anchor, err, out)
	}
	return nil
}

func pfFlushAnchor(anchor string) error {
	out, err := exec.Command("/sbin/pfctl", "-a", anchor, "-F", "all").CombinedOutput()
	if err != nil {
		return fmt.Errorf("pfctl -a %s -F all: %w: %s", anchor, err, out)
	}
	return nil
}
