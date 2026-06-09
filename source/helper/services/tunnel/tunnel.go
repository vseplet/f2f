// Package tunnel is the application-level service that decides what
// public-network traffic gets routed through the f2f overlay and
// which peer it goes to. Today this means "intercepts" — user-driven
// (spec → peer) bindings where spec is a CIDR, IP, or DNS name.
// Future work: tunnelling traffic to public domains the user has
// pointed at the camp (ngrok-style ingress); and intercepts for
// domains visible only from the EXIT peer's network (e.g. a corporate
// VPN) — resolve the name AND its outbound interface on the exit peer,
// not the origin. See ARCHITECTURE.md "TODO: intercepts на домены,
// видимые только из сети exit-пира".
//
// Layering:
//
//   - engine: transport substrate (utun, UDP, AWG, pair, punch). Owns
//     the route.Manager primitive but no application-level policy.
//   - this package: intercept lifecycle, on-disk persistence via
//     config.Store, periodic DNS re-resolution for domain-spec
//     intercepts, and the AWG-allowed-ips contribution callback so
//     outbound packets to intercept destinations route through the
//     selected peer over the encrypted tunnel.
package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/mesh/bus"
	"github.com/vseplet/f2f/source/helper/mesh/engine"
	"github.com/vseplet/f2f/source/helper/platform"
)

// busTypeResolve is the bus message type for exit-peer name
// resolution: the payload is a bare DNS name, the response a JSON
// array of IP strings resolved from THIS node's network view (inside
// its corporate VPN, etc.). As a side effect the handler prepares
// per-target egress NAT for the resolved IPs, so a follow-up
// intercept route through this node actually reaches them.
const busTypeResolve = "resolve"

// InterceptInfo describes one intercept entry — the spec the user
// typed, the host routes it owns on the local route table, and the
// peer-name traffic should egress through.
type InterceptInfo struct {
	ID       string   `json:"id"`
	Spec     string   `json:"spec"`
	Peer     string   `json:"peer"`
	Prefixes []string `json:"prefixes"`
}

// Service owns the live intercept set. State is duplicated:
//   - in-memory map keyed by ID for fast lookup / AWG sync
//   - on disk via config.Store.UpdateCamp(c.Intercepts) so the set
//     survives engine restart
type Service struct {
	store *config.Store
	eng   *engine.Engine
	bus   *bus.Service

	// OnDomainPinned / OnDomainUnpinned let main wire the local DNS
	// service: when a domain-spec intercept resolves (on the exit
	// peer), the client's own DNS must answer that name with the SAME
	// IPs the routes point at — otherwise apps resolve publicly (or
	// not at all, for VPN-only names) and miss the tunnel. Set once
	// before Start.
	OnDomainPinned   func(domain string, v4s []string)
	OnDomainUnpinned func(domain string)

	mu         sync.Mutex
	campID     string
	intercepts map[string]*InterceptInfo
	nextID     uint64
	egress     *egress
}

// New constructs a Service. store + engine + bus must outlive it.
func New(store *config.Store, eng *engine.Engine, b *bus.Service) *Service {
	return &Service{
		store:      store,
		eng:        eng,
		bus:        b,
		intercepts: map[string]*InterceptInfo{},
	}
}

// Register installs the "resolve" bus handler: peers ask us to
// resolve a name from our network's point of view (we may sit inside
// a VPN they can't see). We also prepare per-target egress NAT for
// the answer, so the asking peer's follow-up traffic routed through
// us reaches the target even when it egresses via another tunnel
// (split-tunnel corporate VPN). Call once after construction.
func (s *Service) Register() {
	if s.bus == nil {
		return
	}
	s.bus.Handle(busTypeResolve, func(fromPub string, payload []byte) ([]byte, error) {
		name := strings.ToLower(strings.TrimSpace(string(payload)))
		if name == "" || len(name) > 253 || !isDomainSpec(name) {
			return nil, fmt.Errorf("bad resolve name %q", name)
		}
		ips, err := net.LookupIP(name)
		if err != nil {
			return nil, err
		}
		var addrs []netip.Addr
		var out []string
		for _, ip := range ips {
			a, ok := netip.AddrFromSlice(ip)
			if !ok {
				continue
			}
			a = a.Unmap()
			addrs = append(addrs, a)
			out = append(out, a.String())
		}
		s.mu.Lock()
		eg := s.egress
		s.mu.Unlock()
		if eg != nil {
			eg.ensureTargets(addrs, s.eng.UtunName())
		}
		log.Printf("resolve: %s → %s (asked over bus)", name, strings.Join(out, ", "))
		return json.Marshal(out)
	})
}

