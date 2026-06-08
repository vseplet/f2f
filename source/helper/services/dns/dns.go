// Package dns is the user-facing service for the camp's "<camp>.f2f"
// zone: it owns the local DNS server, the catalog of names we publish,
// the poll loop that mirrors peer catalogs, and the TCP health check
// that flags our own services up/down.
//
// Layering (all in this package):
//
//   - server.go: the DNS protocol server (Open/Close, Resolver
//     interface, UDP listener). Pure protocol, no app state.
//   - helper/platform: InstallZoneResolver, RemoveZoneResolver,
//     FlushDNSCache — macOS / Linux specifics for routing system
//     queries to our DNS port.
//   - this file: lifecycle (Start/Stop), Resolver implementation
//     (LookupHost over our and peers' published names), CRUD on
//     MyDomains, peer-domain poll, TCP health check.
//
// The service reads/writes camp state through *config.Store directly
// — engine is consulted only for live peer iteration and the tunnel
// HTTP port. This keeps config persistence centralised and avoids
// engine accumulating per-service proxy setters.
package dns

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/mesh/engine"
	"github.com/vseplet/f2f/source/helper/platform"
)

// Entry is one (name, port, proto) record this peer publishes inside
// the camp's <camp>.f2f zone. Port and proto are advisory: DNS only
// carries the IP, the user types the port in their URLs.
type Entry struct {
	Name string `json:"name"`
	// Host is the upstream the reverse-proxy dials. Blank means
	// 127.0.0.1 (the common case).
	Host            string `json:"host,omitempty"`
	Port            int    `json:"port,omitempty"`
	Proto           string `json:"proto,omitempty"`
	Health          string `json:"health,omitempty"`            // "ok" | "fail" | "" (unknown)
	HealthCheckedAt int64  `json:"health_checked_at,omitempty"` // unix seconds
}

// UpstreamHost returns the effective host the reverse-proxy and
// health-check should dial. Empty Host → 127.0.0.1 fallback so old
// records (created before this field existed) keep working.
func (e Entry) UpstreamHost() string {
	if e.Host == "" {
		return "127.0.0.1"
	}
	return e.Host
}

type healthSnapshot struct {
	status    string // "ok" | "fail"
	checkedAt int64  // unix seconds
}

// Service owns the local DNS server plus the catalog of names this
// peer publishes plus the in-memory mirror of peer-published names
// (populated by PollPeers, persisted via store on every change).
//
// Constructed once in main.go; Start/Stop are driven by engine
// lifecycle hooks. Implements Resolver — passed to
// Open at Start.
type Service struct {
	store *config.Store
	eng   *engine.Engine

	my atomic.Pointer[[]Entry]

	healthMu sync.Mutex
	health   map[string]healthSnapshot

	// peerDoms is the in-memory mirror of peer-published domains,
	// populated by PollPeers and consulted by LookupHost. The
	// canonical persisted copy lives in store under
	// PeerCatalog[*].Domains.
	peerDomsMu sync.RWMutex
	peerDoms   map[string][]Entry // pub → entries

	srvMu  sync.Mutex
	srv    *Server
	zone   string
	campID string
}

// New constructs a Service. The store and engine must outlive the
// service. The camp identity is picked up from engine.Status() at
// Start time — the service is "the current camp's DNS", not bound
// to a specific id, so camp switches just require Stop+Start.
func New(store *config.Store, eng *engine.Engine) *Service {
	return &Service{
		store:    store,
		eng:      eng,
		health:   make(map[string]healthSnapshot),
		peerDoms: make(map[string][]Entry),
	}
}

// Start opens the DNS server on a free loopback port, installs the
// system zone resolver, flushes the OS DNS cache, and seeds both
// the MyDomains list and the peer-domains mirror from the on-disk
// camp config. campID and zone come from the engine's current
// state (typically passed through from eng.OnStarted via Status()).
func (s *Service) Start(campID, zone string) error {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	if s.srv != nil {
		return nil
	}
	s.campID = campID
	if err := s.seedFromStore(); err != nil {
		return err
	}
	srv, err := Open("127.0.0.1:0", zone, s)
	if err != nil {
		return err
	}
	s.srv = srv
	s.zone = zone
	addr := srv.Addr()
	if rerr := platform.InstallZoneResolver(zone, addr); rerr != nil {
		log.Printf("dns: install zone resolver: %v", rerr)
	} else {
		log.Printf("dns: serving %s.f2f on %s", zone, addr)
		_ = platform.FlushDNSCache()
	}
	return nil
}

