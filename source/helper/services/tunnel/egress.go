// egress.go owns the system setup that lets this node forward
// camp-overlay traffic to the real internet:
//
//  1. ip-forwarding sysctl flipped to 1 (saved + restored on close)
//  2. OS packet-filter engine enabled (macOS pf only; no-op on Linux)
//  3. a NAT rule installed via the platform package
//
// All changes are persisted to /var/run/f2f.egress.json before they
// happen; on startup any leftover state from a dead prior instance
// is rolled back automatically.
//
// Conceptually a sibling of anchor.go (pf-anchor lifecycle) — both
// are pf-state owners; both live in services/tunnel because tunnel
// is the application-level owner of "how this node participates in
// routing".

package tunnel

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"syscall"

	"github.com/vseplet/f2f/source/helper/platform"
)

const egressStatePath = "/var/run/f2f.egress.json"

type egressState struct {
	PID           int    `json:"pid"`
	FilterToken   string `json:"filter_token"`
	OldForwarding string `json:"old_forwarding"`
	EgressIface   string `json:"egress_iface"`
	Subnet        string `json:"subnet"`
}

// egress holds the live system-setup. Internal — Service owns
// exactly one at a time, opened in Service.Start and closed in
// Service.Stop.
type egress struct {
	state egressState
}

// openEgress performs the three setup steps in order. If any step
// fails, the already-applied changes are rolled back before the
// error returns.
func openEgress(iface string, subnet netip.Prefix) (*egress, error) {
	if err := sweepEgressLeftover(); err != nil {
		log.Printf("WARN: pre-flight egress cleanup: %v", err)
	}

	oldFwd, err := platform.GetIPForwarding()
	if err != nil {
		return nil, fmt.Errorf("read ip-forwarding: %w", err)
	}

	e := &egress{
		state: egressState{
			PID:           os.Getpid(),
			OldForwarding: oldFwd,
			EgressIface:   iface,
			Subnet:        subnet.String(),
		},
	}

	if err := platform.SetIPForwarding("1"); err != nil {
		return nil, fmt.Errorf("enable forwarding: %w", err)
	}

	token, err := platform.EnableFilterEngine()
	if err != nil {
		_ = platform.SetIPForwarding(oldFwd)
		return nil, fmt.Errorf("enable filter engine: %w", err)
	}
	e.state.FilterToken = string(token)

	if err := platform.InstallNAT(platform.NATRules{EgressIface: iface, Subnet: subnet}); err != nil {
		_ = platform.DisableFilterEngine(token)
		_ = platform.SetIPForwarding(oldFwd)
		return nil, fmt.Errorf("install NAT: %w", err)
	}

	if err := writeEgressState(e.state); err != nil {
		log.Printf("WARN: write egress state file: %v", err)
	}
	return e, nil
}

// Close rolls back everything Open did. It runs every step
// independently so a failure in one does not block the others.
func (e *egress) close() error {
	var errs []error
	if err := platform.RemoveNAT(); err != nil {
		errs = append(errs, fmt.Errorf("remove NAT: %w", err))
	}
	if e.state.FilterToken != "" {
		if err := platform.DisableFilterEngine(platform.FilterEngineToken(e.state.FilterToken)); err != nil {
			errs = append(errs, fmt.Errorf("disable filter engine: %w", err))
		}
	}
	if e.state.OldForwarding != "" {
		if err := platform.SetIPForwarding(e.state.OldForwarding); err != nil {
			errs = append(errs, fmt.Errorf("restore forwarding: %w", err))
		}
	}
	if err := os.Remove(egressStatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("remove state file: %w", err))
	}
	return errors.Join(errs...)
}

// sweepEgressLeftover handles the case where a prior invocation crashed
// (or was kill -9'd) and never ran Close. Reads the state file,
// checks the pid is gone, and tries to roll back what was recorded.
func sweepEgressLeftover() error {
	s, err := readEgressState()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read state: %w", err)
	}
	if processAlive(s.PID) {
		return fmt.Errorf("state file references running pid %d — refusing to touch; "+
			"another f2f is using egress, or remove %s manually", s.PID, egressStatePath)
	}
	log.Printf("found stale egress state from pid %d, rolling back", s.PID)
	if err := platform.RemoveNAT(); err != nil {
		log.Printf("WARN: sweep remove NAT: %v", err)
	}
	if s.FilterToken != "" {
		if err := platform.DisableFilterEngine(platform.FilterEngineToken(s.FilterToken)); err != nil {
			log.Printf("WARN: sweep disable filter engine: %v", err)
		}
	}
	if s.OldForwarding != "" {
		if err := platform.SetIPForwarding(s.OldForwarding); err != nil {
			log.Printf("WARN: sweep restore forwarding: %v", err)
		}
	}
	return os.Remove(egressStatePath)
}

func readEgressState() (egressState, error) {
	var s egressState
	data, err := os.ReadFile(egressStatePath)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("parse state %s: %w", egressStatePath, err)
	}
	return s, nil
}

func writeEgressState(s egressState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(egressStatePath, data, 0o644)
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
