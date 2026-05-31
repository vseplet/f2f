//go:build darwin

// Package peerping implements a bidirectional UDP ping/pong with peers.
//
// Why this exists: the engine's existing peerState.LastSeenMs only tells
// us "this peer is sending us packets". It does NOT tell us whether our
// packets are reaching the peer. Under asymmetric NAT failures (or a
// half-broken local UDP socket) we can have one direction silently dead
// while the other works — invisible from any one-way signal.
//
// Wire protocol (JSON, same socket as everything else):
//
//	A → B: {"t":"f2f-ping","id":"<16 hex>","ts":<unix_ms>}
//	B → A: {"t":"f2f-pong","id":"<same>","ts":<same>}
//
// The responder is fully stateless: parse → rename "ping" to "pong" →
// send back. RTT is computed by the initiator on pong receipt as
// now - ts. The id field is there to drop late pongs after timeout.
package peerping

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// Target is one peer to ping. Key is the peer's tunnel_ip — stable
// across NAT rebinds; UDPAddr can shift, the tunnel_ip identifies who.
type Target struct {
	Key  string
	Addr *net.UDPAddr
}

// Result is the latest per-peer reachability info.
type Result struct {
	LastPongMs int64 // wall-clock epoch ms; 0 = never
	LastRTTMs  int64 // most recent round-trip
}

// Pinger sends periodic pings to the current peer set and tracks
// pong responses. It is safe to use from one Run goroutine plus
// concurrent HandlePacket callers (typically one per UDP read loop).
type Pinger struct {
	conn     *net.UDPConn
	targets  func() []Target
	interval time.Duration // between ping rounds
	timeout  time.Duration // pending ping older than this is dropped

	mu      sync.Mutex
	pending map[string]pendingPing // id → who/when
	results map[string]Result      // tunnel_ip → result

	// state tracks per-peer log transitions so we only log on change
	// (first pong, silent → recovered, healthy → silent) instead of
	// spamming every tick.
	state map[string]peerLog
}

type pendingPing struct {
	key    string
	sentMs int64
}

type peerLog struct {
	sawPong bool // ever
	healthy bool // last check passed
}

// New constructs a Pinger. conn is the shared engine UDP socket;
// targets is a callback the engine implements to expose the current
// peer set on every tick.
func New(conn *net.UDPConn, targets func() []Target) *Pinger {
	return &Pinger{
		conn:     conn,
		targets:  targets,
		interval: 10 * time.Second,
		timeout:  30 * time.Second,
		pending:  map[string]pendingPing{},
		results:  map[string]Result{},
		state:    map[string]peerLog{},
	}
}

// Run blocks until ctx is done. Each tick sends one ping per known
// peer; in parallel a slower ticker emits a per-peer summary so the
// healthy state is visible in logs even without transitions.
func (p *Pinger) Run(ctx context.Context) {
	log.Printf("peerping: started (interval=%s, timeout=%s)", p.interval, p.timeout)
	sendT := time.NewTicker(p.interval)
	defer sendT.Stop()
	reportT := time.NewTicker(30 * time.Second)
	defer reportT.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sendT.C:
			p.tickSend()
		case <-reportT.C:
			p.tickReport()
		}
	}
}

// HandlePacket is dispatched from engine.peerToTunLoop for every UDP
// packet read off the socket. Returns true if the packet is a ping or
// pong (so the caller drops it from the tunnel data path).
func (p *Pinger) HandlePacket(pkt []byte, from *net.UDPAddr) bool {
	// Cheap triage: f2f-ping/pong start with '{' and are well under
	// any reasonable bound. Bail before json.Unmarshal on anything
	// that obviously isn't ours.
	if len(pkt) < 10 || len(pkt) > 256 || pkt[0] != '{' {
		return false
	}
	var msg wireMsg
	if err := json.Unmarshal(pkt, &msg); err != nil {
		return false
	}
	switch msg.T {
	case "f2f-ping":
		p.handlePing(msg, from)
		return true
	case "f2f-pong":
		p.handlePong(msg)
		return true
	}
	return false
}

// Result returns the latest result for a peer (by tunnel_ip key), or
// (zero, false) if we've never had a pong from them.
func (p *Pinger) Result(key string) (Result, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.results[key]
	return r, ok
}

