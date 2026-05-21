// Package lite is the userspace-only f2f client: open a UDP socket,
// announce to camp, learn peers, keep NAT mappings open by
// hole-punching. No utun, no pf, no DNS hijack, no privileged ports
// — runs as the regular user. Built for the desktop GUI client.
package lite

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vseplet/f2f/source/desktop/internal/rendezvous"
)

// Config is what the UI passes when joining a camp.
type Config struct {
	CampURL  string // wss://f2f-camp.fly.dev/ws (only host:port really used here)
	StunAddr string // f2f-camp.fly.dev:3478 — camp's UDP receiver for announce
	Name     string
	ID       string
}

// PeerSnap is one row in the UI peer table — a snapshot of what we
// know right now. Reachable means hole-punch is alive (received a
// packet from this peer in the last 30s).
type PeerSnap struct {
	Name        string `json:"name"`
	TunnelIP    string `json:"tunnel_ip"`
	PublicIP    string `json:"public_ip"`
	UDPEndpoint string `json:"udp_endpoint"`
	Online      bool   `json:"online"`     // camp says it announced recently
	Reachable   bool   `json:"reachable"`  // we got UDP from it recently
	LastSeenMs  int64  `json:"last_seen_ms"`
	Self        bool   `json:"self,omitempty"`
}

// Status is the live snapshot the UI polls.
type Status struct {
	Running    bool       `json:"running"`
	Name       string     `json:"name,omitempty"`
	CampID     string     `json:"camp_id,omitempty"`
	TunnelIP   string     `json:"tunnel_ip,omitempty"`
	UDPLocal   string     `json:"udp_local,omitempty"`   // our local bind address
	Reflex     string     `json:"reflex,omitempty"`      // public endpoint per camp
	Peers      []PeerSnap `json:"peers"`
}

// peer is our local view of one camp member.
type peer struct {
	info       rendezvous.PeerInfo
	udpAddr    *net.UDPAddr
	lastSeenMs atomic.Int64
}

// signalPrefix marks a UDP packet as "this is a signal-frame, not
// tunnel/holepunch/announce traffic". Picked from 0xF0-0xFF: above the
// IPv4 (0x40-0x4F) and IPv6 (0x60-0x6F) version-byte ranges and not a
// 1-byte hole-punch ping. The full mac engine recognises the same byte
// (when we wire it on that side) so signalling between lite and full
// peers shares one transport.
const signalPrefix byte = 0xF2

// Client is the lite-mode runtime.
type Client struct {
	mu sync.Mutex

	cfg     Config
	udp     *net.UDPConn
	ann     *rendezvous.AnnounceClient
	poller  *rendezvous.PeerListPoller
	cancel  context.CancelFunc
	workers sync.WaitGroup
	running atomic.Bool

	peers map[string]*peer // keyed by tunnel_ip

	tunnelIP    atomic.Value // string
	reflex      atomic.Value // string
	campUDPAddr atomic.Value // *net.UDPAddr (camp endpoint)

	// OnSignal is invoked from the recv loop when a 0xF2-prefixed
	// signal-frame arrives. The body is the bytes following the
	// prefix (we don't interpret them — that's the UI layer's job:
	// WebRTC SDP, ICE candidates, ad-hoc pings, whatever). nil-safe.
	OnSignal func(fromTunnelIP string, body []byte)
}

func New() *Client {
	return &Client{peers: map[string]*peer{}}
}

