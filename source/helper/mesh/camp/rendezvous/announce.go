package rendezvous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/vseplet/f2f/source/helper/clog"
)

// AnnounceClient is the UDP rendezvous client. It piggybacks on the
// engine's main tunnel UDP socket: sending an announce packet to camp
// (a) registers/refreshes our peer entry, (b) lets camp observe our
// public endpoint via the packet's source address (replaces STUN), and
// (c) keeps the NAT mapping for our tunnel port alive on the
// camp-facing path (which matters under endpoint-dependent NATs where
// the peer-facing mapping doesn't help the camp-facing one).
type AnnounceClient struct {
	conn          *net.UDPConn
	campAddrStr   string                      // unresolved "host:port"; re-resolved on send
	campAddr      atomic.Pointer[net.UDPAddr] // latest resolved endpoint (nil until resolved)
	lastResolveMs atomic.Int64                // throttles re-resolution
	name          atomic.Pointer[string]      // display name; SetName updates it live (re-announced next tick)
	campID        string
	pub           string // local ed25519 pubkey in hex, "" in static --peer mode

	self atomic.Pointer[PeerInfo] // latest announced reply

	// onPeers, if set, is called with the roster carried in each
	// announce reply. Set once before Run (the announce-delivered peer
	// list replaces the HTTP poll).
	onPeers func([]PeerInfo)

	// Liveness counters for the UI's camp-health section. lastSentMs is
	// stamped right before WriteToUDP; lastReplyMs is stamped when a
	// recognised reply arrives via HandlePacket. RTT = reply-time minus
	// the most recent send-time at parse time (best-effort: with our
	// 20s announce cadence multiple sends almost never overlap).
	lastSentMs  atomic.Int64
	lastReplyMs atomic.Int64
	lastRTTMs   atomic.Int64
}

// NewAnnounceClient prepares the client. The camp address is resolved
// lazily (best-effort here, then re-resolved on every send) so a DNS
// outage at startup — e.g. the machine just woke and the network isn't
// up yet — self-heals instead of permanently failing. pub is the local
// ed25519 pubkey in hex, empty if not available.
func NewAnnounceClient(conn *net.UDPConn, campAddrStr, name, campID, pub string) (*AnnounceClient, error) {
	a := &AnnounceClient{
		conn:        conn,
		campAddrStr: campAddrStr,
		campID:      campID,
		pub:         pub,
	}
	a.name.Store(&name)
	a.resolve() // best-effort; sendAnnounce keeps retrying if DNS isn't ready
	return a, nil
}

// resolve re-resolves the camp address. Throttled to ~30s unless we have
// nothing yet, so DNS hiccups self-heal and fly.io IP changes are picked
// up. The previous good address is kept on failure.
func (a *AnnounceClient) resolve() {
	cur := a.campAddr.Load()
	now := time.Now().UnixMilli()
	if cur != nil && now-a.lastResolveMs.Load() < 30_000 {
		return
	}
	a.lastResolveMs.Store(now)
	addr, err := net.ResolveUDPAddr("udp4", a.campAddrStr)
	if err != nil {
		if cur == nil {
			clog.Warn("camp", "resolve %q failed (%v) — will retry", a.campAddrStr, err)
		}
		return
	}
	if cur == nil || cur.String() != addr.String() {
		clog.Debug("camp", "resolved %q → %s", a.campAddrStr, addr)
	}
	a.campAddr.Store(addr)
}

// CampAddr returns the latest resolved camp UDP endpoint, or nil if it
// hasn't resolved yet. The engine's read loop uses this to identify
// incoming packets that belong to us, so it must read it dynamically.
func (a *AnnounceClient) CampAddr() *net.UDPAddr { return a.campAddr.Load() }

// Self returns the latest PeerInfo camp gave us, or nil if we haven't
// received any reply yet.
func (a *AnnounceClient) Self() *PeerInfo { return a.self.Load() }

// SetName changes the display name announced from the next tick onward.
func (a *AnnounceClient) SetName(name string) { a.name.Store(&name) }

// OnPeers registers the callback invoked with the roster carried in
// announce replies. Set once before Run.
func (a *AnnounceClient) OnPeers(fn func([]PeerInfo)) { a.onPeers = fn }

// LastSentMs / LastReplyMs / LastRTTMs report the UDP-side liveness
// timestamps. Zero means "never". Read concurrently from the UI.
func (a *AnnounceClient) LastSentMs() int64  { return a.lastSentMs.Load() }
func (a *AnnounceClient) LastReplyMs() int64 { return a.lastReplyMs.Load() }
func (a *AnnounceClient) LastRTTMs() int64   { return a.lastRTTMs.Load() }

