// Package bus is the peer-to-peer data bus: a single QUIC transport over
// the AWG overlay that carries typed messages (signalling, certs,
// notifications, chat, …) between peers, replacing the ad-hoc
// HTTP-over-tunnel calls.
//
// Trust model: the AWG overlay (WireGuard) already encrypts and
// authenticates — a peer's overlay IP is cryptographically bound to its
// identity pub, and spoofed source IPs are dropped. So the bus does NOT
// add its own auth: QUIC uses a throwaway self-signed cert with peer
// verification disabled, and a connection's identity is simply the overlay
// IP it came from (resolved back to a pub via the camp roster).
//
// Wire format, per stream: a JSON header frame then a payload frame, each
// length-prefixed (uint32 big-endian). For a request the responder writes
// one response frame back on the same bidi stream; Notify skips the reply.
package bus

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/vseplet/f2f/source/helper/clog"
	"github.com/vseplet/f2f/source/helper/identity"
)

const (
	alpn = "f2f-bus"
	Port = "2203" // UDP port on the overlay IP

	// pingFailLimit is how many consecutive failed liveness pings drop a peer's
	// cached conn. >1 so a single transient stall doesn't tear a conn carrying a
	// live VNC/shell stream; a truly dead conn still fails the streak and goes.
	pingFailLimit = 2
)

// dbg logs verbose connection-lifecycle diagnostics (dial/adopt/forget/drop
// and the cause a connection died with) to trace why links flap. Shown at
// F2F_LOG=debug.
func dbg(format string, args ...any) {
	clog.Debug("bus", format, args...)
}

// Stream is the raw bidirectional stream handed to/returned by the stream
// API (HandleStream / OpenStream). Aliased so callers needn't import
// quic-go directly.
type Stream = quic.Stream

// Resolver maps between a peer's f2f identity pub and its overlay IP, and
// lists the peers we should keep a QUIC connection to.
type Resolver interface {
	AddrForPub(pub string) string // overlay IP, "" if unknown/offline
	PubForIP(ip string) string    // pub for an overlay IP, "" if unknown
	NameForPub(pub string) string // display name for a pub, "" if unknown
	Peers() []string              // pubs of reachable peers (excluding self)
	SelfPub() string              // our own identity pub (for the dial tie-break)
}

// HandlerFunc handles an inbound message of a registered type. fromPub is
// the authenticated sender (by overlay IP). The returned bytes are sent
// back as the response (ignored for Notify); an error is logged and the
// stream is reset.
type HandlerFunc func(fromPub string, payload []byte) ([]byte, error)

// StreamHandlerFunc handles an inbound long-lived stream (vs the one-shot
// request/response of HandlerFunc). open is the initial frame's payload
// (open parameters); st is the raw bidirectional QUIC stream, which the
// handler OWNS for its whole lifetime — it must read/write and Close it
// itself. Used for streaming workloads (PTY/shell, file transfer) where
// request/response framing doesn't fit. fromPub is the authenticated
// sender (by overlay IP).
type StreamHandlerFunc func(fromPub string, open []byte, st *quic.Stream)

// Service owns the QUIC listener and the per-peer outbound connections.
type Service struct {
	// Events, if set, is called when a peer's reachability changes — text is
	// "up" or "down" — so a higher layer (the notification hub) can surface
	// peer presence in the UI. Set once before Start.
	Events func(kind, peerPub, text string)

	resolver  Resolver
	tlsServer *tls.Config
	tlsClient *tls.Config
	quicConf  *quic.Config

	mu             sync.Mutex
	ln             *quic.Listener
	lnConn         net.PacketConn // the UDP socket we own under ln (closed hard on Stop)
	ctx            context.Context // service lifetime; accept loops on dialed conns ride it
	cancel         context.CancelFunc
	running        bool
	conns          map[string]*quic.Conn // pub → outbound connection (reused)
	handlers       map[string]HandlerFunc
	streamHandlers map[string]StreamHandlerFunc
	linkUp         map[string]bool // pub → last ping outcome (for up/down notifications)
	pingFail       map[string]int  // pub → consecutive failed pings; drop the conn only after a streak
	// dialing collapses concurrent dials to the same peer (the shell and
	// vnc discovery probes fire together every 5s): the first caller dials,
	// the rest wait on the channel and reuse the outcome. Without this every
	// probe pair runs two parallel QUIC handshakes to the same address —
	// twice the burn against an unreachable peer.
	dialing map[string]chan struct{}
}