// Start picks up the active camp_id, restores every (spec, peer)
// pair persisted in the camp config into kernel routes + the
// in-memory map, and registers the AWG allowed-ips hook with the
// engine. Idempotent — re-calling after a camp switch reloads.
func (s *Service) Start(campID string) {
	s.mu.Lock()
	s.campID = campID
	s.intercepts = map[string]*InterceptInfo{}
	s.nextID = 0
	s.mu.Unlock()
	s.eng.SetAWGAllowedCIDRsHook(s.allowedCIDRsForPeer)
	s.startEgress()
	s.restoreFromStore()
}

// Stop clears the in-memory intercept set and tears down NAT egress.
// Routes themselves are removed by engine.Stop when it tears down
// its route.Manager.
func (s *Service) Stop() {
	s.mu.Lock()
	old := s.intercepts
	s.intercepts = map[string]*InterceptInfo{}
	s.campID = ""
	eg := s.egress
	s.egress = nil
	s.mu.Unlock()
	if s.OnDomainUnpinned != nil {
		for _, info := range old {
			if isDomainSpec(info.Spec) {
				s.OnDomainUnpinned(strings.ToLower(info.Spec))
			}
		}
	}
	if eg != nil {
		if err := eg.close(); err != nil {
			log.Printf("egress close: %v", err)
		}
	}
	s.eng.SetAWGAllowedCIDRsHook(nil)
}

// startEgress installs the system-wide NAT rule that lets traffic
// from the camp's overlay /10 subnet leave this node through the
// host's default-route interface. Auto-detects the egress interface
// via platform.DefaultEgressInterface; non-fatal if anything fails
// (peers just won't reach the internet through this node).
func (s *Service) startEgress() {
	iface, err := platform.DefaultEgressInterface()
	if err != nil {
		log.Printf("egress: %v; skipping NAT (peers won't reach internet through this node)", err)
		return
	}
	subnet := netip.MustParsePrefix(engine.V4Subnet)
	eg, err := openEgress(iface, subnet)
	if err != nil {
		log.Printf("egress: %v (peers won't reach internet through this node)", err)
		return
	}
	s.mu.Lock()
	s.egress = eg
	s.mu.Unlock()
	log.Printf("egress: NAT %s → %s, ip-forwarding=1", subnet, iface)
}

// EgressActive reports whether NAT is currently installed for the
// overlay subnet. Surfaces in /api/status diagnostics.
func (s *Service) EgressActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.egress != nil
}

// List returns a copy of the current intercept set.
func (s *Service) List() []InterceptInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]InterceptInfo, 0, len(s.intercepts))
	for _, info := range s.intercepts {
		out = append(out, *info)
	}
	return out
}

// Add resolves spec to one or more host routes, installs them via
// engine.Routes() pointing at the utun, binds them to the named
// peer, and persists the (spec, peer) pair in the camp config.
func (s *Service) Add(spec, peer string) (InterceptInfo, error) {
	if peer == "" {
		return InterceptInfo{}, errors.New("intercept peer is required")
	}
	if !s.eng.HasPeerName(peer) {
		return InterceptInfo{}, fmt.Errorf("peer %q is not in the camp", peer)
	}
	info, err := s.addLocked(spec, peer)
	if err != nil {
		return info, err
	}
	s.mu.Lock()
	campID := s.campID
	s.mu.Unlock()
	if campID != "" {
		_ = s.store.UpdateCamp(campID, func(c *config.Camp) {
			for _, it := range c.Intercepts {
				if it.Spec == info.Spec && it.Peer == info.Peer {
					return
				}
			}
			c.Intercepts = append(c.Intercepts, config.Intercept{
				Spec: info.Spec, Peer: info.Peer,
			})
		})
	}
	s.eng.SyncAWG()
	return info, nil
}