// seedFromStore loads MyDomains + per-peer Domains from camp config
// into the in-memory caches. Called on Start; safe to re-invoke after
// a camp switch (just replaces the caches).
func (s *Service) seedFromStore() error {
	camp, err := s.store.SnapshotCamp(s.campID)
	if err != nil {
		return err
	}
	if camp == nil {
		empty := []Entry{}
		s.my.Store(&empty)
		return nil
	}
	mine := make([]Entry, 0, len(camp.MyDomains))
	for _, d := range camp.MyDomains {
		mine = append(mine, Entry{Name: d.Name, Host: d.Host, Port: d.Port, Proto: d.Proto})
	}
	s.my.Store(&mine)

	pd := make(map[string][]Entry, len(camp.PeerCatalog))
	for _, p := range camp.PeerCatalog {
		if p.Pub == "" || len(p.Domains) == 0 {
			continue
		}
		list := make([]Entry, 0, len(p.Domains))
		for _, d := range p.Domains {
			list = append(list, Entry{Name: d.Name, Host: d.Host, Port: d.Port, Proto: d.Proto})
		}
		pd[p.Pub] = list
	}
	s.peerDomsMu.Lock()
	s.peerDoms = pd
	s.peerDomsMu.Unlock()
	log.Printf("dns: seeded %d my-domains + %d peers with cached domains", len(mine), len(pd))
	return nil
}

// Stop removes the zone resolver hint and closes the DNS server.
// Idempotent.
func (s *Service) Stop() error {
	s.srvMu.Lock()
	srv := s.srv
	zone := s.zone
	s.srv = nil
	s.zone = ""
	s.srvMu.Unlock()
	if zone != "" {
		if err := platform.RemoveZoneResolver(zone); err != nil {
			log.Printf("dns: remove zone resolver: %v", err)
		}
	}
	if srv == nil {
		return nil
	}
	return srv.Close()
}

// Addr returns the loopback "host:port" the DNS server is listening
// on, or "" when not running. Surfaced in /api/status for diagnostics.
func (s *Service) Addr() string {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	if s.srv == nil {
		return ""
	}
	return s.srv.Addr()
}

// Stats returns a snapshot of the DNS server's request counters.
// Zero value when not running.
func (s *Service) Stats() Stats {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	if s.srv == nil {
		return Stats{}
	}
	return s.srv.Stats()
}

// Active reports whether the DNS server is currently bound.
func (s *Service) Active() bool {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	return s.srv != nil
}

// MyDomains returns a copy of the local-published catalog with the
// latest health stamped onto each entry. Never nil.
func (s *Service) MyDomains() []Entry {
	p := s.my.Load()
	if p == nil {
		return []Entry{}
	}
	out := make([]Entry, len(*p))
	copy(out, *p)
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	for i := range out {
		if h, ok := s.health[out[i].Name]; ok {
			out[i].Health = h.status
			out[i].HealthCheckedAt = h.checkedAt
		}
	}
	return out
}

// portalName is the built-in, local-only domain that points at this
// node's own web UI (portal.<camp>.f2f → the loopback UI). It is never
// in MyDomains, so it is neither shared with peers (/api/domains) nor
// routable on the tunnel-facing proxy — only the loopback proxy and the
// local resolver honour it via LocalRoutes.
const (
	portalName = "portal"
	portalPort = 2202 // default web-UI bind; see main.go defaultBind
)

// LocalRoutes is MyDomains plus the built-in portal entry — the set the
// LOCAL reverse-proxy (loopback listener) and the resolver should
// honour. Kept separate from MyDomains so portal never leaks to peers.
func (s *Service) LocalRoutes() []Entry {
	return append(s.MyDomains(), Entry{Name: portalName, Host: "127.0.0.1", Port: portalPort})
}

// PeerDomains returns the in-memory mirror of one peer's published
// catalog (empty slice when nothing's been polled yet).
func (s *Service) PeerDomains(pub string) []Entry {
	s.peerDomsMu.RLock()
	defer s.peerDomsMu.RUnlock()
	list, ok := s.peerDoms[pub]
	if !ok {
		return nil
	}
	out := make([]Entry, len(list))
	copy(out, list)
	return out
}