// New builds the service. The self-signed cert is generated once.
func New(r Resolver) (*Service, error) {
	cert, err := selfSignedCert()
	if err != nil {
		return nil, fmt.Errorf("bus: cert: %w", err)
	}
	qc := &quic.Config{MaxIdleTimeout: 90 * time.Second, KeepAlivePeriod: 20 * time.Second}
	s := &Service{
		resolver:       r,
		tlsServer:      &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{alpn}, MinVersion: tls.VersionTLS13},
		tlsClient:      &tls.Config{InsecureSkipVerify: true, NextProtos: []string{alpn}, MinVersion: tls.VersionTLS13}, // overlay already authenticates
		quicConf:       qc,
		conns:          make(map[string]*quic.Conn),
		handlers:       make(map[string]HandlerFunc),
		streamHandlers: make(map[string]StreamHandlerFunc),
		linkUp:         make(map[string]bool),
		pingFail:       make(map[string]int),
		dialing:        make(map[string]chan struct{}),
	}
	// Built-in liveness probe: echo back so the caller can measure RTT.
	s.handlers["ping"] = func(_ string, payload []byte) ([]byte, error) { return payload, nil }
	return s, nil
}

// Handle registers a handler for a message type. Call before/after Start.
func (s *Service) Handle(typ string, fn HandlerFunc) {
	s.mu.Lock()
	s.handlers[typ] = fn
	s.mu.Unlock()
}

// HandleStream registers a stream handler for a message type. An inbound
// stream whose header type matches is handed to fn as a raw bidirectional
// QUIC stream (fn owns its lifetime, including Close), instead of going
// through the one-shot request/response path. Use for streaming workloads.
func (s *Service) HandleStream(typ string, fn StreamHandlerFunc) {
	s.mu.Lock()
	s.streamHandlers[typ] = fn
	s.mu.Unlock()
}

// Peers returns the pubs of reachable peers (from the resolver) — used by
// higher layers (gossip) to fan out to the mesh.
func (s *Service) Peers() []string { return s.resolver.Peers() }

// Label renders a peer pub as the canonical name/fp for logs. Exposed so
// services that only learn a peer by pub over the bus (shell, vnc) name it
// the same way every other component does.
func (s *Service) Label(pub string) string { return s.label(pub) }

// Start brings the bus up: QUIC listener on overlayIP:Port plus the
// auto-mesh ping loop. Idempotent. Never fails permanently — a bind error
// (overlay IP not yet settled during engine bring-up, stale socket) is
// retried in the background, because outbound dials work without a
// listener and a one-shot failure here used to leave the bus dead until
// the next engine restart.
func (s *Service) Start(overlayIP string) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx, s.cancel, s.running = ctx, cancel, true
	s.mu.Unlock()
	go s.listenLoop(ctx, net.JoinHostPort(overlayIP, Port))
	go s.pingLoop(ctx) // auto-mesh: keep a QUIC link to every peer alive
	return nil
}