// Remove deletes routes for the given intercept ID, drops the
// in-memory record, and removes the matching (spec, peer) pair from
// the camp config.
func (s *Service) Remove(id string) error {
	s.mu.Lock()
	info, ok := s.intercepts[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("intercept %q not found", id)
	}
	delete(s.intercepts, id)
	campID := s.campID
	s.mu.Unlock()
	routes := s.eng.Routes()
	for _, prefStr := range info.Prefixes {
		prefStr = strings.TrimSuffix(prefStr, " (reject)")
		p, err := netip.ParsePrefix(prefStr)
		if err != nil {
			continue
		}
		if err := routes.Remove(p); err != nil {
			log.Printf("WARN: remove route %s: %v", prefStr, err)
		}
	}
	log.Printf("removed intercept %s (%s)", id, info.Spec)
	if isDomainSpec(info.Spec) && s.OnDomainUnpinned != nil {
		s.OnDomainUnpinned(strings.ToLower(info.Spec))
	}
	if campID != "" {
		_ = s.store.UpdateCamp(campID, func(c *config.Camp) {
			kept := c.Intercepts[:0]
			for _, it := range c.Intercepts {
				if it.Spec == info.Spec && it.Peer == info.Peer {
					continue
				}
				kept = append(kept, it)
			}
			c.Intercepts = kept
		})
	}
	s.eng.SyncAWG()
	return nil
}

// addLocked allocates an ID, resolves spec → prefixes, and installs
// routes. Mutates the in-memory map under its own lock; routes go
// through engine.Routes() which has its own synchronisation.
func (s *Service) addLocked(spec, peer string) (InterceptInfo, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return InterceptInfo{}, errors.New("empty intercept spec")
	}
	prefixes, err := s.resolvePrefixes(spec, peer)
	if err != nil {
		return InterceptInfo{}, err
	}
	if len(prefixes) == 0 {
		return InterceptInfo{}, fmt.Errorf("%q: no addresses", spec)
	}
	s.mu.Lock()
	s.nextID++
	id := "i" + strconv.FormatUint(s.nextID, 10)
	s.mu.Unlock()

	info := &InterceptInfo{ID: id, Spec: spec, Peer: peer}
	routes := s.eng.Routes()
	utunName := s.eng.UtunName()
	for _, p := range prefixes {
		if p.Addr().Is6() {
			if err := routes.AddReject(p); err != nil {
				log.Printf("WARN: route -reject %s: %v", p, err)
				continue
			}
			info.Prefixes = append(info.Prefixes, p.String()+" (reject)")
			log.Printf("route %s → reject (IPv6 fallback to IPv4)", p)
			continue
		}
		if err := routes.Add(p); err != nil {
			log.Printf("WARN: route %s: %v", p, err)
			continue
		}
		info.Prefixes = append(info.Prefixes, p.String())
		log.Printf("route %s → %s", p, utunName)
	}
	if len(info.Prefixes) == 0 {
		return InterceptInfo{}, fmt.Errorf("%q: all route adds failed", spec)
	}
	s.mu.Lock()
	s.intercepts[id] = info
	s.mu.Unlock()
	s.pinDomain(spec, info.Prefixes)
	return *info, nil
}

// addPendingLocked registers a domain intercept whose resolve failed
// (exit peer offline at restore time) with no routes — so the
// RefreshDomainRoutes ticker keeps retrying it instead of the entry
// silently disappearing until the next restart.
func (s *Service) addPendingLocked(spec, peer string) {
	s.mu.Lock()
	s.nextID++
	id := "i" + strconv.FormatUint(s.nextID, 10)
	s.intercepts[id] = &InterceptInfo{ID: id, Spec: spec, Peer: peer}
	s.mu.Unlock()
}

