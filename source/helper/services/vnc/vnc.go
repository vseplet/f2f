// Package vnc is the remote-desktop bridge: it proxies a peer (over the
// QUIC bus) to the host's local VNC server — macOS Screen Sharing on :5900,
// x11vnc/wayvnc on Linux, etc. We do NOT capture or encode anything; the OS
// VNC server does all the work (and its own authentication). This service is
// a thin TCP proxy: bus stream ⟷ TCP localhost:5900.
//
// Layering mirrors services/shell: peer↔peer over the bus, and the local web
// layer bridges a browser noVNC WebSocket to a bus stream opened here.
//
// SECURITY: a desktop is full graphical control of the machine. Access is gated
// by channel membership — only members of a channel the owner exposed the
// desktop to may connect — AND by the VNC server's own auth (password / login).
// Fail-closed: no channels ⇒ nobody may connect.
package vnc

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"sync"
	"time"

	"github.com/vseplet/f2f/source/helper/clog"
	"github.com/vseplet/f2f/source/helper/mesh/bus"
)

// bus message types.
const (
	TypeOpen   = "vnc.open"   // stream: proxy to the local VNC server
	TypeStatus = "vnc.status" // request: is a desktop available to me?
)

const defaultAddr = "127.0.0.1:5900"

type statusResp struct {
	Available bool `json:"available"`
}

// Service proxies bus streams to the host's local VNC server.
type Service struct {
	bus *bus.Service

	mu       sync.Mutex
	channels []string                   // expose the desktop to members of these channels (bids)
	isMember func(bid, pub string) bool // injected channel-membership predicate
	addr     string
}

// New constructs the service. Access is DENIED until channels are exposed via
// SetChannels and a membership predicate is wired with SetMembershipCheck.
func New(b *bus.Service) *Service {
	return &Service{bus: b}
}

// Register wires the bus handlers. Call once after constructing the bus.
func (s *Service) Register() {
	s.bus.HandleStream(TypeOpen, s.handleOpen)
	s.bus.Handle(TypeStatus, s.handleStatus)
}

// SetMembershipCheck wires the channel-membership predicate (channels.IsMember).
func (s *Service) SetMembershipCheck(fn func(bid, pub string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.isMember = fn
}

// SetChannels exposes the desktop to members of the given channels (bids);
// empty closes it. addr overrides the local VNC endpoint ("" = 127.0.0.1:5900).
func (s *Service) SetChannels(channels []string, addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels = append([]string(nil), channels...)
	s.addr = addr
}

// Channels returns the current exposure list (copy).
func (s *Service) Channels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.channels...)
}

// allowed reports whether fromPub may open a desktop here: member of at least
// one exposed channel. Fail-closed (no channels / no predicate ⇒ denied).
func (s *Service) allowed(fromPub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fromPub == "" || s.isMember == nil {
		return false
	}
	for _, bid := range s.channels {
		if s.isMember(bid, fromPub) {
			return true
		}
	}
	return false
}

func (s *Service) target() string {
	s.mu.Lock()
	a := s.addr
	s.mu.Unlock()
	if a == "" {
		return defaultAddr
	}
	return a
}

// handleStatus reports availability: allowed AND a VNC server is actually
// listening locally (so the UI only lists machines with a real desktop).
func (s *Service) handleStatus(fromPub string, _ []byte) ([]byte, error) {
	avail := s.allowed(fromPub) && s.serverUp()
	return json.Marshal(statusResp{Available: avail})
}

func (s *Service) serverUp() bool {
	c, err := net.DialTimeout("tcp", s.target(), 700*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// handleOpen proxies the inbound bus stream to the local VNC server, copying
// bytes both ways until either side closes. RFB (incl. its auth handshake)
// flows end-to-end between the peer's noVNC and our VNC server.
func (s *Service) handleOpen(fromPub string, _ []byte, st *bus.Stream) {
	defer st.Close()
	if !s.allowed(fromPub) {
		return
	}
	conn, err := net.DialTimeout("tcp", s.target(), 4*time.Second)
	if err != nil {
		clog.Warn("vnc", "dial %s: %v", s.target(), err)
		return
	}
	defer conn.Close()
	clog.Info("vnc", "%s → %s", s.bus.Label(fromPub), s.target())

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(conn, st); done <- struct{}{} }() // peer → VNC server
	go func() { _, _ = io.Copy(st, conn); done <- struct{}{} }() // VNC server → peer
	<-done
}

// --- client helpers (used by the web bridge) ---

// Open dials a peer and proxies to its VNC server, returning the raw stream
// (carries RFB both ways). Caller closes it.
func (s *Service) Open(ctx context.Context, pub string) (*bus.Stream, error) {
	return s.bus.OpenStream(ctx, pub, TypeOpen, nil)
}

// Available asks pub whether a desktop is open to us.
func (s *Service) Available(ctx context.Context, pub string) bool {
	resp, err := s.bus.Request(ctx, pub, TypeStatus, nil)
	if err != nil {
		return false
	}
	var r statusResp
	if json.Unmarshal(resp, &r) != nil {
		return false
	}
	return r.Available
}
