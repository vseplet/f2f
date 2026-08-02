// Package camp is the user-facing service that talks to the
// rendezvous server (camp): announces our presence on UDP and pushes
// the peer roster (carried on each announce reply) into the engine so
// pair-handshake / hole-punch have someone to dial.
//
// Layering:
//
//   - engine/rendezvous: wire protocol (UDP announce + reply roster).
//   - this package: lifecycle, AnnounceClient ownership, UDP-dispatch
//     hook registered with the engine, public getters for the UI
//     (Self, Snapshot, stats).
//
// Engine surface used:
//
//   - eng.UDPConn() / eng.IdentityPub() / eng.CampConfigSnapshot() for setup
//   - eng.RegisterUDPHandler(fn) to receive camp-source packets
//   - eng.ApplyCampRoster(peers) to push the roster into peers map
package camp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vseplet/f2f/source/helper/clog"
	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/mesh/camp/rendezvous"
	"github.com/vseplet/f2f/source/helper/mesh/engine"
)

// Health reports UDP-announce liveness against the camp server,
// surfaced via /api/status for the UI's camp-health card.
type Health struct {
	UDPLastSentMs  int64 `json:"udp_last_sent_ms,omitempty"`
	UDPLastReplyMs int64 `json:"udp_last_reply_ms,omitempty"`
	UDPRTTMs       int64 `json:"udp_rtt_ms,omitempty"`
}

// HealthSnapshot returns the current camp health from announce
// counters. Zero value when nothing is running yet.
func (s *Service) HealthSnapshot() *Health {
	sent, reply, rtt := s.AnnounceStats()
	return &Health{UDPLastSentMs: sent, UDPLastReplyMs: reply, UDPRTTMs: rtt}
}

// Service owns the AnnounceClient. Constructed once in main.go; Start
// runs on eng.OnStarted, Stop on eng.OnStopped.
type Service struct {
	eng     *engine.Engine
	store   *config.Store
	version string // client build version, announced to the camp
	role    string // run-mode role (user/service/task), published in the roster

	// httpOK flips true once the HTTP roster poll (/api/id) succeeds; while set,
	// the UDP announce reply's roster is ignored (HTTP is the authoritative
	// source with full PeerInfo). Stays false against an old server without
	// /api/id, so the UDP roster remains the fallback — backward compatible.
	httpOK atomic.Bool

	mu            sync.Mutex
	announce      *rendezvous.AnnounceClient
	cancel        context.CancelFunc
	announceDone  chan struct{}
	unregisterUDP func()

	// cycleAcc accumulates peers across a paged roster's windows (keyed by
	// pub). Reconciled into the engine only when a window marks cycleEnd —
	// see onUpdate. Touched solely on the announce/read-loop goroutine.
	cycleAcc map[string]rendezvous.PeerInfo

	// rosterSeen / rosterMiss give grace to peers that miss a cycle: a single
	// lost UDP window-reply leaves cycleAcc incomplete, and dropping those peers
	// immediately churns the whole roster (peer leaves → bus tears its conn →
	// reappears next cycle). Instead carry a peer forward for rosterGraceCycles
	// missed cycles before truly dropping it. Touched only on the announce
	// goroutine, like cycleAcc.
	rosterSeen map[string]rendezvous.PeerInfo // last-known info per pub
	rosterMiss map[string]int                 // consecutive cycles a known pub was absent

	// snapshot holds the latest roster (defensive copy). Read lock-free
	// via atomic for the /api/status hot path.
	snapshot atomic.Pointer[[]rendezvous.PeerInfo]
	// self holds the latest announce reply (our PeerInfo as camp sees
	// us). Read by UI for the self peer-row endpoint display.
	self atomic.Pointer[rendezvous.PeerInfo]
	// curCampID is the camp_id of the running camp, set in Start and
	// cleared in Stop. Read lock-free by onUpdate (which runs on the
	// announce goroutine) so it never contends with s.mu held by Stop.
	curCampID atomic.Pointer[string]
}

// New constructs a Service. The engine must outlive the service. The
// store is where this service persists each camp's peer catalog (the
// roster snapshot it receives on every announce reply) — the engine no
// longer owns that write.
func New(eng *engine.Engine, store *config.Store, version, role string) *Service {
	return &Service{eng: eng, store: store, version: version, role: role}
}

// Role reports our own run-mode role (user/service/task) — the same value we
// publish to the roster. The web layer stamps it onto the self peer row so the
// network view shows our role too.
func (s *Service) Role() string { return s.role }