// resolvePrefixes turns a spec into prefixes. CIDR/IP specs parse
// locally; domain specs are resolved ON THE EXIT PEER over the bus —
// it may sit inside a network (corporate VPN) where the name resolves
// differently or exclusively. Falls back to local resolution when the
// peer can't be reached, which keeps public-domain intercepts working.
func (s *Service) resolvePrefixes(spec, peer string) ([]netip.Prefix, error) {
	if !isDomainSpec(spec) {
		return resolveSpec(spec)
	}
	if s.bus != nil {
		if pub := s.pubForPeerName(peer); pub != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			raw, err := s.bus.Request(ctx, pub, busTypeResolve, []byte(spec))
			cancel()
			if err != nil {
				log.Printf("resolve %q on %s: %v (falling back to local DNS)", spec, peer, err)
			} else {
				var addrs []string
				if jerr := json.Unmarshal(raw, &addrs); jerr == nil {
					var out []netip.Prefix
					for _, str := range addrs {
						a, aerr := netip.ParseAddr(str)
						if aerr != nil {
							continue
						}
						a = a.Unmap()
						out = append(out, netip.PrefixFrom(a, a.BitLen()))
						log.Printf("resolved %s → %s (on %s)", spec, a, peer)
					}
					if len(out) > 0 {
						return out, nil
					}
				}
			}
		}
	}
	return resolveSpec(spec)
}

// pubForPeerName maps a camp peer's display name to its pub via the
// engine roster ("" if unknown).
func (s *Service) pubForPeerName(name string) string {
	if name == "" {
		return ""
	}
	for _, p := range s.eng.Status().Peers {
		if !p.Self && p.Name == name {
			return p.Pub
		}
	}
	return ""
}

// pinDomain mirrors a domain intercept's resolved v4 addresses into
// the local DNS (via OnDomainPinned), so apps on this node resolve
// the name to exactly the IPs the routes cover. No-op for non-domain
// specs or when nothing v4 resolved.
func (s *Service) pinDomain(spec string, prefixes []string) {
	if s.OnDomainPinned == nil || !isDomainSpec(spec) {
		return
	}
	var v4s []string
	for _, pref := range prefixes {
		if strings.HasSuffix(pref, " (reject)") {
			continue
		}
		if p, err := netip.ParsePrefix(pref); err == nil && p.Addr().Is4() {
			v4s = append(v4s, p.Addr().String())
		}
	}
	if len(v4s) > 0 {
		s.OnDomainPinned(strings.ToLower(spec), v4s)
	}
}

// allowedCIDRsForPeer is the hook engine.awgSyncPeers calls to
// gather extra allowed_ips for one peer (added on top of the peer's
// overlay /32). Returns CIDRs as strings, IPv6 entries stripped of
// the local " (reject)" annotation.
func (s *Service) allowedCIDRsForPeer(peerName string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, info := range s.intercepts {
		if info.Peer != peerName {
			continue
		}
		for _, pref := range info.Prefixes {
			pref = strings.TrimSuffix(pref, " (reject)")
			if pref == "" {
				continue
			}
			out = append(out, pref)
		}
	}
	return out
}

// restoreFromStore re-installs every (spec, peer) pair persisted in
// the camp config. Entries whose peer is no longer in the camp
// catalog are logged and skipped; they'll be retried on the next
// reconcile (currently driven by the UI).
func (s *Service) restoreFromStore() {
	camp, err := s.store.SnapshotCamp(s.campID)
	if err != nil || camp == nil {
		return
	}
	for _, it := range camp.Intercepts {
		if it.Spec == "" || it.Peer == "" {
			continue
		}
		if !s.eng.HasPeerName(it.Peer) {
			log.Printf("config: intercept %q via %s skipped (peer not in catalog)", it.Spec, it.Peer)
			continue
		}
		if _, err := s.addLocked(it.Spec, it.Peer); err != nil {
			if isDomainSpec(it.Spec) {
				// Exit peer probably not reachable yet (bus comes up
				// after hole punch). Keep the entry route-less; the
				// RefreshDomainRoutes ticker retries every minute.
				s.addPendingLocked(it.Spec, it.Peer)
				log.Printf("config: intercept %q via %s pending (%v); will retry", it.Spec, it.Peer, err)
				continue
			}
			log.Printf("config: restore intercept %q via %s: %v", it.Spec, it.Peer, err)
		}
	}
	s.eng.SyncAWG()
}