// AnnounceOnce sends an announce and synchronously reads from conn
// until a matching reply arrives or the timeout expires. Used at
// startup, while we have exclusive access to the socket (the main
// peerToTunLoop hasn't started yet).
func (a *AnnounceClient) AnnounceOnce(timeout time.Duration) (PeerInfo, error) {
	deadline := time.Now().Add(timeout)
	if err := a.conn.SetReadDeadline(deadline); err != nil {
		return PeerInfo{}, fmt.Errorf("set deadline: %w", err)
	}
	defer func() { _ = a.conn.SetReadDeadline(time.Time{}) }()

	backoff := 300 * time.Millisecond
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		if err := a.sendAnnounce(); err != nil {
			return PeerInfo{}, err
		}
		// Wait up to `backoff`, then resend.
		next := time.Now().Add(backoff)
		if next.After(deadline) {
			next = deadline
		}
		_ = a.conn.SetReadDeadline(next)

	read:
		for {
			n, _, err := a.conn.ReadFromUDP(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break read
				}
				return PeerInfo{}, fmt.Errorf("read: %w", err)
			}
			info, _, perr, isAnnounceReply := parseAnnounceReply(buf[:n])
			if !isAnnounceReply {
				continue // some other UDP noise; keep reading until deadline
			}
			if perr != nil {
				return PeerInfo{}, perr
			}
			now := time.Now().UnixMilli()
			a.lastReplyMs.Store(now)
			if sent := a.lastSentMs.Load(); sent > 0 && now >= sent {
				a.lastRTTMs.Store(now - sent)
			}
			a.self.Store(&info)
			return info, nil
		}
		backoff *= 2
		if backoff > time.Second {
			backoff = time.Second
		}
	}
	return PeerInfo{}, errors.New("camp: no announce reply within timeout")
}

// Run sends an immediate announce, then continues every `every`
// until ctx is done. Reply packets are handled by HandlePacket via
// the engine's main read loop (or the camp service's UDP dispatch
// handler) — Run never reads from the socket itself.
func (a *AnnounceClient) Run(ctx context.Context, every time.Duration) {
	if err := a.sendAnnounce(); err != nil {
		clog.Warn("camp", "announce: %v", err)
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.sendAnnounce(); err != nil {
				clog.Warn("camp", "announce: %v", err)
			}
		}
	}
}

// HandlePacket is called by the engine's UDP read loop for any packet
// whose source matches CampAddr(). Returns true if the packet is an
// announce-protocol message (so the loop should not treat it as tunnel
// data); false to fall through.
func (a *AnnounceClient) HandlePacket(pkt []byte) bool {
	info, peers, perr, isAnnounceReply := parseAnnounceReply(pkt)
	if !isAnnounceReply {
		return false
	}
	if perr != nil {
		clog.Warn("camp", "announce reply: %v", perr)
		return true
	}
	now := time.Now().UnixMilli()
	a.lastReplyMs.Store(now)
	if sent := a.lastSentMs.Load(); sent > 0 && now >= sent {
		a.lastRTTMs.Store(now - sent)
	}
	a.self.Store(&info)
	if len(peers) > 0 && a.onPeers != nil {
		a.onPeers(peers)
	}
	return true
}

func parseAnnounceReply(pkt []byte) (info PeerInfo, peers []PeerInfo, perr error, isAnnounceReply bool) {
	var head struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal(pkt, &head); err != nil {
		return PeerInfo{}, nil, nil, false
	}
	switch head.T {
	case "announced":
		var msg struct {
			T     string     `json:"t"`
			You   PeerInfo   `json:"you"`
			Peers []PeerInfo `json:"peers"`
		}
		if err := json.Unmarshal(pkt, &msg); err != nil {
			return PeerInfo{}, nil, fmt.Errorf("decode announced: %w", err), true
		}
		return msg.You, msg.Peers, nil, true
	case "error":
		var msg struct {
			T       string `json:"t"`
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(pkt, &msg)
		return PeerInfo{}, nil, fmt.Errorf("camp: %s: %s", msg.Code, msg.Message), true
	}
	return PeerInfo{}, nil, nil, false
}

func (a *AnnounceClient) sendAnnounce() error {
	a.resolve()
	addr := a.campAddr.Load()
	if addr == nil {
		return fmt.Errorf("camp addr %q unresolved", a.campAddrStr)
	}
	name := ""
	if p := a.name.Load(); p != nil {
		name = *p
	}
	data, err := json.Marshal(AnnounceReq{
		T:      "announce",
		Name:   name,
		CampID: a.campID,
		Pub:    a.pub,
	})
	if err != nil {
		return err
	}
	a.lastSentMs.Store(time.Now().UnixMilli())
	if _, err := a.conn.WriteToUDP(data, addr); err != nil {
		return fmt.Errorf("send announce: %w", err)
	}
	return nil
}
