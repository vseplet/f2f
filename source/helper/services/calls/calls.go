// Package calls is the user-facing service for group voice/video
// calls inside a camp. One peer hosts the SFU (Selective Forwarding
// Unit) for the whole call; others join over the camp's overlay v4
// addresses through the SFU's HTTP signalling endpoint.
//
// The service owns:
//
//   - Local call state (one active call hosted by us, if any).
//   - Remote call discovery: PollPeers walks online peers every 3s
//     and asks /api/call/state for any call they're currently
//     hosting; the union shows up in the UI's "active calls" list.
//   - SFU signalling delivery: the SFU emits messages addressed to
//     either the local browser (via the OnLocalSignal hook) or a
//     remote peer's tunnel HTTP endpoint.
//
// engine.Engine is consulted for Status (LocalIP, UtunName, CampName),
// TunnelHTTPPort, and OnlinePeers; no engine state is owned here.
package calls

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/mesh/engine"
	"github.com/vseplet/f2f/source/helper/services/calls/sfu"
)

// State is one row in the UI's "active calls" list. Local + remote
// calls share the same shape; Remote=true distinguishes peer-hosted
// calls discovered via polling.
type State struct {
	CallID       string                `json:"call_id"`
	SFUHost      string                `json:"sfu_host"`
	Participants []sfu.ParticipantInfo `json:"participants"`
	StartedAt    time.Time             `json:"started_at"`
	Remote       bool                  `json:"remote"`
}

// callCtx pairs the published call State with the running SFU
// instance. Stored under Service.call when a call is active.
type callCtx struct {
	state State
	sfu   *sfu.SFU
}

// Service owns the active local call + the cache of remote calls.
// Construct once in main.go; the SFU lifecycle is tied to
// CreateCall/EndCall, not to engine start/stop.
type Service struct {
	eng   *engine.Engine
	store *config.Store

	// OnLocalSignal delivers SFU signal messages destined for the
	// local browser, bypassing HTTP-through-tunnel. Set by main.go
	// after the web layer is constructed.
	OnLocalSignal func(msg []byte)

	call          atomic.Value // *callCtx
	remoteCalls   atomic.Value // *[]State
	joinedSFUHost atomic.Value // *string

	// only used to serialise mutations on call state; the atomic
	// pointer protects the read fast-path.
	mu sync.Mutex
}

// New constructs a Service. The engine and store must outlive it.
func New(store *config.Store, eng *engine.Engine) *Service {
	return &Service{store: store, eng: eng}
}

// --- state helpers ---

func (s *Service) loadCall() *callCtx {
	v := s.call.Load()
	if v == nil {
		return nil
	}
	cc, _ := v.(*callCtx)
	return cc
}

func (s *Service) loadRemoteCalls() []State {
	v := s.remoteCalls.Load()
	if v == nil {
		return nil
	}
	p, _ := v.(*[]State)
	if p == nil {
		return nil
	}
	return *p
}

func (s *Service) clearCall() {
	s.call.Store((*callCtx)(nil))
}

func (s *Service) storeRemoteCalls(calls []State) {
	s.remoteCalls.Store(&calls)
}

// --- joined SFU host (the peer whose call we joined as participant) ---

func (s *Service) JoinedSFUHost() string {
	v := s.joinedSFUHost.Load()
	if v == nil {
		return ""
	}
	p, _ := v.(*string)
	if p == nil {
		return ""
	}
	return *p
}

func (s *Service) SetJoinedSFUHost(host string) {
	s.joinedSFUHost.Store(&host)
}

func (s *Service) ClearJoinedSFUHost() {
	s.joinedSFUHost.Store((*string)(nil))
}

// --- read-only views for UI ---

// LocalCall returns the call currently hosted by this peer, or nil.
func (s *Service) LocalCall() *State {
	if cc := s.loadCall(); cc != nil {
		st := cc.state
		st.Participants = cc.sfu.Participants()
		st.Remote = false
		return &st
	}
	return nil
}

// RemoteCalls returns the cached list of calls discovered on other
// peers (refreshed every 3s by PollPeers).
func (s *Service) RemoteCalls() []State {
	return s.loadRemoteCalls()
}

// AllCalls returns local + remote calls for the UI.
func (s *Service) AllCalls() []State {
	var out []State
	if cs := s.LocalCall(); cs != nil {
		out = append(out, *cs)
	}
	out = append(out, s.loadRemoteCalls()...)
	return out
}

// SFU returns the running SFU instance for the local call, or nil.
// Web layer needs it for /api/call/signal forwarding into the SFU.
func (s *Service) SFU() *sfu.SFU {
	if cc := s.loadCall(); cc != nil {
		return cc.sfu
	}
	return nil
}

// --- lifecycle ---