// listenLoop binds the QUIC listener, retrying every few seconds until it
// sticks, then serves it until the service stops. We create the UDP socket
// ourselves with SO_REUSEADDR (see reuseAddrControl): an engine restart tears
// the utun (and our old socket) down and rebinds the SAME overlay IP, and
// without REUSEADDR the briefly-lingering old socket blocks the new bind and
// this loop spins forever. Because we own the conn, Stop closes it directly —
// hard-releasing the port instead of waiting on QUIC's graceful drain.
func (s *Service) listenLoop(ctx context.Context, addr string) {
	lc := net.ListenConfig{Control: reuseAddrControl}
	for {
		pc, err := lc.ListenPacket(ctx, "udp", addr)
		var ln *quic.Listener
		if err == nil {
			if ln, err = quic.Listen(pc, s.tlsServer, s.quicConf); err != nil {
				_ = pc.Close()
			}
		}
		if err == nil {
			s.mu.Lock()
			if !s.running {
				s.mu.Unlock()
				_ = ln.Close()
				_ = pc.Close()
				return
			}
			s.ln, s.lnConn = ln, pc
			s.mu.Unlock()
			clog.Info("bus", "QUIC listening on %s", addr)
			s.acceptLoop(ctx, ln)
			return
		}
		clog.Warn("bus", "listen %s: %v — retrying in 3s", addr, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// reuseAddrControl sets SO_REUSEADDR on the bus listener socket so it can
// rebind overlayIP:Port immediately after a restart drops the old socket.
// Only REUSEADDR (not REUSEPORT) — two real instances should still conflict,
// which is a useful "already running" signal rather than silent port-sharing.
func reuseAddrControl(network, address string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return serr
}

// pingLoop dials + probes every known peer over QUIC, so the mesh forms
// automatically and the traffic is visible in the logs. The first round
// fires immediately (a bare ticker would leave the mesh unformed for its
// whole first period), a second follows shortly after — the camp roster
// often lands a few seconds into the engine's life — then the steady 30s
// cadence takes over.
func (s *Service) pingLoop(ctx context.Context) {
	s.pingRound(ctx)
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Second):
		s.pingRound(ctx)
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pingRound(ctx)
		}
	}
}

// pingRound probes every currently-known peer once and evicts conns to
// peers that left the roster.
func (s *Service) pingRound(ctx context.Context) {
	peers := s.resolver.Peers()
	known := make(map[string]bool, len(peers))
	for _, pub := range peers {
		known[pub] = true
		go s.pingOne(ctx, pub)
	}
	s.evictStale(known)
}

func (s *Service) pingOne(ctx context.Context, pub string) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := s.Request(rctx, pub, "ping", nil)
	ok := err == nil
	rtt := time.Since(start).Round(time.Millisecond)
	if ok {
		s.mu.Lock()
		s.pingFail[pub] = 0
		s.mu.Unlock()
		clog.Debug("bus", "ping %s ok via QUIC (%s)", s.label(pub), rtt)
	} else {
		// Drop only after a streak — a single missed ping is usually a
		// transient stall, and tearing the conn would also kill any live
		// VNC/shell stream multiplexed on it. A genuinely dead conn fails
		// repeatedly and gets dropped after the streak.
		s.mu.Lock()
		s.pingFail[pub]++
		fails := s.pingFail[pub]
		s.mu.Unlock()
		clog.Warn("bus", "ping %s failed (%d/%d): %v", s.label(pub), fails, pingFailLimit, err)
		if fails >= pingFailLimit {
			s.dropConn(pub, fmt.Sprintf("%d consecutive ping failures", fails))
		}
	}

	// Notify only on a link-state CHANGE, not every ping: "up" when it first
	// connects or recovers, "down" only when a previously-up link drops.
	// A never-reachable peer stays quiet until it actually comes up.
	s.mu.Lock()
	prev, had := s.linkUp[pub]
	s.linkUp[pub] = ok
	s.mu.Unlock()
	switch {
	case ok && (!had || !prev):
		s.emit("peer", pub, "up")
	case !ok && had && prev:
		s.emit("peer", pub, "down")
	}
}

