// Package firewall installs an inbound filter on the tunnel
// interface that default-denies traffic to our tunnel_ip and
// explicitly allows a small set of ports — f2f's own engine ports
// (HTTP API, proxies, BitTorrent) plus user-defined ones from the
// Tunnel tab UI.
//
// Without this, any service the user has listening on 0.0.0.0
// (sshd, postgres, dev servers) becomes reachable from every camp
// peer at <tunnel_ip>:<port>. The firewall makes the tunnel as
// restrictive as any incoming-from-internet interface by default.
//
// Layering:
//
//   - helper/platform/firewall_*.go: OS primitives (pfctl on Darwin,
//     nft on Linux). Pure system commands.
//   - this package's anchor.go: pf-anchor lifecycle (open/apply/close)
//     plus state-file recovery for crashed prior processes.
//   - this package's Service: user-facing CRUD on the allow list,
//     persistence into the camp config, and re-applying the anchor.
package firewall

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/vseplet/f2f/source/helper/config"
)

// ErrEngineNotRunning is returned by SetUserPorts when the engine
// hasn't been Start'ed yet (no camp loaded → no per-camp config file
// to persist to). UI shows this as "start the tunnel first".
var ErrEngineNotRunning = errors.New("firewall: engine not running")

// BuiltinRules are the ports f2f's own engine listens on over the
// tunnel. They are always allowed by the inbound filter regardless
// of user settings, because the engine itself is the consumer:
//
//   - 2202/tcp — HTTP API used by the web UI and by peer-to-peer
//     signal polling over utun.
//   - 80/tcp, 443/tcp — reverse proxy entry points that forward to
//     locally-registered domains.
//   - 6881/tcp + 6881/udp — BitTorrent peer wire and uTP for the
//     drop subsystem.
//
// Keep in sync with web.Server and the torrent client.
var BuiltinRules = []PortRule{
	{Port: 2202, Protocol: "tcp"},
	{Port: 80, Protocol: "tcp"},
	{Port: 443, Protocol: "tcp"},
	{Port: 6881, Protocol: "tcp"},
	{Port: 6881, Protocol: "udp"},
}

// BuiltinLabel returns a human-readable description for a builtin
// (port, proto) pair, intended for the UI's "always-on" rule list.
// Returns "" for non-builtin combinations.
func BuiltinLabel(port int, proto string) string {
	switch {
	case port == 2202 && proto == "tcp":
		return "f2f HTTP API"
	case port == 80 && proto == "tcp":
		return "f2f HTTP proxy"
	case port == 443 && proto == "tcp":
		return "f2f HTTPS proxy"
	case port == 6881:
		return "f2f BitTorrent"
	}
	return ""
}

// Service is the per-process firewall manager. Owns the pf-anchor
// lifecycle and reads/writes its own slice of camp config (the user
// firewall rule list) directly through *config.Store — no engine
// indirection. The camp identity is picked up at Start(iface,
// tunnelIP, campID) so the service is camp-independent until then.
type Service struct {
	store *config.Store

	mu     sync.Mutex
	anchor *anchor
	campID string
}

// New constructs a Service. The store must outlive the service.
func New(store *config.Store) *Service {
	return &Service{store: store}
}

// Start installs the pf anchor on iface scoped to tunnelIP with the
// builtin + currently-enabled user rules. campID identifies which
// camp's section of the store this run operates on; usually obtained
// from eng.Status().CampID by the caller. Idempotent enough — calling
// twice without Stop in between fails because the prior pid is still
// alive; that's fine, callers don't do that.
func (s *Service) Start(iface, tunnelIP, campID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.anchor != nil {
		return errors.New("firewall: already started")
	}
	s.campID = campID
	a, err := openAnchor(iface, tunnelIP, mergeRules(s.userPortsLocked()))
	if err != nil {
		return err
	}
	s.anchor = a
	return nil
}

// userPortsLocked reads the user-configured allow list from the
// camp config in the store. Returns nil on read error (which the
// caller treats as "no user rules"). Caller holds s.mu, but the
// store has its own mutex so reads are safe either way.
func (s *Service) userPortsLocked() []config.Firewall {
	if s.campID == "" {
		return nil
	}
	c, err := s.store.SnapshotCamp(s.campID)
	if err != nil || c == nil {
		return nil
	}
	return c.Firewall
}

// Stop tears down the pf anchor and removes the state file.
// Idempotent — no-op when Start was never called or already stopped.
func (s *Service) Stop() error {
	s.mu.Lock()
	a := s.anchor
	s.anchor = nil
	s.mu.Unlock()
	if a == nil {
		return nil
	}
	return a.close()
}

// Active reports whether the pf anchor is currently installed.
// false means Start was never called, Start failed (e.g. pfctl
// permission denied), or the service has been Stop'd.
func (s *Service) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.anchor != nil
}

// BuiltinPorts returns the always-on f2f-internal allow list as the
// shape the UI expects.
func (s *Service) BuiltinPorts() []config.Firewall {
	out := make([]config.Firewall, len(BuiltinRules))
	for i, r := range BuiltinRules {
		out[i] = config.Firewall{
			Port:        r.Port,
			Protocol:    r.Protocol,
			Description: BuiltinLabel(r.Port, r.Protocol),
			Enabled:     true,
		}
	}
	return out
}

// UserPorts returns a copy of the user-configured allow list, sourced
// from the on-disk camp config. Returns nil before Start (campID not
// yet set) or on store error.
func (s *Service) UserPorts() []config.Firewall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]config.Firewall(nil), s.userPortsLocked()...)
}

// SetUserPorts replaces the user allow list, persists it into the
// per-camp config on disk, and re-applies the pf anchor with builtin
// + enabled user rules. Re-applying the anchor while it isn't open
// is a no-op — rules will be picked up on the next Start.
func (s *Service) SetUserPorts(list []config.Firewall) error {
	cleaned := CleanList(list)
	s.mu.Lock()
	campID := s.campID
	a := s.anchor
	s.mu.Unlock()
	if campID == "" {
		return ErrEngineNotRunning
	}
	if err := s.store.UpdateCamp(campID, func(c *config.Camp) {
		c.Firewall = append([]config.Firewall(nil), cleaned...)
	}); err != nil {
		return err
	}
	if a == nil {
		return nil
	}
	if err := a.apply(mergeRules(cleaned)); err != nil {
		return fmt.Errorf("firewall: apply: %w", err)
	}
	return nil
}

// mergeRules combines builtins with the enabled user entries.
func mergeRules(user []config.Firewall) []PortRule {
	out := make([]PortRule, 0, len(BuiltinRules)+len(user))
	out = append(out, BuiltinRules...)
	for _, p := range user {
		if !p.Enabled {
			continue
		}
		out = append(out, PortRule{Port: p.Port, Protocol: p.Protocol})
	}
	return out
}

// CleanList validates and deduplicates a user-supplied allow list.
// Drops entries with unknown protocols, out-of-range ports, and
// duplicate (port, protocol) pairs.
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
