// Package camp is the user-facing service that talks to the
// rendezvous server (camp): announces our presence on UDP, polls the
// peer roster on HTTP, and pushes roster updates into the engine so
// pair-handshake / hole-punch have someone to dial.
//
// Layering:
//
//   - engine/rendezvous: wire protocol (UDP announce + HTTP roster).
//   - this package: lifecycle, AnnounceClient ownership, periodic
//     poll loop, UDP-dispatch hook registered with the engine, public
//     getters for the UI (Self, Snapshot, stats).
//
// Engine surface used:
//
//   - eng.UDPConn() / eng.IdentityPub() / eng.CampConfigSnapshot() for setup
//   - eng.RegisterUDPHandler(fn) to receive camp-source packets
//   - eng.ApplyCampRoster(peers) to push polled roster into peers map
package camp

import (
	"context"
	"errors"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vseplet/f2f/source/helper/engine"
	"github.com/vseplet/f2f/source/helper/services/camp/rendezvous"
)

// Health aggregates UDP-announce and HTTP-poll liveness signals
// against the camp server. The two halves travel different
// transports, so split health makes asymmetric failures visible
// (e.g. HTTP fine + UDP wedged after laptop sleep). Surfaced via
// /api/status; computed from announce + poller stats.
type Health struct {
	UDPLastSentMs     int64  `json:"udp_last_sent_ms,omitempty"`
	UDPLastReplyMs    int64  `json:"udp_last_reply_ms,omitempty"`
	UDPRTTMs          int64  `json:"udp_rtt_ms,omitempty"`
	HTTPLastPollMs    int64  `json:"http_last_poll_ms,omitempty"`
	HTTPLastSuccessMs int64  `json:"http_last_success_ms,omitempty"`
	HTTPRTTMs         int64  `json:"http_rtt_ms,omitempty"`
	HTTPLastErr       string `json:"http_last_err,omitempty"`
	HTTPPeersCount    int    `json:"http_peers_count,omitempty"`
}

// HealthSnapshot returns the current camp health based on announce +
// poller counters. Zero value when nothing is running yet.
func (s *Service) HealthSnapshot() *Health {
	h := &Health{}
	sent, reply, rtt := s.AnnounceStats()
	h.UDPLastSentMs = sent
	h.UDPLastReplyMs = reply
	h.UDPRTTMs = rtt
	ps := s.PollerStats()
	h.HTTPLastPollMs = ps.LastPollMs
	h.HTTPLastSuccessMs = ps.LastSuccessMs
	h.HTTPRTTMs = ps.LastRTTMs
	h.HTTPLastErr = ps.LastErr
	h.HTTPPeersCount = ps.PeersCount
	return h
}

// Service owns the AnnounceClient + HTTP poller. Constructed once in
// main.go; Start runs on eng.OnStarted, Stop on eng.OnStopped.
type Service struct {
	eng *engine.Engine

	mu             sync.Mutex
	announce       *rendezvous.AnnounceClient
	poller         *rendezvous.PeerListPoller
	cancel         context.CancelFunc
	announceDone   chan struct{}
	pollDone       chan struct{}
	unregisterUDP  func()

	// snapshot holds the latest polled roster (defensive copy). Read
	// lock-free via atomic for the /api/status hot path.
	snapshot atomic.Pointer[[]rendezvous.PeerInfo]
	// self holds the latest announce reply (our PeerInfo as camp sees
	// us). Read by UI for the self peer-row endpoint display.
	self atomic.Pointer[rendezvous.PeerInfo]
}

// New constructs a Service. The engine must outlive the service.
func New(eng *engine.Engine) *Service {
	return &Service{eng: eng}
}

// Start brings up the announce client + HTTP poller for the given
// camp config. Order matters: register the UDP dispatch handler
// BEFORE the announce client touches the socket — engine's
// peerToTunLoop is already reading from it, and synchronous reads
// (the old AnnounceOnce path) would steal the announce reply and
// wedge peerToTunLoop with i/o timeout. Idempotent — Stop required
// between consecutive Starts for different camps.
func (s *Service) Start(cfg engine.CampConfig) error {
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
	ac, err := rendezvous.NewAnnounceClient(udp, cfg.StunAddr, cfg.Name, cfg.ID, pub)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	// Register the UDP dispatch handler first so any reply from camp
	// reaches HandlePacket via the engine read loop without anyone
	// else having to touch the socket.
	campAddr := ac.CampAddr()
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
					log.Printf("camp: registered as %s in camp %s, reflex=%s", cfg.Name, cfg.ID, reflex)
				}
			}
		}
		return claimed
	})

	base, perr := rendezvous.CampHTTPBase(cfg.URL)
	var poller *rendezvous.PeerListPoller
	if perr != nil {
		log.Printf("camp: %v (peer list disabled)", perr)
	} else {
		poller = rendezvous.NewPeerListPoller(base, cfg.ID, s.onUpdate)
	}

	ctx, cancel := context.WithCancel(context.Background())
	announceDone := make(chan struct{})
	go func() {
		defer close(announceDone)
		ac.Run(ctx, 20*time.Second) // sends immediately on entry, then every 20s
	}()
	var pollDone chan struct{}
	if poller != nil {
		pollDone = make(chan struct{})
		go func() {
			defer close(pollDone)
			poller.Run(ctx, 30*time.Second)
		}()
	}

	s.mu.Lock()
	s.announce = ac
	s.poller = poller
	s.cancel = cancel
	s.announceDone = announceDone
	s.pollDone = pollDone
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
	pollDone := s.pollDone
	unreg := s.unregisterUDP
	s.announce = nil
	s.poller = nil
	s.cancel = nil
	s.announceDone = nil
	s.pollDone = nil
	s.unregisterUDP = nil
	s.mu.Unlock()
	if unreg != nil {
		unreg()
	}
	if cancel != nil {
		cancel()
	}
	for _, done := range []chan struct{}{announceDone, pollDone} {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			log.Printf("WARN: camp loop did not exit in 2s")
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

// Snapshot returns the latest polled roster (empty slice before the
// first successful poll). Read lock-free.
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

// PollerStats returns the live HTTP-poll counters for the UI's
// camp-health card. Zero value when the poller isn't running.
func (s *Service) PollerStats() rendezvous.PollerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poller == nil {
		return rendezvous.PollerStats{}
	}
	return s.poller.Stats()
}

// onUpdate is the poller's callback. Stores a snapshot for the UI
// hot path and pushes the diff into the engine so peers map +
// catalog reconcile.
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
