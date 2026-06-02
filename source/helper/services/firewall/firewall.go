// Package firewall is the user-facing service layer for the inbound
// utun firewall — it owns the user-configured allow list, persistence
// to the camp config, and re-applying the pf anchor on change.
//
// The low-level pf wrapper lives in helper/engine/firewall (imported
// here as enginefw) and is owned by Engine — this service borrows the
// handle through engine.FirewallHandle() and uses it to push fresh
// rule sets.
//
// The service is created once at process start (from main.go) and
// outlives camp transitions. It is safe to call its methods even when
// the engine is stopped — UserPorts returns the on-disk state, and
// SetUserPorts returns an error indicating the engine isn't running.
package firewall

import (
	"errors"
	"fmt"
	"strings"

	"github.com/vseplet/f2f/source/helper/config"
	enginefw "github.com/vseplet/f2f/source/helper/engine/firewall"
)

// ErrEngineNotRunning is returned by SetUserPorts when the engine
// hasn't been Start'ed yet (no camp loaded → no per-camp config file
// to persist to). UI shows this as "start the tunnel first".
var ErrEngineNotRunning = errors.New("firewall: engine not running")

// Engine is the small subset of the real *engine.Engine surface the
// service depends on. Declaring it as an interface keeps services/
// from importing engine (no import cycle risk) and makes the service
// trivially testable with a fake.
type Engine interface {
	FirewallHandle() *enginefw.Firewall
	CampFirewall() []config.Firewall
	SetCampFirewall(list []config.Firewall) error
}

// Service is the per-process firewall manager. Construct once in
// main.go and pass to anything that needs to read or mutate the user
// rule list (currently just the web API handlers).
type Service struct {
	eng Engine
}

// New constructs a Service. The engine must outlive the service but
// need not be Start'ed yet — accessors degrade gracefully when the
// engine is down.
func New(eng Engine) *Service {
	return &Service{eng: eng}
}

// BuiltinPorts returns the always-on f2f-internal allow list as the
// shape the UI expects. Read-only from the UI's perspective; the
// underlying rules live in enginefw.BuiltinRules.
func (s *Service) BuiltinPorts() []config.Firewall {
	out := make([]config.Firewall, len(enginefw.BuiltinRules))
	for i, r := range enginefw.BuiltinRules {
		out[i] = config.Firewall{
			Port:        r.Port,
			Protocol:    r.Protocol,
			Description: enginefw.BuiltinLabel(r.Port, r.Protocol),
			Enabled:     true,
		}
	}
	return out
}

// UserPorts returns a copy of the user-configured allow list, sourced
// from the engine's current camp config. Returns nil if the engine
// isn't running.
func (s *Service) UserPorts() []config.Firewall {
	return s.eng.CampFirewall()
}

// SetUserPorts replaces the user allow list, persists it into the
// per-camp config on disk, and re-applies the pf anchor with builtin
// + enabled user rules. Idempotent — safe to call on every UI save.
// Returns ErrEngineNotRunning if the engine isn't up.
func (s *Service) SetUserPorts(list []config.Firewall) error {
	cleaned := CleanList(list)
	if err := s.eng.SetCampFirewall(cleaned); err != nil {
		return err
	}
	fw := s.eng.FirewallHandle()
	if fw == nil {
		// pf-load failed at engine.Start; rules are persisted but not
		// enforced. Next engine restart will retry.
		return nil
	}
	if err := fw.Apply(MergeRules(cleaned)); err != nil {
		return fmt.Errorf("firewall: apply: %w", err)
	}
	return nil
}

// Active reports whether the pf anchor for the inbound utun filter is
// currently loaded — true means user-toggled rules are actually
// enforced by the kernel, false means the engine isn't running (or
// pf-load failed at startup and we fell back to no filtering).
func (s *Service) Active() bool {
	return s.eng.FirewallHandle() != nil
}

// MergeRules combines the always-on builtins with the enabled user
// entries into the kernel-level rule list passed to pf. Exported so
// engine.Start can use the same merge logic for its initial apply
// (avoiding two divergent code paths for "builtin + user-enabled").
func MergeRules(user []config.Firewall) []enginefw.PortRule {
	out := make([]enginefw.PortRule, 0, len(enginefw.BuiltinRules)+len(user))
	out = append(out, enginefw.BuiltinRules...)
	for _, p := range user {
		if !p.Enabled {
			continue
		}
		out = append(out, enginefw.PortRule{Port: p.Port, Protocol: p.Protocol})
	}
	return out
}

// CleanList validates and deduplicates a user-supplied allow list.
// Drops entries with unknown protocols, out-of-range ports, and
// duplicate (port, protocol) pairs. Exported so engine.Start can
// sanitise camp-loaded rules through the same code path.
func CleanList(in []config.Firewall) []config.Firewall {
	seen := make(map[string]struct{}, len(in))
	out := make([]config.Firewall, 0, len(in))
	for _, p := range in {
		proto := strings.ToLower(strings.TrimSpace(p.Protocol))
		if proto != "tcp" && proto != "udp" {
			continue
		}
		if p.Port <= 0 || p.Port > 65535 {
			continue
		}
		key := fmt.Sprintf("%d/%s", p.Port, proto)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, config.Firewall{
			Port:        p.Port,
			Protocol:    proto,
			Description: strings.TrimSpace(p.Description),
			Enabled:     p.Enabled,
		})
	}
	return out
}
