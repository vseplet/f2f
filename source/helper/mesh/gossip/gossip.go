// Package gossip replicates each node's fabric-level state across the mesh
// over the QUIC bus. This is LOW-LEVEL state owned by the fabric itself —
// platform info (OS/arch/host) and the node's peer-reachability view ("who
// sees whom") — NOT application data (files/domains live in their own
// services). Every node publishes its NodeState; gossip caches the latest
// from each peer, so combining All() yields a mesh-wide topology + inventory.
//
// gossip depends only on the bus (transport). Our local NodeState is supplied
// by main via a Source callback, so gossip never imports engine/platform —
// it just defines and moves the NodeState type.
package gossip

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/vseplet/f2f/source/helper/mesh/bus"
)

const (
	msgType       = "gossip"
	announceEvery = 20 * time.Second
)

// Platform is a node's static environment fingerprint.
type Platform struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname,omitempty"`
	NumCPU   int    `json:"num_cpu,omitempty"`
	Go       string `json:"go,omitempty"`
}

// PeerLink is one edge of a node's reachability view.
type PeerLink struct {
	Pub       string `json:"pub"`
	Name      string `json:"name,omitempty"`
	Paired    bool   `json:"paired,omitempty"`
	Reachable bool   `json:"reachable,omitempty"`
	RTTMs     int64  `json:"rtt_ms,omitempty"`
}

// NodeState is one node's fabric snapshot: identity, platform, and the
// peers it currently sees. Versioned by TS so stale pushes don't win.
type NodeState struct {
	Pub      string     `json:"pub"`
	Name     string     `json:"name,omitempty"`
	Platform Platform   `json:"platform"`
	Sees     []PeerLink `json:"sees,omitempty"`
	TS       int64      `json:"ts"`
}

// Source returns our current local NodeState. Wired in main from
// engine.Status() + platform info.
type Source func() NodeState

// Service replicates NodeState across the mesh and caches peers' states.
type Service struct {
	bus   *bus.Service
	local Source

	mu       sync.Mutex
	peers    map[string]NodeState // pub → latest NodeState
	onChange func(pub string)
	cancel   context.CancelFunc
}

// New wires gossip onto the bus (registers the "gossip" handler). Call
// Start to begin announcing once the bus/overlay is up.
func New(b *bus.Service, local Source) *Service {
	s := &Service{bus: b, local: local, peers: make(map[string]NodeState)}
	b.Handle(msgType, s.handle)
	return s
}

// Start begins the periodic announce loop. Idempotent.
func (s *Service) Start() {
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.mu.Unlock()
	go s.loop(ctx)
}

// Stop halts the announce loop. Idempotent.
func (s *Service) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// OnChange registers a callback fired when a peer's NodeState updates (for
// UI refresh / notifications). Replaces any previous callback.
func (s *Service) OnChange(fn func(pub string)) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// Peer returns a peer's latest NodeState.
func (s *Service) Peer(pub string) (NodeState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.peers[pub]
	return st, ok
}

// All returns every cached peer NodeState (copy).
func (s *Service) All() map[string]NodeState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]NodeState, len(s.peers))
	for k, v := range s.peers {
		out[k] = v
	}
	return out
}

// Announce pushes our current NodeState to every reachable peer.
func (s *Service) Announce() {
	blob, err := json.Marshal(s.localStamped())
	if err != nil {
		return
	}
	for _, pub := range s.bus.Peers() {
		go func(p string) { _ = s.bus.Notify(p, msgType, blob) }(pub)
	}
}

func (s *Service) loop(ctx context.Context) {
	s.Announce()
	t := time.NewTicker(announceEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Announce()
		}
	}
}

func (s *Service) localStamped() NodeState {
	st := s.local()
	if st.TS == 0 {
		st.TS = time.Now().UnixMilli()
	}
	return st
}

// handle is the bus handler for "gossip": an empty payload is a pull (return
// our state); a non-empty one is a push (store the peer's state).
func (s *Service) handle(fromPub string, payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return json.Marshal(s.localStamped())
	}
	var st NodeState
	if err := json.Unmarshal(payload, &st); err != nil {
		return nil, err
	}
	s.store(fromPub, st)
	return nil, nil
}

func (s *Service) store(fromPub string, st NodeState) {
	s.mu.Lock()
	if prev, ok := s.peers[fromPub]; ok && st.TS != 0 && st.TS < prev.TS {
		s.mu.Unlock()
		return // stale
	}
	s.peers[fromPub] = st
	onChange := s.onChange
	s.mu.Unlock()
	if onChange != nil {
		onChange(fromPub)
	}
}
