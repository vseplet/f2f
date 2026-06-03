package rendezvous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sync/atomic"
	"time"
)

// AnnounceClient is the UDP rendezvous client. It piggybacks on the
// engine's main tunnel UDP socket: sending an announce packet to camp
// (a) registers/refreshes our peer entry, (b) lets camp observe our
// public endpoint via the packet's source address (replaces STUN), and
// (c) keeps the NAT mapping for our tunnel port alive on the
// camp-facing path (which matters under endpoint-dependent NATs where
// the peer-facing mapping doesn't help the camp-facing one).
type AnnounceClient struct {
	conn     *net.UDPConn
	campAddr *net.UDPAddr // resolved camp UDP endpoint
	name     string
	campID   string
	pub      string // local ed25519 pubkey in hex, "" in static --peer mode

	self atomic.Pointer[PeerInfo] // latest announced reply

	// Liveness counters for the UI's camp-health section. lastSentMs is
	// stamped right before WriteToUDP; lastReplyMs is stamped when a
	// recognised reply arrives via HandlePacket. RTT = reply-time minus
	// the most recent send-time at parse time (best-effort: with our
	// 20s announce cadence multiple sends almost never overlap).
	lastSentMs  atomic.Int64
	lastReplyMs atomic.Int64
	lastRTTMs   atomic.Int64
}

// NewAnnounceClient resolves campAddrStr and prepares the client. The
// underlying UDP socket is shared — no exclusive ownership beyond the
// brief AnnounceOnce bootstrap. pub is the local ed25519 pubkey in hex,
// empty if not available.
func NewAnnounceClient(conn *net.UDPConn, campAddrStr, name, campID, pub string) (*AnnounceClient, error) {
	addr, err := net.ResolveUDPAddr("udp4", campAddrStr)
	if err != nil {
		return nil, fmt.Errorf("resolve camp addr %q: %w", campAddrStr, err)
	}
	return &AnnounceClient{
		conn:     conn,
		campAddr: addr,
		name:     name,
		campID:   campID,
		pub:      pub,
	}, nil
}

// CampAddr returns the resolved camp UDP endpoint. The engine's read
// loop uses this to identify incoming packets that belong to us.
func (a *AnnounceClient) CampAddr() *net.UDPAddr { return a.campAddr }

// Self returns the latest PeerInfo camp gave us, or nil if we haven't
// received any reply yet.
func (a *AnnounceClient) Self() *PeerInfo { return a.self.Load() }

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
			info, perr, isAnnounceReply := parseAnnounceReply(buf[:n])
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
		log.Printf("camp: announce: %v", err)
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.sendAnnounce(); err != nil {
				log.Printf("camp: announce: %v", err)
			}
		}
	}
}

// HandlePacket is called by the engine's UDP read loop for any packet
// whose source matches CampAddr(). Returns true if the packet is an
// announce-protocol message (so the loop should not treat it as tunnel
// data); false to fall through.
func (a *AnnounceClient) HandlePacket(pkt []byte) bool {
	info, perr, isAnnounceReply := parseAnnounceReply(pkt)
	if !isAnnounceReply {
		return false
	}
	if perr != nil {
		log.Printf("camp: announce reply: %v", perr)
		return true
	}
	now := time.Now().UnixMilli()
	a.lastReplyMs.Store(now)
	if sent := a.lastSentMs.Load(); sent > 0 && now >= sent {
		a.lastRTTMs.Store(now - sent)
	}
	a.self.Store(&info)
	return true
}

func parseAnnounceReply(pkt []byte) (info PeerInfo, perr error, isAnnounceReply bool) {
	var head struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal(pkt, &head); err != nil {
		return PeerInfo{}, nil, false
	}
	switch head.T {
	case "announced":
		var msg struct {
			T   string   `json:"t"`
			You PeerInfo `json:"you"`
		}
		if err := json.Unmarshal(pkt, &msg); err != nil {
			return PeerInfo{}, fmt.Errorf("decode announced: %w", err), true
		}
		return msg.You, nil, true
	case "error":
		var msg struct {
			T       string `json:"t"`
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(pkt, &msg)
		return PeerInfo{}, fmt.Errorf("camp: %s: %s", msg.Code, msg.Message), true
	}
	return PeerInfo{}, nil, false
}

func (a *AnnounceClient) sendAnnounce() error {
	data, err := json.Marshal(AnnounceReq{
		T:      "announce",
		Name:   a.name,
		CampID: a.campID,
		Pub:    a.pub,
	})
	if err != nil {
		return err
	}
	a.lastSentMs.Store(time.Now().UnixMilli())
	if _, err := a.conn.WriteToUDP(data, a.campAddr); err != nil {
		return fmt.Errorf("send announce: %w", err)
	}
	return nil
}