// evictStale closes cached connections to peers that left the roster.
// pingLoop only probes current peers, and QUIC keepalives hold an unused
// connection open indefinitely — without this sweep a departed peer's
// conn (and its buffers) would live for the rest of the process.
func (s *Service) evictStale(known map[string]bool) {
	s.mu.Lock()
	var stale []string
	for pub := range s.conns {
		if !known[pub] {
			stale = append(stale, pub)
		}
	}
	for pub := range s.linkUp {
		if !known[pub] {
			delete(s.linkUp, pub)
		}
	}
	s.mu.Unlock()
	for _, pub := range stale {
		s.dropConn(pub, "peer left roster")
	}
}

func (s *Service) emit(kind, pub, text string) {
	if s.Events != nil {
		s.Events(kind, pub, text)
	}
}

// Stop closes the listener and all connections. Idempotent.
func (s *Service) Stop() error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	s.running = false
	cancel, ln, lnConn, conns := s.cancel, s.ln, s.lnConn, s.conns
	s.cancel, s.ln, s.lnConn, s.conns = nil, nil, nil, make(map[string]*quic.Conn)
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	for _, c := range conns {
		_ = c.CloseWithError(0, "stop")
	}
	if ln != nil {
		_ = ln.Close()
	}
	// Close the socket we own AFTER the listener — frees overlayIP:Port now,
	// not after QUIC's graceful drain, so a follow-up Start can rebind it.
	if lnConn != nil {
		return lnConn.Close()
	}
	return nil
}

func (s *Service) acceptLoop(ctx context.Context, ln *quic.Listener) {
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			return // listener closed
		}
		go s.serveConn(ctx, conn)
	}
}

func (s *Service) serveConn(ctx context.Context, conn *quic.Conn) {
	// Identity = the overlay IP we received the connection from (AWG-attested).
	ip, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	fromPub := s.resolver.PubForIP(ip)
	clog.Debug("bus", "inbound QUIC from %s", s.labelOr(fromPub, ip))
	// Reuse this inbound connection for our own outbound sends too — QUIC
	// streams are bidirectional, so one connection per pair serves both ways.
	if fromPub != "" {
		s.adoptConn(fromPub, conn)
		defer s.forgetConn(fromPub, conn)
	}
	s.acceptStreams(ctx, ip, fromPub, conn)
}

// acceptStreams runs a connection's inbound-stream loop, dispatching each
// stream to its handler. Run for BOTH directions of a connection — the side
// that accepted it (serveConn) AND the side that dialed it (dial) — because a
// QUIC connection only carries streams in a direction if that end calls
// AcceptStream. Without running it on dialed conns, the peer (which reuses the
// same conn to send back to us) opens streams we'd never accept, so our
// requests to it would silently time out. Returns when the conn dies.
func (s *Service) acceptStreams(ctx context.Context, ip, fromPub string, conn *quic.Conn) {
	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			// Why did this conn die? context.Cause exposes the QUIC reason
			// (idle timeout vs peer CONNECTION_CLOSE vs path error) — the signal
			// we need to tell a real fault from our own redial.
			dbg("conn %s closed: accept=%v cause=%v",
				s.labelOr(fromPub, ip), err, context.Cause(conn.Context()))
			return
		}
		go s.serveStream(fromPub, stream)
	}
}

// adoptConn caches a connection for pub unless one is already cached.
func (s *Service) adoptConn(pub string, conn *quic.Conn) {
	s.mu.Lock()
	adopted := s.conns[pub] == nil
	if adopted {
		s.conns[pub] = conn
	}
	s.mu.Unlock()
	if adopted {
		clog.Info("bus", "conn UP %s (inbound)", s.label(pub))
	}
}

// forgetConn drops conn from the cache if it's still the cached one. This is
// the path a connection takes when it DIES ON ITS OWN (accept loop returned) —
// QUIC idle-timeout, the peer sending CONNECTION_CLOSE, or a path error — as
// opposed to dropConn, where WE tear it. Log the QUIC-level cause so the log
// distinguishes "the link broke under us" from "we redialed it".
func (s *Service) forgetConn(pub string, conn *quic.Conn) {
	s.mu.Lock()
	removed := s.conns[pub] == conn
	if removed {
		delete(s.conns, pub)
	}
	s.mu.Unlock()
	if removed {
		clog.Info("bus", "conn DOWN %s: died (quic cause=%v)", s.label(pub), context.Cause(conn.Context()))
	}
}