// Create starts a new SFU on this peer and adds the local user as
// the first participant. Errors if a call is already in progress or
// the engine isn't running.
func (s *Service) Create() (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cc := s.loadCall(); cc != nil {
		return nil, fmt.Errorf("call already active")
	}
	st := s.eng.Status()
	if !st.Running {
		return nil, fmt.Errorf("engine not running")
	}
	s.ClearJoinedSFUHost()

	sfuInst := sfu.New(st.LocalIP, st.UtunName, func(to string, msg []byte) {
		s.deliverSignal(to, msg)
	})

	cc := &callCtx{
		state: State{
			CallID:    fmt.Sprintf("%d", time.Now().UnixNano()),
			SFUHost:   st.LocalIP,
			StartedAt: time.Now(),
		},
		sfu: sfuInst,
	}
	s.call.Store(cc)

	var ourName string
	if cc, _ := s.store.SnapshotCamp(st.CampID); cc != nil {
		ourName = cc.Identity.Name
	}
	if _, err := sfuInst.AddParticipant(st.LocalIP, ourName); err != nil {
		sfuInst.Close()
		s.clearCall()
		return nil, fmt.Errorf("add self to sfu: %w", err)
	}
	log.Printf("call: created %s, sfu host %s", cc.state.CallID, st.LocalIP)
	return s.LocalCall(), nil
}

// Join adds a participant by tunnel IP to the active local call.
// Errors if no call is hosted here.
func (s *Service) Join(tunnelIP, name string) error {
	cc := s.loadCall()
	if cc == nil {
		return fmt.Errorf("no active call on this host")
	}
	_, err := cc.sfu.AddParticipant(tunnelIP, name)
	return err
}

// Leave removes a participant from the active local call. If the
// caller is the host, or the last participant, the call ends.
func (s *Service) Leave(tunnelIP string) {
	cc := s.loadCall()
	if cc == nil {
		return
	}
	if tunnelIP == cc.state.SFUHost {
		cc.sfu.Close()
		s.clearCall()
		log.Printf("call: ended (host left)")
		return
	}
	cc.sfu.RemoveParticipant(tunnelIP)
	if len(cc.sfu.Participants()) == 0 {
		cc.sfu.Close()
		s.clearCall()
		log.Printf("call: ended (last participant left)")
	}
}

// End forces the active local call to terminate.
func (s *Service) End() {
	cc := s.loadCall()
	if cc == nil {
		return
	}
	cc.sfu.Close()
	s.clearCall()
	log.Printf("call: ended")
}

// HandleSignal forwards a peer-originated SFU signal into the local
// SFU instance. Used by the web layer's /api/call/signal handler on
// the tunnel listener.
func (s *Service) HandleSignal(fromTunnelIP string, body []byte) ([]byte, error) {
	cc := s.loadCall()
	if cc == nil {
		return nil, fmt.Errorf("no active call")
	}
	return cc.sfu.HandleSignal(fromTunnelIP, body)
}

// Reset is called from engine.Stop (via the lifecycle hook) so the
// active call is torn down when the tunnel goes away. Idempotent.
func (s *Service) Reset() {
	if cc := s.loadCall(); cc != nil {
		cc.sfu.Close()
		s.clearCall()
	}
	s.ClearJoinedSFUHost()
	s.storeRemoteCalls(nil)
}

// --- signalling delivery ---

func (s *Service) deliverSignal(to string, msg []byte) {
	st := s.eng.Status()
	if to == st.LocalIP && s.OnLocalSignal != nil {
		s.OnLocalSignal(msg)
		return
	}
	port := s.eng.TunnelHTTPPort()
	if port == "" {
		port = "2202"
	}
	url := "http://" + to + ":" + port + "/api/call/signal"
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Post(url, "application/json", bytes.NewReader(msg))
		if err != nil {
			log.Printf("call: deliver signal to %s: %v", to, err)
			return
		}
		resp.Body.Close()
	}()
}

// --- remote call polling ---

// PollPeers blocks until ctx is done, walking online peers every 3s
// and asking each for the call it's hosting (if any). The union
// surfaces in the UI's "active calls" list via RemoteCalls.
func (s *Service) PollPeers(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
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
		return
	}
	targets := s.eng.OnlinePeersForCAPoll()
	if len(targets) == 0 {
		s.storeRemoteCalls(nil)
		return
	}
	client := &http.Client{Timeout: 3 * time.Second}
	var found []State
	for _, t := range targets {
		url := "http://" + net.JoinHostPort(t.Host, port) + "/api/call/state"
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		var cs State
		if err := json.NewDecoder(resp.Body).Decode(&cs); err != nil {
			resp.Body.Close()
			continue
		}
		resp.Body.Close()
		if cs.CallID != "" {
			cs.Remote = true
			found = append(found, cs)
		}
	}
	s.storeRemoteCalls(found)
}
