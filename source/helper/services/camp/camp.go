// Package camp is the user-facing service that polls the rendezvous
// server (camp), maintains the local view of who's in the camp, and
// pushes roster updates into the engine so hole-punching and pair-
// handshake have someone to talk to.
//
// Stage 1: HTTP peer-list poll only. UDP announce (which observes
// our own external endpoint as side-effect) still lives in
// engine.Start — that's a separate decoupling step.
//
// Layering:
//
//   - engine/rendezvous: wire protocol (UDP announce + HTTP roster).
//   - this package: service lifecycle, periodic poll loop, applying
//     roster diffs via engine.ApplyCampRoster.
package camp

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vseplet/f2f/source/helper/engine"
	"github.com/vseplet/f2f/source/helper/engine/rendezvous"
)

// Service owns the HTTP roster poller. Constructed once in main.go;
// Start is driven by eng.OnStarted with the active camp config.
type Service struct {
	eng *engine.Engine

	mu     sync.Mutex
	poller *rendezvous.PeerListPoller
	cancel context.CancelFunc
	done   chan struct{}

	// snapshot holds the latest polled roster (defensive copy). Read
	// lock-free via atomic for the /api/status hot path.
	snapshot atomic.Pointer[[]rendezvous.PeerInfo]
}

// New constructs a Service. The engine must outlive the service.
func New(eng *engine.Engine) *Service {
	return &Service{eng: eng}
}

// Start brings up the HTTP poller for the given camp config and
// spawns a goroutine that runs until Stop. Idempotent enough — Stop
// is required between consecutive Starts for different camps.
func (s *Service) Start(cfg engine.CampConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poller != nil {
		return nil
	}
	base, err := rendezvous.CampHTTPBase(cfg.URL)
	if err != nil {
		log.Printf("camp: %v (peer list disabled)", err)
		return err
	}
	s.poller = rendezvous.NewPeerListPoller(base, cfg.ID, s.onUpdate)
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		s.poller.Run(ctx, 30*time.Second)
	}()
	return nil
}

// Stop signals the poller goroutine to exit and waits for it. Safe
// to call when never started.
func (s *Service) Stop() error {
	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	s.poller = nil
	s.cancel = nil
	s.done = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			log.Printf("WARN: camp poller did not exit in 2s")
		}
	}
	return nil
}

// onUpdate is the poller's callback. Stores a snapshot for the UI
// hot path and pushes the diff into the engine so peers map +
// catalog reconcile.
func (s *Service) onUpdate(peers []rendezvous.PeerInfo) {
	dup := append([]rendezvous.PeerInfo(nil), peers...)
	s.snapshot.Store(&dup)
	s.eng.ApplyCampRoster(peers)
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