// RefreshDomainRoutes blocks until ctx is done, re-resolving every
// domain-spec intercept once per minute and rewriting its routes if
// the resolved address set changed.
func (s *Service) RefreshDomainRoutes(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.refreshOnce()
	}
}

func (s *Service) refreshOnce() {
	type entry struct {
		id   string
		spec string
		peer string
		old  []string
	}
	var domains []entry
	s.mu.Lock()
	for id, info := range s.intercepts {
		if isDomainSpec(info.Spec) {
			domains = append(domains, entry{
				id:   id,
				spec: info.Spec,
				peer: info.Peer,
				old:  append([]string(nil), info.Prefixes...),
			})
		}
	}
	s.mu.Unlock()

	routes := s.eng.Routes()
	for _, d := range domains {
		newPrefixes, err := s.resolvePrefixes(d.spec, d.peer)
		if err != nil {
			log.Printf("WARN: refresh %s: %v", d.spec, err)
			continue
		}
		newSet := make(map[string]netip.Prefix, len(newPrefixes))
		for _, p := range newPrefixes {
			newSet[p.String()] = p
		}
		oldSet := make(map[string]struct{}, len(d.old))
		for _, str := range d.old {
			oldSet[strings.TrimSuffix(str, " (reject)")] = struct{}{}
		}
		changed := len(newSet) != len(oldSet)
		if !changed {
			for str := range oldSet {
				if _, ok := newSet[str]; !ok {
					changed = true
					break
				}
			}
		}
		if !changed {
			continue
		}

		s.mu.Lock()
		info, ok := s.intercepts[d.id]
		if !ok {
			s.mu.Unlock()
			continue
		}
		for _, prefStr := range info.Prefixes {
			prefStr = strings.TrimSuffix(prefStr, " (reject)")
			if p, err := netip.ParsePrefix(prefStr); err == nil {
				if err := routes.Remove(p); err != nil {
					log.Printf("WARN: refresh remove route %s: %v", p, err)
				}
			}
		}
		info.Prefixes = nil
		for _, p := range newPrefixes {
			if p.Addr().Is6() {
				if err := routes.AddReject(p); err != nil {
					log.Printf("WARN: refresh route -reject %s: %v", p, err)
					continue
				}
				info.Prefixes = append(info.Prefixes, p.String()+" (reject)")
				continue
			}
			if err := routes.Add(p); err != nil {
				log.Printf("WARN: refresh route %s: %v", p, err)
				continue
			}
			info.Prefixes = append(info.Prefixes, p.String())
		}
		newPinned := append([]string(nil), info.Prefixes...)
		log.Printf("refreshed routes for %s → %s", d.spec, strings.Join(info.Prefixes, ", "))
		s.mu.Unlock()
		s.pinDomain(d.spec, newPinned)
	}
	s.eng.SyncAWG()
}

// resolveSpec converts a user-supplied spec into one or more
// netip.Prefix values. Accepts:
//   - CIDR ("10.0.0.0/24")
//   - bare IP ("192.0.2.1") — coerced to /32 or /128
//   - DNS name ("api.openai.com") — resolved via the OS resolver
func resolveSpec(spec string) ([]netip.Prefix, error) {
	if p, err := netip.ParsePrefix(spec); err == nil {
		return []netip.Prefix{p}, nil
	}
	if a, err := netip.ParseAddr(spec); err == nil {
		return []netip.Prefix{netip.PrefixFrom(a, a.BitLen())}, nil
	}
	ips, err := net.LookupIP(spec)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", spec, err)
	}
	out := make([]netip.Prefix, 0, len(ips))
	for _, ip := range ips {
		a, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
		log.Printf("resolved %s → %s", spec, a)
	}
	return out, nil
}

// isDomainSpec reports whether spec is a DNS name (not CIDR / IP).
// Domain specs need periodic re-resolution to track DNS changes.
func isDomainSpec(spec string) bool {
	if _, err := netip.ParsePrefix(spec); err == nil {
		return false
	}
	if _, err := netip.ParseAddr(spec); err == nil {
		return false
	}
	return true
}