// Start joins the camp: open UDP socket on an ephemeral port,
// announce, kick off peer-list poller + announce loop + hole-punch.
// Idempotent — repeated Start while running returns an error.
func (c *Client) Start(cfg Config) error {
	if !c.running.CompareAndSwap(false, true) {
		return errors.New("lite: already running")
	}
	if cfg.Name == "" || cfg.ID == "" || cfg.StunAddr == "" || cfg.CampURL == "" {
		c.running.Store(false)
		return errors.New("lite: Name, ID, StunAddr, CampURL all required")
	}

	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		c.running.Store(false)
		return fmt.Errorf("lite: bind udp: %w", err)
	}
	c.udp = udp
	log.Printf("lite: UDP listening on %s", udp.LocalAddr())

	ac, err := rendezvous.NewAnnounceClient(udp, cfg.StunAddr, cfg.Name, cfg.ID)
	if err != nil {
		_ = udp.Close()
		c.running.Store(false)
		return fmt.Errorf("lite: announce client: %w", err)
	}
	c.ann = ac
	c.campUDPAddr.Store(ac.CampAddr())

	self, err := ac.AnnounceOnce(5 * time.Second)
	if err != nil {
		_ = udp.Close()
		c.running.Store(false)
		return fmt.Errorf("lite: announce: %w", err)
	}
	if self.TunnelIP == "" {
		_ = udp.Close()
		c.running.Store(false)
		return errors.New("lite: announce reply missing tunnel_ip")
	}
	c.tunnelIP.Store(self.TunnelIP)
	reflex := self.UDPEndpoint
	if reflex == "" {
		reflex = self.PublicIP
	}
	c.reflex.Store(reflex)
	log.Printf("lite: registered as %s in camp %s, tunnel_ip=%s reflex=%s",
		cfg.Name, cfg.ID, self.TunnelIP, reflex)

	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.cfg = cfg

	// Periodic announce keeps camp's last_seen fresh + NAT mapping open
	// (matters under symmetric NAT).
	c.workers.Add(1)
	go func() {
		defer c.workers.Done()
		ac.Run(ctx, 20*time.Second)
	}()

	// Peer list poller (HTTP) — drops new peers into c.peers.
	base, err := rendezvous.CampHTTPBase(cfg.CampURL)
	if err != nil {
		log.Printf("lite: %v (peer list disabled)", err)
	} else {
		p := rendezvous.NewPeerListPoller(base, cfg.ID, c.applyPeerList)
		c.poller = p
		c.workers.Add(1)
		go func() {
			defer c.workers.Done()
			p.Run(ctx, 30*time.Second)
		}()
	}

	// UDP receive loop — hole-punch ack updates LastSeen.
	c.workers.Add(1)
	go c.recvLoop(ctx)
	// Hole-punch sender — 1Hz burst until each peer answers, then 25s keepalive.
	c.workers.Add(1)
	go c.holePunchLoop(ctx)

	return nil
}

// Stop closes the socket and waits for workers.
func (c *Client) Stop() error {
	if !c.running.CompareAndSwap(true, false) {
		return nil
	}
	if c.cancel != nil {
		c.cancel()
	}
	if c.udp != nil {
		_ = c.udp.Close()
	}
	c.workers.Wait()
	c.mu.Lock()
	c.peers = map[string]*peer{}
	c.mu.Unlock()
	c.tunnelIP.Store("")
	c.reflex.Store("")
	return nil
}

// Status returns the live snapshot for the UI.
func (c *Client) Status() Status {
	st := Status{Running: c.running.Load()}
	if !st.Running {
		return st
	}
	if t, _ := c.tunnelIP.Load().(string); t != "" {
		st.TunnelIP = t
	}
	if r, _ := c.reflex.Load().(string); r != "" {
		st.Reflex = r
	}
	st.Name = c.cfg.Name
	st.CampID = c.cfg.ID
	if c.udp != nil {
		st.UDPLocal = c.udp.LocalAddr().String()
	}
	st.Peers = c.peersSnap()
	return st
}

const reachableWindowMs = 30000

func (c *Client) peersSnap() []PeerSnap {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().UnixMilli()
	out := make([]PeerSnap, 0, len(c.peers)+1)
	if t, _ := c.tunnelIP.Load().(string); t != "" {
		r, _ := c.reflex.Load().(string)
		out = append(out, PeerSnap{
			Name:        c.cfg.Name,
			TunnelIP:    t,
			UDPEndpoint: r,
			Online:      true,
			Reachable:   true,
			Self:        true,
		})
	}
	for _, p := range c.peers {
		seen := p.lastSeenMs.Load()
		out = append(out, PeerSnap{
			Name:        p.info.Name,
			TunnelIP:    p.info.TunnelIP,
			PublicIP:    p.info.PublicIP,
			UDPEndpoint: p.info.UDPEndpoint,
			Online:      p.info.Online,
			Reachable:   p.info.Online && seen != 0 && now-seen < reachableWindowMs,
			LastSeenMs:  seen,
		})
	}
	return out
}