func (s *Service) serveStream(fromPub string, st *quic.Stream) {
	hdr, payload, err := readFrame(st)
	if err != nil {
		st.Close()
		return
	}
	s.mu.Lock()
	sfn := s.streamHandlers[hdr.Type]
	fn := s.handlers[hdr.Type]
	s.mu.Unlock()
	// Stream handler: hand off the raw stream — it owns the lifetime (incl.
	// Close), so we must NOT close it here.
	if sfn != nil {
		sfn(fromPub, payload, st)
		return
	}
	defer st.Close()
	if fn == nil {
		clog.Warn("bus", "no handler for type %q from %s", hdr.Type, s.label(fromPub))
		return
	}
	resp, err := fn(fromPub, payload)
	if hdr.Notify {
		return
	}
	if err != nil {
		st.CancelWrite(1) // signal the requester that the handler failed
		return
	}
	_ = writeChunk(st, resp)
}

// OpenStream opens a long-lived bidirectional stream to pub, writes the
// open frame (type + open params), and returns the raw QUIC stream for the
// caller to read/write directly. The caller OWNS the stream and must Close
// it. Counterpart of HandleStream. Use for PTY/file-transfer style traffic
// that doesn't fit Request/Notify.
func (s *Service) OpenStream(ctx context.Context, pub, typ string, open []byte) (*quic.Stream, error) {
	st, err := s.openStream(ctx, pub, false)
	if err != nil {
		return nil, err
	}
	if err := writeFrame(st, header{Type: typ}, open); err != nil {
		st.Close()
		s.dropIfStalled(pub, err)
		return nil, err
	}
	return st, nil
}

// Request opens a stream to pub, sends a typed message and waits for the
// response. Retries once on a stale cached connection.
func (s *Service) Request(ctx context.Context, pub, typ string, payload []byte) ([]byte, error) {
	st, err := s.openStream(ctx, pub, false)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	// Bound the WHOLE exchange by ctx — crucially the response read. Without a
	// stream deadline, readChunk blocks on a half-dead conn until the QUIC
	// idle-timeout (tens of seconds), not the caller's ctx, which hangs callers
	// (e.g. the 2s discovery poll) and lets requests pile up on a dying conn.
	if dl, ok := ctx.Deadline(); ok {
		_ = st.SetDeadline(dl)
	}
	if err := writeFrame(st, header{Type: typ}, payload); err != nil {
		s.dropIfStalled(pub, err)
		return nil, err
	}
	resp, err := readChunk(st)
	if err != nil {
		s.dropIfStalled(pub, err)
		return nil, err
	}
	return resp, nil
}

// dropIfStalled evicts the cached conn for pub when err is a deadline-type
// failure. A conn that eats a whole request deadline is a zombie: QUIC may
// keep it alive indefinitely (keepalive/ACK packets flow, stream data
// doesn't — half-dead path, or a peer that never accepts streams), so the
// idle timeout never fires and only the 30s ping sweep would clean it up —
// leaving every request in between to fail. Evict now; the next call
// redials. A stream reset (handler failure on a healthy conn) is NOT a
// stall and keeps the conn.
func (s *Service) dropIfStalled(pub string, err error) {
	var ne net.Error
	if errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &ne) && ne.Timeout()) {
		// Don't tear the connection — that also kills any live VNC/shell stream
		// multiplexed on it. The request's own stream is already closed (its
		// deadline bounded it); a genuinely dead conn is reaped by the ping
		// streak (pingFailLimit) instead.
		dbg("%s: request stalled (stream closed; conn kept)", s.label(pub))
	}
}

