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
	"log"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	alpn = "f2f-bus"
	Port = "2203" // UDP port on the overlay IP
)

// Stream is the raw bidirectional stream handed to/returned by the stream
// API (HandleStream / OpenStream). Aliased so callers needn't import
// quic-go directly.
type Stream = quic.Stream

// Resolver maps between a peer's f2f identity pub and its overlay IP, and
// lists the peers we should keep a QUIC connection to.
type Resolver interface {
	AddrForPub(pub string) string // overlay IP, "" if unknown/offline
	PubForIP(ip string) string    // pub for an overlay IP, "" if unknown
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
	// Events, if set, is called for notable bus activity (ping results,
	// inbound connects) so a higher layer (the notification hub) can surface
	// it in the UI. Set once before Start.
	Events func(kind, peerPub, text string)

	resolver  Resolver
	tlsServer *tls.Config
	tlsClient *tls.Config
	quicConf  *quic.Config

	mu             sync.Mutex
	ln             *quic.Listener
	cancel         context.CancelFunc
	running        bool
	conns          map[string]*quic.Conn // pub → outbound connection (reused)
	handlers       map[string]HandlerFunc
	streamHandlers map[string]StreamHandlerFunc
	linkUp         map[string]bool // pub → last ping outcome (for up/down notifications)
}

// New builds the service. The self-signed cert is generated once.
func New(r Resolver) (*Service, error) {
	cert, err := selfSignedCert()
	if err != nil {
		return nil, fmt.Errorf("bus: cert: %w", err)
	}
	// Tight keepalive + idle so a broken link is detected and re-dialed in
	// ~20s instead of lingering as a 90s zombie that blocks every bus op.
	// Frequent keepalive (5s) also keeps marginal links from flapping.
	qc := &quic.Config{MaxIdleTimeout: 20 * time.Second, KeepAlivePeriod: 5 * time.Second}
	s := &Service{
		resolver:       r,
		tlsServer:      &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{alpn}, MinVersion: tls.VersionTLS13},
		tlsClient:      &tls.Config{InsecureSkipVerify: true, NextProtos: []string{alpn}, MinVersion: tls.VersionTLS13}, // overlay already authenticates
		quicConf:       qc,
		conns:          make(map[string]*quic.Conn),
		handlers:       make(map[string]HandlerFunc),
		streamHandlers: make(map[string]StreamHandlerFunc),
		linkUp:         make(map[string]bool),
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

// Start binds the QUIC listener on overlayIP:Port and serves it. Idempotent.
func (s *Service) Start(overlayIP string) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	addr := net.JoinHostPort(overlayIP, Port)
	ln, err := quic.ListenAddr(addr, s.tlsServer, s.quicConf)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("bus: listen %s: %w", addr, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.ln, s.cancel, s.running = ln, cancel, true
	s.mu.Unlock()
	log.Printf("bus: QUIC listening on %s", addr)
	go s.acceptLoop(ctx, ln)
	go s.pingLoop(ctx) // auto-mesh: keep a QUIC link to every peer alive
	return nil
}

// pingLoop dials + probes every known peer over QUIC every few seconds, so
// the mesh forms automatically and the traffic is visible in the logs.
func (s *Service) pingLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, pub := range s.resolver.Peers() {
				go s.pingOne(ctx, pub)
			}
		}
	}
}

func (s *Service) pingOne(ctx context.Context, pub string) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := s.Request(rctx, pub, "ping", nil)
	ok := err == nil
	rtt := time.Since(start).Round(time.Millisecond)
	if ok {
		log.Printf("bus: ping %s ok via QUIC (%s)", short(pub), rtt)
	} else {
		log.Printf("bus: ping %s failed: %v", short(pub), err)
		s.dropConn(pub) // force a fresh dial next round
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
		s.emit("ping", pub, "QUIC link up · "+rtt.String())
	case !ok && had && prev:
		s.emit("ping", pub, "QUIC link down")
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
	cancel, ln, conns := s.cancel, s.ln, s.conns
	s.cancel, s.ln, s.conns = nil, nil, make(map[string]*quic.Conn)
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	for _, c := range conns {
		_ = c.CloseWithError(0, "stop")
	}
	if ln != nil {
		return ln.Close()
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
	log.Printf("bus: inbound QUIC from %s (%s)", ip, short(fromPub))
	// Reuse this inbound connection for our own outbound sends too — QUIC
	// streams are bidirectional, so one connection per pair serves both ways.
	if fromPub != "" {
		s.adoptConn(fromPub, conn)
		defer s.forgetConn(fromPub, conn)
	}
	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		go s.serveStream(fromPub, stream)
	}
}

// adoptConn caches a connection for pub unless one is already cached.
func (s *Service) adoptConn(pub string, conn *quic.Conn) {
	s.mu.Lock()
	if s.conns[pub] == nil {
		s.conns[pub] = conn
	}
	s.mu.Unlock()
}

// forgetConn drops conn from the cache if it's still the cached one.
func (s *Service) forgetConn(pub string, conn *quic.Conn) {
	s.mu.Lock()
	if s.conns[pub] == conn {
		delete(s.conns, pub)
	}
	s.mu.Unlock()
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
		log.Printf("bus: no handler for type %q from %s", hdr.Type, short(fromPub))
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
	if err := writeFrame(st, header{Type: typ}, payload); err != nil {
		return nil, err
	}
	return readChunk(st)
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
		s.dropConn(pub)
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
	// dialing (so it never gets stuck if the peer never dials).
	if my := s.resolver.SelfPub(); my != "" && my > pub {
		if c := s.waitInbound(ctx, pub, 2*time.Second); c != nil {
			return c, nil
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

func (s *Service) dial(ctx context.Context, pub string) (*quic.Conn, error) {
	ip := s.resolver.AddrForPub(pub)
	if ip == "" {
		return nil, fmt.Errorf("bus: no overlay ip for %s", short(pub))
	}
	conn, err := quic.DialAddr(ctx, net.JoinHostPort(ip, Port), s.tlsClient, s.quicConf)
	if err != nil {
		return nil, fmt.Errorf("bus: dial %s: %w", ip, err)
	}
	s.mu.Lock()
	if existing := s.conns[pub]; existing != nil { // lost a race; keep the first
		s.mu.Unlock()
		_ = conn.CloseWithError(0, "dup")
		return existing, nil
	}
	s.conns[pub] = conn
	s.mu.Unlock()
	// Reap the moment it dies (idle-timeout, peer close, …) so it never
	// lingers in the cache as a zombie that blocks the next bus op — the
	// inbound side is reaped by serveConn's accept loop, dialed ones weren't.
	go func() {
		<-conn.Context().Done()
		s.forgetConn(pub, conn)
	}()
	return conn, nil
}

func (s *Service) dropConn(pub string) {
	s.mu.Lock()
	c := s.conns[pub]
	delete(s.conns, pub)
	s.mu.Unlock()
	if c != nil {
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

func short(p string) string {
	if len(p) > 12 {
		return p[:12]
	}
	return p
}