// Version reports our own announced client build string.
func (s *Service) Version() string { return s.version }

// SetName changes our announced display name live — the next announce carries
// it, so peers pick up the rename without a restart.
func (s *Service) SetName(name string) {
	s.mu.Lock()
	ac := s.announce
	s.mu.Unlock()
	if ac != nil {
		ac.SetName(name)
	}
}

// Start brings up the announce client for the given camp config.
// Order matters: register the UDP dispatch handler
// BEFORE the announce client touches the socket — engine's
// peerToTunLoop is already reading from it, and synchronous reads
// (the old AnnounceOnce path) would steal the announce reply and
// wedge peerToTunLoop with i/o timeout. Idempotent — Stop required
// between consecutive Starts for different camps.
func (s *Service) Start(c *config.Camp) error {
	if c == nil {
		return errors.New("camp: nil config")
	}
	s.mu.Lock()
	if s.announce != nil {
		s.mu.Unlock()
		return nil
	}
	udp := s.eng.UDPConn()
	if udp == nil {
		s.mu.Unlock()
		return errors.New("camp: engine UDP socket not ready")
	}
	pub := s.eng.IdentityPub()
	ac, err := rendezvous.NewAnnounceClient(udp, c.StunAddr, c.Identity.Name, c.CampID, pub, s.version, s.role)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	// Roster now arrives on the announce reply — same sink the HTTP
	// poller used.
	ac.OnPeers(s.onUpdate)
	s.mu.Unlock()

	// Register the UDP dispatch handler first so any reply from camp
	// reaches HandlePacket via the engine read loop without anyone
	// else having to touch the socket.
	campName := c.Identity.Name
	campID := c.CampID
	s.curCampID.Store(&campID) // onUpdate persists the catalog under this id
	unreg := s.eng.RegisterUDPHandler(func(pkt []byte, from *net.UDPAddr) bool {
		// Read the camp address dynamically — it can change as the address
		// re-resolves (DNS recovery / fly.io IP rotation).
		if !sameUDPAddr(ac.CampAddr(), from) {
			return false
		}
		claimed := ac.HandlePacket(pkt)
		if claimed {
			if self := ac.Self(); self != nil {
				prev := s.self.Swap(self)
				if prev == nil {
					reflex := self.UDPEndpoint
					if reflex == "" {
						reflex = self.PublicIP
					}
					clog.Info("camp", "registered as %s in camp %s, reflex=%s", campName, campID, reflex)
				}
			}
		}
		return claimed
	})

	ctx, cancel := context.WithCancel(context.Background())
	announceDone := make(chan struct{})
	go func() {
		defer close(announceDone)
		ac.Run(ctx, 4*time.Second) // sends immediately, then every 4s; with paged
		//                            windows (rosterWindow) a full roster cycle
		//                            completes in ceil(N/window) ticks.
	}()
	// HTTP roster poller: fetches the full roster from the camp server's
	// /api/id (no MTU cap → exhaustive PeerInfo). Once it succeeds it becomes the
	// authoritative source (onUpdate then ignores the UDP roster). Falls back to
	// UDP silently against an old server without /api/id.
	go s.httpRosterLoop(ctx, c.ServerURL, campID)
	// Camp deaf-detector: rebind on a fresh port if we keep announcing but the
	// camp stops answering. Clock-agnostic (accumulated silence, not a clock
	// jump), so it also catches the case a VM's host slept and the guest saw no
	// time jump — the wake detector in the engine would miss that.
	go s.livenessLoop(ctx, ac)
	// Push our per-peer connection matrix to the camp (POST /api/links) so the
	// camp view / other peers can see who reaches whom (direct/half/seen/relay).
	go s.linksReportLoop(ctx, c.ServerURL, campID)

	s.mu.Lock()
	s.announce = ac
	s.cancel = cancel
	s.announceDone = announceDone
	s.unregisterUDP = unreg
	s.mu.Unlock()
	// self is populated asynchronously by the UDP dispatch handler
	// when the first announce reply arrives.
	return nil
}

// Stop signals goroutines to exit, removes the UDP handler from the
// engine, and waits up to 2s per loop for clean exit.
func (s *Service) Stop() error {
	s.mu.Lock()
	cancel := s.cancel
	announceDone := s.announceDone
	unreg := s.unregisterUDP
	s.announce = nil
	s.cancel = nil
	s.announceDone = nil
	s.unregisterUDP = nil
	s.mu.Unlock()
	s.curCampID.Store(nil)
	s.httpOK.Store(false) // next Start re-probes /api/id fresh
	if unreg != nil {
		unreg()
	}
	if cancel != nil {
		cancel()
	}
	if announceDone != nil {
		select {
		case <-announceDone:
		case <-time.After(2 * time.Second):
			clog.Warn("camp", "announce loop did not exit in 2s")
		}
	}
	return nil
}