// All returns a snapshot of all results, keyed by tunnel_ip.
func (p *Pinger) All() map[string]Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]Result, len(p.results))
	for k, v := range p.results {
		out[k] = v
	}
	return out
}

type wireMsg struct {
	T  string `json:"t"`
	ID string `json:"id"`
	TS int64  `json:"ts"`
}

func (p *Pinger) handlePing(msg wireMsg, from *net.UDPAddr) {
	// Stateless echo: same id, same ts, just rename t.
	reply, err := json.Marshal(wireMsg{T: "f2f-pong", ID: msg.ID, TS: msg.TS})
	if err != nil {
		return
	}
	_, _ = p.conn.WriteToUDP(reply, from)
}

func (p *Pinger) handlePong(msg wireMsg) {
	now := time.Now().UnixMilli()
	rtt := now - msg.TS
	if rtt < 0 {
		rtt = 0
	}

	p.mu.Lock()
	pp, ok := p.pending[msg.ID]
	if ok {
		delete(p.pending, msg.ID)
	}
	if !ok {
		// Late or unknown id — peer's ping reply we didn't initiate,
		// or a pong after timeout pruned it.
		p.mu.Unlock()
		return
	}
	p.results[pp.key] = Result{LastPongMs: now, LastRTTMs: rtt}
	st := p.state[pp.key]
	wasFirst := !st.sawPong
	wasUnhealthy := st.sawPong && !st.healthy
	st.sawPong = true
	st.healthy = true
	p.state[pp.key] = st
	p.mu.Unlock()

	switch {
	case wasFirst:
		log.Printf("peerping: %s verified (first pong, rtt=%dms)", pp.key, rtt)
	case wasUnhealthy:
		log.Printf("peerping: %s recovered (rtt=%dms)", pp.key, rtt)
	}
}

func (p *Pinger) tickSend() {
	targets := p.targets()
	now := time.Now().UnixMilli()
	timeoutMs := p.timeout.Milliseconds()

	// Prune timed-out pending pings before we add new ones. Any peer
	// that loses its pending entry without a pong is implicitly counted
	// as "missed this round" in checkSilentLocked below.
	p.mu.Lock()
	for id, pp := range p.pending {
		if now-pp.sentMs > timeoutMs {
			delete(p.pending, id)
		}
	}
	p.mu.Unlock()

	if len(targets) == 0 {
		return
	}

	for _, t := range targets {
		if t.Addr == nil {
			continue
		}
		id := randomID()
		raw, err := json.Marshal(wireMsg{T: "f2f-ping", ID: id, TS: now})
		if err != nil {
			continue
		}
		p.mu.Lock()
		p.pending[id] = pendingPing{key: t.Key, sentMs: now}
		p.mu.Unlock()
		if _, err := p.conn.WriteToUDP(raw, t.Addr); err != nil {
			// Drop our own pending — the engine's other write sites
			// already surface socket-level failures; no need to double-log.
			p.mu.Lock()
			delete(p.pending, id)
			p.mu.Unlock()
		}
	}

	p.checkSilent(now)
}

// checkSilent walks known peers and flips state from healthy → silent
// when no pong has arrived within 2× timeout. The 2× is deliberate
// (slack for one lost ping at our interval).
func (p *Pinger) checkSilent(now int64) {
	silentMs := p.timeout.Milliseconds() * 2
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, st := range p.state {
		if !st.sawPong || !st.healthy {
			continue
		}
		r, ok := p.results[key]
		if !ok {
			continue
		}
		age := now - r.LastPongMs
		if age > silentMs {
			st.healthy = false
			p.state[key] = st
			log.Printf("peerping: %s silent (last pong %ds ago)", key, age/1000)
		}
	}
}

func (p *Pinger) tickReport() {
	p.mu.Lock()
	if len(p.results) == 0 {
		p.mu.Unlock()
		return
	}
	now := time.Now().UnixMilli()
	keys := make([]string, 0, len(p.results))
	for k := range p.results {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		r := p.results[k]
		age := (now - r.LastPongMs) / 1000
		mark := "ok"
		if st, ok := p.state[k]; ok && !st.healthy {
			mark = "silent"
		}
		parts = append(parts, fmt.Sprintf("%s=%dms/age=%ds/%s", k, r.LastRTTMs, age, mark))
	}
	p.mu.Unlock()
	log.Printf("peerping: %s", strings.Join(parts, " "))
}

func randomID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