// SetMyDomains replaces the local-published list atomically and
// persists it into camp config. Health state for removed names is
// dropped so stale "ok" indicators don't linger in the UI. Other
// peers pick up the change on their next /api/domains poll (~10s).
func (s *Service) SetMyDomains(list []Entry) error {
	dup := make([]Entry, len(list))
	copy(dup, list)
	s.my.Store(&dup)
	keep := make(map[string]struct{}, len(dup))
	for _, d := range dup {
		keep[d.Name] = struct{}{}
	}
	s.healthMu.Lock()
	for name := range s.health {
		if _, ok := keep[name]; !ok {
			delete(s.health, name)
		}
	}
	s.healthMu.Unlock()
	cfg := toCampDomains(dup)
	return s.store.UpdateCamp(s.campID, func(c *config.Camp) {
		c.MyDomains = cfg
	})
}

// RemovePeerDomain drops one (peer, domain) entry from the in-memory
// mirror and from the persisted peer catalog. Walks every catalog
// row matching peerName since legacy data may have multiple rows for
// the same peer name (one stale row with empty Pub plus the current
// row with a proper Pub). If the peer is online and still publishing
// the name, the next successful poll re-adds it.
func (s *Service) RemovePeerDomain(peerName, domainName string) error {
	if peerName == "" || domainName == "" {
		return errors.New("peer and name required")
	}
	droppedPubs := map[string]struct{}{}
	if err := s.store.UpdateCamp(s.campID, func(c *config.Camp) {
		for i := range c.PeerCatalog {
			if c.PeerCatalog[i].Name != peerName {
				continue
			}
			kept := c.PeerCatalog[i].Domains[:0]
			changed := false
			for _, d := range c.PeerCatalog[i].Domains {
				if d.Name == domainName {
					changed = true
					continue
				}
				kept = append(kept, d)
			}
			if changed {
				c.PeerCatalog[i].Domains = kept
				if c.PeerCatalog[i].Pub != "" {
					droppedPubs[c.PeerCatalog[i].Pub] = struct{}{}
				}
			}
		}
	}); err != nil {
		return err
	}
	if len(droppedPubs) == 0 {
		return nil
	}
	s.peerDomsMu.Lock()
	for pub := range droppedPubs {
		list := s.peerDoms[pub]
		kept := list[:0]
		for _, d := range list {
			if d.Name == domainName {
				continue
			}
			kept = append(kept, d)
		}
		s.peerDoms[pub] = kept
	}
	s.peerDomsMu.Unlock()
	return nil
}

// LookupHost implements Resolver. Resolves a label under
// our camp's f2f zone to a v4 address:
//
//   - Our own published names → V4=127.0.0.1 (loopback; we host the
//     actual service on localhost).
//   - Peer-published names → V4=peer's overlay v4 (100.64.X.Y derived
//     from pub, routes through utun).
//   - Anything else → ok=false.
//
// Names are skipped for offline peers — handing out an address for a
// peer we can't currently reach would just make apps stall. The set
// of "online" peers comes from the engine via OnlinePeersWithDomains.
func (s *Service) LookupHost(label string) (Host, bool) {
	// LocalRoutes (not MyDomains) so the built-in portal resolves too;
	// the resolver only answers local queries (127.0.0.1:5354).
	mine := s.LocalRoutes()
	for _, d := range mine {
		if !IsWildcardLabel(d.Name) && strings.EqualFold(d.Name, label) {
			return Host{V4: "127.0.0.1"}, true
		}
	}
	for _, d := range mine {
		if IsWildcardLabel(d.Name) && MatchesWildcard(d.Name, label) {
			return Host{V4: "127.0.0.1"}, true
		}
	}
	online := s.eng.OnlinePeersForCAPoll() // re-used: same shape (Name/Host) + Pub via overlay
	// Pass 1: exact match.
	if host, ok := s.matchPeers(online, label, false); ok {
		return host, ok
	}
	// Pass 2: wildcard match.
	if host, ok := s.matchPeers(online, label, true); ok {
		return host, ok
	}
	return Host{}, false
}

// matchPeers walks the online peer list, looks up each peer's
// in-memory domain mirror by Pub, and returns the first matching v4.
// wildcard selects which entries to consider (non-wildcards pass 1,
// wildcards pass 2 — exact match wins over wildcard).
func (s *Service) matchPeers(online []engine.OnlinePeerHTTPInfo, label string, wildcard bool) (Host, bool) {
	for _, p := range online {
		if p.Pub == "" {
			continue
		}
		list := s.PeerDomains(p.Pub)
		for _, d := range list {
			if IsWildcardLabel(d.Name) != wildcard {
				continue
			}
			if !wildcard && !strings.EqualFold(d.Name, label) {
				continue
			}
			if wildcard && !MatchesWildcard(d.Name, label) {
				continue
			}
			return Host{V4: p.Host}, true
		}
	}
	return Host{}, false
}