// Active reports whether the announce client is currently running.
func (s *Service) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.announce != nil
}

// Self returns our PeerInfo as camp last reported it on an announce
// reply (our external endpoint + camp-assigned tunnel IP). nil
// before the first reply.
func (s *Service) Self() *rendezvous.PeerInfo {
	return s.self.Load()
}

// Reflex returns the current external endpoint camp sees us at, or
// "" before the first announce reply. Falls back to PublicIP when
// UDPEndpoint is empty (camp without UDP visibility).
func (s *Service) Reflex() string {
	self := s.self.Load()
	if self == nil {
		return ""
	}
	if self.UDPEndpoint != "" {
		return self.UDPEndpoint
	}
	return self.PublicIP
}

// Snapshot returns the latest roster from an announce reply (empty
// slice before the first reply). Read lock-free.
func (s *Service) Snapshot() []rendezvous.PeerInfo {
	p := s.snapshot.Load()
	if p == nil {
		return nil
	}
	out := make([]rendezvous.PeerInfo, len(*p))
	copy(out, *p)
	return out
}

// AnnounceStats returns UDP-side liveness signals for the UI's
// camp-health card (LastSentMs / LastReplyMs / LastRTTMs). Zero
// values when announce isn't running.
func (s *Service) AnnounceStats() (sentMs, replyMs, rttMs int64) {
	s.mu.Lock()
	ac := s.announce
	s.mu.Unlock()
	if ac == nil {
		return 0, 0, 0
	}
	return ac.LastSentMs(), ac.LastReplyMs(), ac.LastRTTMs()
}

// onUpdate is the announce-reply roster callback. Stores a snapshot for
// the UI hot path, pushes the roster into the engine so the live peers
// map reconciles, and persists the roster into the per-camp catalog so
// the UI sees known nodes (incl. currently-offline) on the next start.
func (s *Service) onUpdate(peers []rendezvous.PeerInfo, paged, cycleEnd bool) {
	// Once the HTTP roster poll works, it's the authoritative source (full
	// PeerInfo, no paging) — ignore the UDP reply's roster to avoid the two
	// fighting. UDP still registers us + keeps the NAT mapping alive.
	if s.httpOK.Load() {
		return
	}
	if !paged {
		// Legacy / small-camp full list — authoritative, reconcile now.
		s.applyRoster(peers)
		return
	}
	// Paged: each reply is a WINDOW. Accumulate across windows; reconcile the
	// engine's peer set only when a window completes the rotation (cycleEnd),
	// so a peer is dropped only after it failed to appear in a whole cycle —
	// never on a window that simply doesn't include it.
	if s.cycleAcc == nil {
		s.cycleAcc = map[string]rendezvous.PeerInfo{}
	}
	for _, p := range peers {
		if p.Pub != "" {
			s.cycleAcc[p.Pub] = p
		}
	}
	if !cycleEnd {
		return
	}
	// Cycle complete. Apply grace so a single lost window-reply doesn't evict the
	// peers it carried: a known peer absent this cycle is carried forward until it
	// has missed rosterGraceCycles cycles in a row, then truly dropped.
	if s.rosterSeen == nil {
		s.rosterSeen = map[string]rendezvous.PeerInfo{}
		s.rosterMiss = map[string]int{}
	}
	for pub, p := range s.cycleAcc { // present now → refresh, reset miss-count
		s.rosterSeen[pub] = p
		s.rosterMiss[pub] = 0
	}
	for pub := range s.rosterSeen { // absent now → bump, drop past the grace
		if _, present := s.cycleAcc[pub]; present {
			continue
		}
		s.rosterMiss[pub]++
		if s.rosterMiss[pub] > rosterGraceCycles {
			delete(s.rosterSeen, pub)
			delete(s.rosterMiss, pub)
		}
	}
	s.cycleAcc = nil
	full := make([]rendezvous.PeerInfo, 0, len(s.rosterSeen))
	for _, p := range s.rosterSeen {
		full = append(full, p)
	}
	s.applyRoster(full)
}

