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
	"context"
	"errors"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

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
	eng   *engine.Engine
	store *config.Store

	mu            sync.Mutex
	announce      *rendezvous.AnnounceClient
	cancel        context.CancelFunc
	announceDone  chan struct{}
	unregisterUDP func()

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
func New(eng *engine.Engine, store *config.Store) *Service {
	return &Service{eng: eng, store: store}
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
	ac, err := rendezvous.NewAnnounceClient(udp, c.StunAddr, c.Identity.Name, c.CampID, pub)
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
					log.Printf("camp: registered as %s in camp %s, reflex=%s", campName, campID, reflex)
				}
			}
		}
		return claimed
	})

	ctx, cancel := context.WithCancel(context.Background())
	announceDone := make(chan struct{})
	go func() {
		defer close(announceDone)
		ac.Run(ctx, 20*time.Second) // sends immediately on entry, then every 20s
	}()

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
			log.Printf("WARN: camp announce loop did not exit in 2s")
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
func (s *Service) onUpdate(peers []rendezvous.PeerInfo) {
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
		log.Printf("camp: persist peer catalog: %v", err)
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