// HealthCheck blocks until ctx is done, TCP-dialing each published
// name on 127.0.0.1:<port> every 8s and stamping the result onto the
// in-memory health map. The status flows out through MyDomains() —
// into the UI for our own row and into /api/domains so OTHER peers
// see whether our services are actually up.
func (s *Service) HealthCheck(ctx context.Context) {
	const interval = 8 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.checkOnce(ctx)
	}
}

func (s *Service) checkOnce(ctx context.Context) {
	domains := s.MyDomains()
	now := time.Now().Unix()
	for _, d := range domains {
		if d.Port == 0 {
			continue
		}
		if IsWildcardLabel(d.Name) {
			continue
		}
		status := "fail"
		addr := net.JoinHostPort(d.UpstreamHost(), strconv.Itoa(d.Port))
		dialer := net.Dialer{Timeout: 2 * time.Second}
		if conn, err := dialer.DialContext(ctx, "tcp", addr); err == nil {
			_ = conn.Close()
			status = "ok"
		}
		s.healthMu.Lock()
		s.health[d.Name] = healthSnapshot{status: status, checkedAt: now}
		s.healthMu.Unlock()
	}
}

// PollPeers blocks until ctx is done, walking every online peer once
// per 10s tick and pulling their /api/domains list over the tunnel.
// Each result is mirrored into the in-memory map and the persisted
// peer catalog so the catalog survives engine restart.
func (s *Service) PollPeers(ctx context.Context) {
	const interval = 10 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.pollOnce(ctx)
	}
}

func (s *Service) pollOnce(ctx context.Context) {
	port := s.eng.TunnelHTTPPort()
	if port == "" {
		// log.Printf("dns-poll: tunnel HTTP port not set — skipping")
		return
	}
	targets := s.eng.OnlinePeersForCAPoll()
	if len(targets) == 0 {
		// log.Printf("dns-poll: 0 peers online — skipping tick")
		return
	}
	// log.Printf("dns-poll: polling %d peer(s) on port %s", len(targets), port)
	client := &http.Client{Timeout: 3 * time.Second}
	for _, t := range targets {
		url := "http://" + net.JoinHostPort(t.Host, port) + "/api/domains"
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		var list []Entry
		err = json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if err != nil {
			continue
		}
		s.upsertPeer(t.Pub, t.Name, list)
		// log.Printf("dns: peer %s published %d domain(s)", t.Name, len(list))
	}
}

// upsertPeer mirrors a peer's just-polled domain list into both the
// in-memory peerDoms map and the persisted PeerCatalog entry. Match
// is by Pub — Name is a display-only hint that may collide across
// historical catalog entries (e.g. one legacy row with empty Pub
// plus a new row with proper Pub for the same peer name). Pub is
// the only stable key.
func (s *Service) upsertPeer(pub, peerName string, list []Entry) {
	if pub == "" {
		return
	}
	cfgDomains := toCampDomains(list)
	_ = s.store.UpdateCamp(s.campID, func(c *config.Camp) {
		for i := range c.PeerCatalog {
			if c.PeerCatalog[i].Pub == pub {
				c.PeerCatalog[i].Domains = cfgDomains
				return
			}
		}
	})
	s.peerDomsMu.Lock()
	s.peerDoms[pub] = append([]Entry(nil), list...)
	s.peerDomsMu.Unlock()
}

func toCampDomains(list []Entry) []config.Domain {
	out := make([]config.Domain, 0, len(list))
	for _, d := range list {
		out = append(out, config.Domain{
			Name:  d.Name,
			Host:  d.Host,
			Port:  d.Port,
			Proto: d.Proto,
		})
	}
	return out
}

// IsWildcardLabel returns true for entries of the form "*.X" — a
// catch-all claim over the subdomain X.
func IsWildcardLabel(name string) bool {
	return strings.HasPrefix(name, "*.") && len(name) > 2
}

// MatchesWildcard reports whether label is covered by pattern of the
// form "*.X" — i.e. label ends with ".X". Pattern must already be a
// known wildcard.
func MatchesWildcard(pattern, label string) bool {
	suffix := pattern[1:] // drop the "*", keep ".X"
	return len(label) > len(suffix) && strings.HasSuffix(strings.ToLower(label), strings.ToLower(suffix))
}

// SortByName returns a stable, name-sorted copy of in.
func SortByName(in []Entry) []Entry {
	out := append([]Entry(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