// Notify sends a fire-and-forget typed message to pub (no response).
func (s *Service) Notify(pub, typ string, payload []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := s.openStream(ctx, pub, true)
	if err != nil {
		return err
	}
	defer st.Close()
	return writeFrame(st, header{Type: typ, Notify: true}, payload)
}

func (s *Service) openStream(ctx context.Context, pub string, notify bool) (*quic.Stream, error) {
	conn, err := s.connFor(ctx, pub)
	if err != nil {
		return nil, err
	}
	st, err := conn.OpenStreamSync(ctx)
	if err != nil {
		// likely a stale connection — drop and dial fresh once.
		s.dropConn(pub, fmt.Sprintf("OpenStream failed on cached conn: %v", err))
		conn, err = s.connFor(ctx, pub)
		if err != nil {
			return nil, err
		}
		st, err = conn.OpenStreamSync(ctx)
		if err != nil {
			return nil, err
		}
	}
	return st, nil
}

func (s *Service) connFor(ctx context.Context, pub string) (*quic.Conn, error) {
	if c := s.cached(pub); c != nil {
		return c, nil
	}
	// Tie-break so a pair forms ONE connection: the lower pub dials, the
	// higher pub waits for the inbound. The waiter still falls back to
	// dialing (so it never gets stuck if the peer never dials). The wait
	// must never eat the caller's whole budget: a 2s discovery probe that
	// waits 2s reaches the fallback dial with an already-expired ctx and
	// fails in ~1ms every time — so cap the wait at half the remaining
	// deadline.
	if my := s.resolver.SelfPub(); my != "" && my > pub {
		wait := 2 * time.Second
		if dl, ok := ctx.Deadline(); ok {
			if half := time.Until(dl) / 2; half < wait {
				wait = half
			}
		}
		if wait > 0 {
			if c := s.waitInbound(ctx, pub, wait); c != nil {
				return c, nil
			}
		}
	}
	return s.dial(ctx, pub)
}

// cached returns the connection for pub if one exists (inbound or outbound).
func (s *Service) cached(pub string) *quic.Conn {
	s.mu.Lock()
	c := s.conns[pub]
	s.mu.Unlock()
	return c
}

// waitInbound polls for an inbound connection from pub to appear, up to d.
func (s *Service) waitInbound(ctx context.Context, pub string, d time.Duration) *quic.Conn {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c := s.cached(pub); c != nil {
			return c
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(50 * time.Millisecond):
		}
	}
	return s.cached(pub)
}

