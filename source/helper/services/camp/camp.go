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
	"github.com/vseplet/f2f/source/helper/engine"
	"github.com/vseplet/f2f/source/helper/services/camp/rendezvous"
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
	eng *engine.Engine

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
}

// New constructs a Service. The engine must outlive the service.
func New(eng *engine.Engine) *Service {
	return &Service{eng: eng}
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
	campAddr := ac.CampAddr()
	campName := c.Identity.Name
	campID := c.CampID
	unreg := s.eng.RegisterUDPHandler(func(pkt []byte, from *net.UDPAddr) bool {
		if !sameUDPAddr(campAddr, from) {
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

// onUpdate is the announce-reply roster callback. Stores a snapshot
// for the UI hot path and pushes the diff into the engine so peers
// map + catalog reconcile.
func (s *Service) onUpdate(peers []rendezvous.PeerInfo) {
	dup := append([]rendezvous.PeerInfo(nil), peers...)
	s.snapshot.Store(&dup)
	s.eng.ApplyCampRoster(peers)
}

// sameUDPAddr compares two *net.UDPAddr by IP+Port (nil-safe).
func sameUDPAddr(a, b *net.UDPAddr) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.IP.Equal(b.IP) && a.Port == b.Port
}