// rosterGraceCycles is how many full roster cycles a known peer may be absent
// (e.g. its paged window-reply was lost) before we evict it. At a ~4s announce
// and a few windows per cycle this is tens of seconds — long enough to ride out
// UDP loss, short enough that a peer that truly left disappears promptly.
const rosterGraceCycles = 2

// applyRoster pushes an authoritative roster into the engine (reconciling the
// live peer map), refreshes the UI snapshot, and persists the catalog.
func (s *Service) applyRoster(peers []rendezvous.PeerInfo) {
	dup := append([]rendezvous.PeerInfo(nil), peers...)
	s.snapshot.Store(&dup)
	s.eng.ApplyCampRoster(toRoster(peers)) // map wire shape → engine's neutral type
	s.persistCatalog(peers)
}

// toRoster maps the camp server's wire reply into the engine's neutral
// RosterEntry input. Keeps the rendezvous protocol types out of the
// engine's public API — the engine never learns the wire format.
func toRoster(peers []rendezvous.PeerInfo) []engine.RosterEntry {
	out := make([]engine.RosterEntry, 0, len(peers))
	for _, p := range peers {
		out = append(out, engine.RosterEntry{
			Name:        p.Name,
			Pub:         p.Pub,
			PublicIP:    p.PublicIP,
			UDPPort:     p.UDPPort,
			UDPEndpoint: p.UDPEndpoint,
			JoinedAt:    p.JoinedAt,
			LastSeenAt:  p.LastSeenAt,
			Online:      p.Online,
			Role:        p.Role,
			Version:     p.Version,
		})
	}
	return out
}

// persistCatalog merges the latest roster into the running camp's
// PeerCatalog on disk. Best-effort: a lost write is re-applied on the
// next announce reply. Runs on the announce goroutine, so it must not
// touch s.mu (Stop holds it while waiting for that goroutine to exit) —
// it reads curCampID lock-free instead.
func (s *Service) persistCatalog(peers []rendezvous.PeerInfo) {
	if s.store == nil {
		return
	}
	idp := s.curCampID.Load()
	if idp == nil {
		return
	}
	if err := s.store.UpdateCamp(*idp, func(c *config.Camp) {
		mergeRoster(c, peers)
	}); err != nil {
		clog.Warn("camp", "persist peer catalog: %v", err)
	}
}

// mergeRoster upserts every PeerInfo from a roster into c.PeerCatalog.
// Existing entries are refreshed in place; new peers are appended;
// removed peers stay — the catalog is historical (no node deletion yet).
// Our own entry (matched by display name) is skipped. When camp reports
// a peer offline its endpoint fields go blank, so we preserve the
// previously-known values — the catalog is our long-term memory of who
// has been in the camp. (Moved here from the engine, which no longer
// writes config.)
func mergeRoster(c *config.Camp, peers []rendezvous.PeerInfo) {
	ourName := c.Identity.Name
	byPub := make(map[string]int, len(c.PeerCatalog))
	for i, p := range c.PeerCatalog {
		if p.Pub != "" {
			byPub[p.Pub] = i
		}
	}
	for _, p := range peers {
		if p.Pub == "" || p.Name == ourName {
			continue
		}
		entry := config.Peer{
			Name:        p.Name,
			Pub:         p.Pub,
			PublicIP:    p.PublicIP,
			UDPPort:     p.UDPPort,
			UDPEndpoint: p.UDPEndpoint,
			JoinedAt:    p.JoinedAt,
			LastSeenAt:  p.LastSeenAt,
			Online:      p.Online,
		}
		idx, ok := byPub[p.Pub]
		if !ok {
			byPub[p.Pub] = len(c.PeerCatalog)
			c.PeerCatalog = append(c.PeerCatalog, entry)
			continue
		}
		if !p.Online {
			prev := c.PeerCatalog[idx]
			if entry.PublicIP == "" {
				entry.PublicIP = prev.PublicIP
			}
			if entry.UDPEndpoint == "" {
				entry.UDPEndpoint = prev.UDPEndpoint
			}
			if entry.UDPPort == 0 {
				entry.UDPPort = prev.UDPPort
			}
			if entry.JoinedAt == 0 {
				entry.JoinedAt = prev.JoinedAt
			}
			if entry.LastSeenAt == 0 {
				entry.LastSeenAt = prev.LastSeenAt
			}
		}
		c.PeerCatalog[idx] = entry
	}
}

// sameUDPAddr compares two *net.UDPAddr by IP+Port (nil-safe).
func sameUDPAddr(a, b *net.UDPAddr) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.IP.Equal(b.IP) && a.Port == b.Port
}