// dial returns a connection to pub, deduplicating concurrent attempts:
// exactly one caller runs the QUIC handshake, the rest wait for its outcome
// and pick the conn up from the cache. A waiter whose winner failed retries
// as the dialer itself (bounded by its own ctx).
func (s *Service) dial(ctx context.Context, pub string) (*quic.Conn, error) {
	for {
		s.mu.Lock()
		if c := s.conns[pub]; c != nil {
			s.mu.Unlock()
			return c, nil
		}
		ch, busy := s.dialing[pub]
		if !busy {
			ch = make(chan struct{})
			s.dialing[pub] = ch
		}
		s.mu.Unlock()
		if busy {
			select {
			case <-ch: // winner finished — re-check the cache, or dial ourselves
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		conn, err := s.dialOnce(ctx, pub)
		s.mu.Lock()
		delete(s.dialing, pub)
		s.mu.Unlock()
		close(ch)
		return conn, err
	}
}

// dialOnce performs one actual QUIC dial to pub's overlay address.
func (s *Service) dialOnce(ctx context.Context, pub string) (*quic.Conn, error) {
	ip := s.resolver.AddrForPub(pub)
	if ip == "" {
		return nil, fmt.Errorf("bus: no overlay ip for %s", s.label(pub))
	}
	lbl := s.label(pub)
	dbg("dial %s …", lbl)
	start := time.Now()
	conn, err := quic.DialAddr(ctx, net.JoinHostPort(ip, Port), s.tlsClient, s.quicConf)
	if err != nil {
		dbg("dial %s failed after %s: %v", lbl, time.Since(start).Round(time.Millisecond), err)
		// No identity in the wrapped error — the caller (ca-poll, etc.)
		// already labels the peer, so repeating it just doubles up.
		return nil, fmt.Errorf("bus: dial: %w", err)
	}
	s.mu.Lock()
	if existing := s.conns[pub]; existing != nil { // lost a race; keep the first
		s.mu.Unlock()
		_ = conn.CloseWithError(0, "dup")
		return existing, nil
	}
	s.conns[pub] = conn
	sctx := s.ctx
	s.mu.Unlock()
	clog.Info("bus", "conn UP %s (dialed in %s)", lbl, time.Since(start).Round(time.Millisecond))
	// A dialed conn is bidirectional too: the peer (which ACCEPTED it) reuses it
	// to open streams back to us. Only the accepting end runs an accept loop, so
	// without this our dialed conns would silently never deliver the peer's
	// streams and its requests to us would time out. Serve them on the service
	// lifetime (not the dial ctx, which is request-scoped), and forget the conn
	// when it dies so the next call re-dials.
	if sctx == nil {
		sctx = context.Background()
	}
	go func() {
		defer s.forgetConn(pub, conn)
		s.acceptStreams(sctx, ip, pub, conn)
	}()
	return conn, nil
}

// dropConn tears down the cached conn for pub because WE decided to (ping
// streak, stale stream-open, peer left roster). reason says which — paired with
// the QUIC-level cause it tells whether the conn was already broken or we
// killed a healthy one. Counterpart of forgetConn (conn died on its own).
func (s *Service) dropConn(pub, reason string) {
	s.mu.Lock()
	c := s.conns[pub]
	delete(s.conns, pub)
	delete(s.pingFail, pub)
	s.mu.Unlock()
	if c != nil {
		clog.Info("bus", "conn DROP %s: %s (quic cause=%v)", s.label(pub), reason, context.Cause(c.Context()))
		_ = c.CloseWithError(0, "redial")
	}
}

// --- wire framing ---

type header struct {
	Type   string `json:"type"`
	Notify bool   `json:"notify,omitempty"`
}

func writeFrame(w io.Writer, h header, payload []byte) error {
	hb, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if err := writeChunk(w, hb); err != nil {
		return err
	}
	return writeChunk(w, payload)
}

func readFrame(r io.Reader) (header, []byte, error) {
	hb, err := readChunk(r)
	if err != nil {
		return header{}, nil, err
	}
	var h header
	if err := json.Unmarshal(hb, &h); err != nil {
		return header{}, nil, err
	}
	payload, err := readChunk(r)
	return h, payload, err
}

func writeChunk(w io.Writer, b []byte) error {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	if _, err := w.Write(n[:]); err != nil {
		return err
	}
	if len(b) > 0 {
		_, err := w.Write(b)
		return err
	}
	return nil
}

func readChunk(r io.Reader) ([]byte, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	sz := binary.BigEndian.Uint32(n[:])
	if sz > 16<<20 {
		return nil, errors.New("bus: frame too large")
	}
	if sz == 0 {
		return nil, nil
	}
	b := make([]byte, sz)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

// --- self-signed cert (overlay handles real auth) ---

func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "f2f-bus"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// label renders a peer for logs as the canonical name/fp — the ONE form
// used everywhere a bus line names a peer. The name comes from the
// resolver (the engine roster); when it's unknown we fall back to the
// bare fingerprint. Never log a raw pub or overlay IP for identity.
func (s *Service) label(pub string) string {
	return identity.Label(s.resolver.NameForPub(pub), pub)
}

// labelOr is label, but for an inbound conn whose pub we couldn't resolve
// (peer not in the roster yet) it falls back to the raw overlay IP — the
// only identity we have in that one case. Everywhere the pub is known,
// it's the same name/fp as label.
func (s *Service) labelOr(pub, ip string) string {
	if l := s.label(pub); l != "" {
		return l
	}
	return ip
}