// applyPeerList runs on the poller goroutine. Merges incoming peer
// list with our local map: new peers added, gone peers removed,
// known peers updated (online flag, endpoints).
func (c *Client) applyPeerList(list []rendezvous.PeerInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	keep := make(map[string]struct{}, len(list))
	for _, info := range list {
		if info.Name == c.cfg.Name || info.TunnelIP == "" {
			continue
		}
		keep[info.TunnelIP] = struct{}{}
		ex, ok := c.peers[info.TunnelIP]
		if !ok {
			p := &peer{info: info}
			if addr := parseUDPEndpoint(info.UDPEndpoint); addr != nil {
				p.udpAddr = addr
			}
			c.peers[info.TunnelIP] = p
			log.Printf("lite: peer joined %s (%s) endpoint=%s", info.Name, info.TunnelIP, info.UDPEndpoint)
			continue
		}
		ex.info = info
		if addr := parseUDPEndpoint(info.UDPEndpoint); addr != nil {
			ex.udpAddr = addr
		}
	}
	for tip := range c.peers {
		if _, ok := keep[tip]; !ok {
			log.Printf("lite: peer gone %s", tip)
			delete(c.peers, tip)
		}
	}
}

func parseUDPEndpoint(s string) *net.UDPAddr {
	if s == "" {
		return nil
	}
	addr, err := net.ResolveUDPAddr("udp4", s)
	if err != nil {
		return nil
	}
	return addr
}

// recvLoop reads UDP. Four sources: camp announce replies (dispatched
// to AnnounceClient.HandlePacket), hole-punch from peers (LastSeen
// updates), signal-frames (0xF2 prefix — forwarded via OnSignal),
// anything else (ignored — no IP tunneling here).
func (c *Client) recvLoop(ctx context.Context) {
	defer c.workers.Done()
	buf := make([]byte, 65535)
	for {
		n, from, err := c.udp.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("lite: udp read: %v", err)
			}
			return
		}
		pkt := buf[:n]
		// Camp reply?
		if ca, _ := c.campUDPAddr.Load().(*net.UDPAddr); ca != nil && sameAddr(ca, from) {
			if c.ann != nil && c.ann.HandlePacket(pkt) {
				continue
			}
		}
		// Identify sender by UDP address. Any packet from a known
		// peer counts as alive (hole-punch ack or signal-frame both).
		c.mu.Lock()
		var fromPeer *peer
		for _, p := range c.peers {
			if sameAddr(p.udpAddr, from) {
				p.lastSeenMs.Store(time.Now().UnixMilli())
				fromPeer = p
				break
			}
		}
		c.mu.Unlock()
		// Signal-frame dispatch happens outside the lock so a slow
		// OnSignal handler doesn't block other peers' updates.
		if fromPeer != nil && n >= 1 && pkt[0] == signalPrefix {
			if cb := c.OnSignal; cb != nil {
				body := make([]byte, n-1)
				copy(body, pkt[1:])
				cb(fromPeer.info.TunnelIP, body)
			}
		}
	}
}

// SendSignal delivers an opaque payload to the peer identified by its
// tunnel_ip. Wrapped in the 0xF2-prefixed framing the recvLoop above
// recognises. Returns an error if the peer is unknown or has no UDP
// endpoint yet (hole-punch still in progress).
func (c *Client) SendSignal(toTunnelIP string, body []byte) error {
	c.mu.Lock()
	p, ok := c.peers[toTunnelIP]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("lite: no peer with tunnel_ip %s", toTunnelIP)
	}
	if p.udpAddr == nil {
		return fmt.Errorf("lite: peer %s has no UDP endpoint yet", toTunnelIP)
	}
	if c.udp == nil {
		return errors.New("lite: not running")
	}
	pkt := make([]byte, 1+len(body))
	pkt[0] = signalPrefix
	copy(pkt[1:], body)
	_, err := c.udp.WriteToUDP(pkt, p.udpAddr)
	return err
}

// holePunchLoop sends 1-byte UDP pings to every known peer. 1Hz burst
// while LastSeen is stale, 25s keepalive once a peer has answered.
// Identical cadence to source/mac engine so behaviour is consistent.
func (c *Client) holePunchLoop(ctx context.Context) {
	defer c.workers.Done()
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.mu.Lock()
		now := time.Now().UnixMilli()
		for _, p := range c.peers {
			if p.udpAddr == nil {
				continue
			}
			seen := p.lastSeenMs.Load()
			fresh := seen != 0 && now-seen < 25000
			if fresh {
				// Keep-alive cadence: send roughly every 25s. Skip
				// most ticks.
				if (now/1000)%25 != 0 {
					continue
				}
			}
			_, _ = c.udp.WriteToUDP([]byte{0xFF}, p.udpAddr)
		}
		c.mu.Unlock()
	}
}

func sameAddr(a, b *net.UDPAddr) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Port == b.Port && a.IP.Equal(b.IP)
}