// livenessLoop rebinds the engine on a fresh port when the camp goes silent: we
// keep announcing (lastSent fresh) but no reply has arrived for deafMs, after
// we'd heard from camp at least once. Clock-agnostic — it needs only
// accumulated silence, so it catches a VM whose host slept without the guest
// seeing a wall-clock jump (which the engine's wake detector keys on) and any
// other silent socket death. A cooldown avoids thrashing when the camp server
// itself is down. Rebinding tears this loop down (ctx cancel) and a fresh one
// starts with the new announce client.
func (s *Service) livenessLoop(ctx context.Context, ac *rendezvous.AnnounceClient) {
	const (
		checkEvery = 15 * time.Second
		deafMs     = 60000  // no camp reply this long while announcing → rebind
		cooldownMs = 120000 // min gap between rebinds (camp-server-down safety)
	)
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	var lastRebind int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		sent, reply := ac.LastSentMs(), ac.LastReplyMs()
		now := time.Now().UnixMilli()
		// Need: the path worked once (reply>0), we've sent since then, and it's
		// been silent for deafMs. Skip if we rebound within the cooldown.
		if reply == 0 || sent <= reply || now-reply < deafMs {
			continue
		}
		if lastRebind != 0 && now-lastRebind < cooldownMs {
			continue
		}
		lastRebind = now
		clog.Warn("camp", "no announce reply for %ds — rebinding on a fresh port", (now-reply)/1000)
		s.eng.RebindEphemeral("camp deaf")
	}
}

// linksReportLoop periodically POSTs our per-peer connection matrix to the camp
// (/api/links/<camp>) so the camp view and other peers can see who reaches whom.
// State per peer: direct (paired), half (one-way), seen (in roster, no crypto
// signal yet). Runs until ctx is cancelled (Stop / rebind).
func (s *Service) linksReportLoop(ctx context.Context, serverURL, campID string) {
	base := httpRosterBase(serverURL)
	if base == "" {
		return
	}
	endpoint := base + "/api/links/" + campID
	client := &http.Client{Timeout: 8 * time.Second}
	t := time.NewTicker(12 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pub := s.eng.IdentityPub()
		if pub == "" {
			continue
		}
		st := s.eng.Status()
		links := make([]rendezvous.Link, 0, len(st.Peers))
		for _, p := range st.Peers {
			if p.Self || p.Fp == "" {
				continue
			}
			var state string
			switch {
			case p.Paired:
				state = "direct"
			case p.HalfPaired:
				state = "half"
			case p.InCamp:
				state = "seen"
			default:
				continue // offline / not in camp — no meaningful link
			}
			links = append(links, rendezvous.Link{Fp: p.Fp, St: state, RTT: int(p.RTTMs)})
		}
		body, err := json.Marshal(rendezvous.LinksReport{Pub: pub, Links: links})
		if err != nil {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("content-type", "application/json")
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	}
}

// httpRosterLoop polls the camp server's /api/id/<camp> for the full roster and
// applies it. On first success it flips httpOK so the UDP reply's roster is
// ignored (this becomes the authoritative source). A non-200 (old server with
// no /api/id) or a network error just leaves httpOK as-is → UDP stays the
// fallback. Runs until ctx is cancelled (Stop).
func (s *Service) httpRosterLoop(ctx context.Context, serverURL, campID string) {
	base := httpRosterBase(serverURL)
	if base == "" {
		return // no usable server URL — stick with the UDP roster
	}
	endpoint := base + "/api/id/" + campID
	client := &http.Client{Timeout: 8 * time.Second}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		if peers, ok := fetchRoster(ctx, client, endpoint); ok {
			if s.httpOK.CompareAndSwap(false, true) {
				clog.Info("camp", "roster via http %s", endpoint)
			}
			s.applyRoster(peers)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// fetchRoster GETs the JSON roster. ok=false on any error or non-200 (e.g. an
// old server returning 404), which the caller treats as "fall back to UDP".
func fetchRoster(ctx context.Context, c *http.Client, endpoint string) ([]rendezvous.PeerInfo, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var body struct {
		Peers []rendezvous.PeerInfo `json:"peers"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return nil, false
	}
	return body.Peers, true
}

// httpRosterBase derives the HTTP(S) base URL of the camp server from its
// ServerURL (a ws:// or wss:// endpoint): wss→https, ws→http, path dropped.
// "" if unparseable.
func httpRosterBase(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := "https"
	if u.Scheme == "ws" || u.Scheme == "http" {
		scheme = "http"
	}
	return scheme + "://" + u.Host
}
