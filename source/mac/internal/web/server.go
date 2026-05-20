//go:build darwin

// Package web is the HTTP UI for f2f-mac. It serves an embedded SPA from
// assets/ and exposes a small REST + SSE API over the engine.
package web

import (
	"bytes"
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vseplet/f2f/source/mac/internal/engine"
)

//go:embed assets
var assetsFS embed.FS

// Server wraps an Engine with an HTTP handler. Two listeners are kept:
//   - srv on the user-facing loopback bind (the full UI + all API endpoints).
//   - tunnelSrv on the utun tunnel_ip, exposed once the engine is up,
//     serving ONLY POST /api/signal/inbox so the remote peer can deliver
//     WebRTC signalling through the tunnel without us exposing the UI to
//     the LAN.
type Server struct {
	engine *engine.Engine
	addr   string
	srv    *http.Server

	mu        sync.Mutex
	tunnelSrv *http.Server   // signaling/domain listener on <tunnel_ip>:<port>
	proxySrvs []*http.Server // HTTP-only reverse proxies on :80
	                         // (tunnel_ip and 127.0.0.1)

	signals    *signalHub
	signalHTTP *http.Client
}

func New(eng *engine.Engine, addr string) *Server {
	return &Server{
		engine:  eng,
		addr:    addr,
		signals: newSignalHub(),
		signalHTTP: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// Addr returns the configured bind address.
func (s *Server) Addr() string { return s.addr }

// ListenAndServe blocks until the server is shut down. Returns http.ErrServerClosed
// on graceful shutdown.
func (s *Server) ListenAndServe() error {
	mux := http.NewServeMux()
	s.routes(mux)
	s.srv = &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s.srv.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	_ = s.UnbindTunnel()
	_ = s.UnbindProxies()
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// BindTunnel starts the tunnel-side listener on ip:<same port as loopback>.
// The mux it serves is intentionally tiny — only POST /api/signal/inbox.
// Everything else 404s, so a LAN attacker who somehow reaches the tunnel
// listener can't drive Start/Stop or read state. Safe to call multiple
// times; subsequent calls with a different IP rebind.
func (s *Server) BindTunnel(ip string) error {
	if ip == "" {
		return nil
	}
	_, port, err := net.SplitHostPort(s.addr)
	if err != nil {
		return fmt.Errorf("split bind addr %q: %w", s.addr, err)
	}
	addr := net.JoinHostPort(ip, port)

	s.mu.Lock()
	if s.tunnelSrv != nil && s.tunnelSrv.Addr == addr {
		s.mu.Unlock()
		return nil // already bound there
	}
	s.mu.Unlock()
	_ = s.UnbindTunnel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/signal/inbox", s.handleSignalInbox)
	mux.HandleFunc("GET /api/domains", s.handleListDomains)
	mux.HandleFunc("GET /api/ca-cert", s.handleCACert)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.mu.Lock()
	s.tunnelSrv = srv
	s.mu.Unlock()
	go func() {
		log.Printf("tunnel inbox listening on http://%s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("WARN: tunnel inbox listener: %v", err)
		}
	}()
	return nil
}

// UnbindTunnel stops the tunnel-side listener if it's running. No-op if
// nothing was bound.
func (s *Server) UnbindTunnel() error {
	s.mu.Lock()
	srv := s.tunnelSrv
	s.tunnelSrv = nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	log.Printf("tunnel inbox stopped (%s)", srv.Addr)
	return srv.Shutdown(ctx)
}

// BindProxies starts reverse-proxy listeners for the local published
// domains on standard web ports — both tunnel_ip and 127.0.0.1:
//
//   - :80   plain HTTP
//   - :443  HTTPS, terminated with leaf certs issued on demand by the
//           local CA (engine.CA()). Only enabled when a CA is available.
//
// The proxy reads Host/SNI, looks up engine.MyDomains, and forwards
// to 127.0.0.1:<configured port> over plain HTTP. Bind failures (port
// busy, no CA) are logged but not fatal — users can keep typing
// explicit ports.
func (s *Server) BindProxies(tunnelIP string) error {
	_ = s.UnbindProxies()
	// HTTP listeners: 127.0.0.1:80 (self traffic) + tunnel_ip:80 (peer traffic).
	httpAddrs := []string{net.JoinHostPort("127.0.0.1", "80")}
	if tunnelIP != "" {
		httpAddrs = append(httpAddrs, net.JoinHostPort(tunnelIP, "80"))
	}
	for _, a := range httpAddrs {
		s.startProxyListener(a, nil)
	}
	// HTTPS listeners — same set of addresses, but only if a CA is up.
	if ca := s.engine.CA(); ca != nil {
		tlsCfg := &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				host := strings.ToLower(strings.TrimSpace(hello.ServerName))
				if host == "" {
					return nil, fmt.Errorf("tls: empty SNI")
				}
				return ca.IssueLeaf(host)
			},
		}
		tlsAddrs := []string{net.JoinHostPort("127.0.0.1", "443")}
		if tunnelIP != "" {
			tlsAddrs = append(tlsAddrs, net.JoinHostPort(tunnelIP, "443"))
		}
		for _, a := range tlsAddrs {
			s.startProxyListener(a, tlsCfg)
		}
	}
	return nil
}

// startProxyListener brings up one listener (HTTP if tlsCfg is nil,
// HTTPS otherwise) and stashes it on s.proxySrvs for shutdown.
func (s *Server) startProxyListener(addr string, tlsCfg *tls.Config) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           http.HandlerFunc(s.handleProxy),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsCfg,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		scheme := "HTTP"
		if tlsCfg != nil {
			scheme = "HTTPS"
		}
		log.Printf("proxy: bind %s %s: %v (skipping)", scheme, addr, err)
		return
	}
	if tlsCfg != nil {
		ln = tls.NewListener(ln, tlsCfg)
	}
	s.mu.Lock()
	s.proxySrvs = append(s.proxySrvs, srv)
	s.mu.Unlock()
	go func() {
		scheme := "HTTP"
		if tlsCfg != nil {
			scheme = "HTTPS"
		}
		log.Printf("proxy: %s listening on %s", scheme, addr)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("WARN: proxy %s: %v", addr, err)
		}
	}()
}

// UnbindProxies stops every active proxy listener. Idempotent.
func (s *Server) UnbindProxies() error {
	s.mu.Lock()
	srvs := s.proxySrvs
	s.proxySrvs = nil
	s.mu.Unlock()
	for _, srv := range srvs {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
		log.Printf("proxy: stopped %s", srv.Addr)
	}
	return nil
}

// handleProxy is the single reverse-proxy handler shared between both
// proxy listeners. Looks up the Host header's label in the local
// published-domains list and forwards to 127.0.0.1:<port>. Anything
// outside our zone or with no matching label returns 404.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	st := s.engine.Status()
	campID := strings.ToLower(strings.TrimSpace(st.CampID))
	if campID == "" {
		http.Error(w, "engine not in a camp", http.StatusServiceUnavailable)
		return
	}
	suffix := "." + campID + ".f2f"

	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil && h != "" {
		host = h
	}
	host = strings.ToLower(host)
	if !strings.HasSuffix(host, suffix) {
		http.Error(w, "not in this camp's f2f zone", http.StatusNotFound)
		return
	}
	label := strings.TrimSuffix(host, suffix)
	if label == "" || strings.Contains(label, ".") {
		http.Error(w, "bad subdomain", http.StatusNotFound)
		return
	}

	var port int
	for _, d := range s.engine.MyDomains() {
		if strings.EqualFold(d.Name, label) {
			port = d.Port
			break
		}
	}
	if port == 0 {
		http.Error(w, "no such domain published locally", http.StatusNotFound)
		return
	}

	target, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(port))
	proxy := httputil.NewSingleHostReverseProxy(target)
	// httputil's default ErrorHandler logs to stderr; replace with a
	// 502 so the client gets a meaningful response instead of a half-open
	// connection.
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("proxy: %s → %s: %v", host, target, err)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}

func (s *Server) routes(mux *http.ServeMux) {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		panic(err) // build-time; embed is wrong
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/start", s.handleStart)
	mux.HandleFunc("POST /api/stop", s.handleStop)
	mux.HandleFunc("POST /api/intercepts", s.handleAddIntercept)
	mux.HandleFunc("DELETE /api/intercepts/{id}", s.handleRemoveIntercept)
	mux.HandleFunc("POST /api/peers/active", s.handleSetActivePeer)
	mux.HandleFunc("GET /api/topology", s.handleTopology)
	mux.HandleFunc("GET /api/log/stream", s.handleLogStream)
	mux.HandleFunc("GET /api/camp/peers", s.handleCampPeers)

	// Local DNS / domain management. /api/my-domains is the UI side
	// (read/write our own list); /api/domains is the read-only side
	// exposed on the tunnel listener for peers to poll.
	mux.HandleFunc("GET /api/my-domains", s.handleListMyDomains)
	mux.HandleFunc("PUT /api/my-domains", s.handleSetMyDomains)
	mux.HandleFunc("GET /api/domains", s.handleListDomains)
	mux.HandleFunc("GET /api/ca-cert", s.handleCACert)
	mux.HandleFunc("GET /api/trusted-peers", s.handleTrustedPeers)

	// WebRTC signalling: outbox = browser → peer, inbox = peer → us,
	// stream = us → browser. Wire-format is opaque JSON blobs forwarded
	// verbatim; only the browsers care about their contents.
	mux.HandleFunc("POST /api/signal/outbox", s.handleSignalOutbox)
	mux.HandleFunc("POST /api/signal/inbox", s.handleSignalInbox)
	mux.HandleFunc("GET /api/signal/stream", s.handleSignalStream)
}

// signalHub is a tiny in-memory broadcaster. Every browser connected to
// /api/signal/stream gets its own channel; signals arriving from the peer
// are fanned out to all of them.
type signalHub struct {
	mu          sync.Mutex
	subscribers map[chan []byte]struct{}
}

func newSignalHub() *signalHub {
	return &signalHub{subscribers: map[chan []byte]struct{}{}}
}

func (h *signalHub) subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 32)
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subscribers[ch]; ok {
			delete(h.subscribers, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

func (h *signalHub) broadcast(data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subscribers {
		select {
		case ch <- data:
		default:
			// Subscriber too slow; drop. Browsers will reconnect on stream
			// error if they really fell behind.
		}
	}
}

// mdnsHostCandidateRE matches the address inside a WebRTC ICE candidate
// of type "host" that uses an mDNS hostname (e.g. "abc-123.local"). Chrome
// and Firefox both mask local IPs this way for privacy, which breaks our
// no-STUN setup because *.local names are only resolvable on the
// originating machine. We rewrite those addresses to our tunnel IP on the
// way out, so the peer sees a candidate it can actually reach.
var mdnsHostCandidateRE = regexp.MustCompile(`(?i)((?:udp|tcp)\s+\d+\s+)([A-Za-z0-9.-]+\.local)(\s+\d+\s+typ\s+host)`)

func rewriteMDNS(body []byte, tunnelIP string) []byte {
	if tunnelIP == "" {
		return body
	}
	return mdnsHostCandidateRE.ReplaceAll(body, []byte("${1}"+tunnelIP+"${3}"))
}

// handleSignalOutbox accepts a JSON signalling message from the local
// browser and forwards it as-is to the peer's /api/signal/inbox over the
// tunnel. The peer IP comes from the engine config; the port is assumed
// to match ours (both sides run the UI on the same port).
func (s *Server) handleSignalOutbox(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	st := s.engine.Status()
	if !st.Running || st.PeerIP == "" {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("engine not running or peer IP unknown"))
		return
	}
	// Strip mDNS masking from any host candidates before forwarding, so the
	// peer's WebRTC stack sees our real tunnel IP and can actually pair.
	body = rewriteMDNS(body, st.LocalIP)
	_, port, err := net.SplitHostPort(s.addr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("server addr %q: %w", s.addr, err))
		return
	}
	url := "http://" + net.JoinHostPort(st.PeerIP, port) + "/api/signal/inbox"
	req, err := http.NewRequestWithContext(r.Context(), "POST", url, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.signalHTTP.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("forward to peer: %w", err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		writeError(w, http.StatusBadGateway, fmt.Errorf("peer returned %s", resp.Status))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSignalInbox receives forwarded signalling messages from the peer
// and broadcasts them to local browser subscribers. As a side effect it
// auto-selects the sending peer as active — so the receiver doesn't have
// to manually pick the caller from a dropdown before being able to
// answer. The source tunnel_ip is read from RemoteAddr, which is the
// peer's address as routed through utun.
func (s *Server) handleSignalInbox(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if host, _, splitErr := net.SplitHostPort(r.RemoteAddr); splitErr == nil && host != "" {
		st := s.engine.Status()
		if st.Running && host != st.ActivePeerTunnelIP {
			if err := s.engine.SetActivePeer(host); err != nil {
				// Not fatal — could just be a peer we haven't seen yet
				// in the camp roster. The signal still gets broadcast.
				_ = err
			}
		}
	}
	s.signals.broadcast(body)
	w.WriteHeader(http.StatusNoContent)
}

// handleSignalStream is an SSE endpoint so the browser can receive
// signalling messages from the peer.
func (s *Server) handleSignalStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, unsubscribe := s.signals.subscribe()
	defer unsubscribe()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case data, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// topologyNode and topologyEdge feed the d3 force graph in the UI. The
// graph is: self in the middle, one node per known camp peer, and each
// intercept hung off its bound peer's node. Orphan intercepts (peer
// unknown) hang off self.
type topologyNode struct {
	ID     string   `json:"id"`
	Label  string   `json:"label"`
	Kind   string   `json:"kind"` // "self" | "peer" | "intercept"
	Spec   string   `json:"spec,omitempty"`
	IPs    []string `json:"ips,omitempty"`
	Online bool     `json:"online,omitempty"` // peer nodes only
}

type topologyEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type topology struct {
	Running bool           `json:"running"`
	Nodes   []topologyNode `json:"nodes"`
	Edges   []topologyEdge `json:"edges"`
}

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	st := s.engine.Status()
	t := topology{Running: st.Running, Nodes: []topologyNode{}, Edges: []topologyEdge{}}
	if !st.Running {
		writeJSON(w, http.StatusOK, t)
		return
	}
	// Unified node labels: every actor in the camp is "<name> · <tunnel_ip>".
	// Interface name (utun7) and public endpoints are diagnostic detail,
	// shown elsewhere — here we want the user-visible identity.
	selfName := st.CampName
	if selfName == "" {
		selfName = "you"
	}
	selfLabel := selfName
	if st.LocalIP != "" {
		selfLabel += " · " + st.LocalIP
	}
	t.Nodes = append(t.Nodes, topologyNode{ID: "self", Label: selfLabel, Kind: "self"})

	peerIDByName := map[string]string{}
	for _, p := range st.Peers {
		if p.Self {
			continue
		}
		id := "peer:" + p.Name
		peerIDByName[p.Name] = id
		label := p.Name
		if p.TunnelIP != "" {
			label += " · " + p.TunnelIP
		}
		t.Nodes = append(t.Nodes, topologyNode{ID: id, Label: label, Kind: "peer", Online: p.Online})
		t.Edges = append(t.Edges, topologyEdge{Source: "self", Target: id})
	}

	for _, it := range st.Intercepts {
		id := "intercept:" + it.ID
		t.Nodes = append(t.Nodes, topologyNode{
			ID: id, Label: it.Spec, Kind: "intercept",
			Spec: it.Spec, IPs: it.Prefixes,
		})
		parent := peerIDByName[it.Peer]
		if parent == "" {
			parent = "self" // orphan — peer not visible right now
		}
		t.Edges = append(t.Edges, topologyEdge{Source: parent, Target: id})
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.Status())
}

// handleCampPeers serves the engine's view of the camp: peer list with
// reachability flags and the active selection. Reads from local engine
// state — no camp HTTP call here (the engine's poller refreshes the
// cache every ~30s).
func (s *Server) handleCampPeers(w http.ResponseWriter, r *http.Request) {
	st := s.engine.Status()
	if !st.CampActive {
		writeJSON(w, http.StatusOK, map[string]any{"running": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"running": true,
		"you":     st.CampName,
		"camp_id": st.CampID,
		"active":  st.ActivePeerTunnelIP,
		"peers":   st.Peers,
	})
}

// handleListMyDomains returns this peer's own published domain list.
// UI-facing — served only on the loopback listener.
func (s *Server) handleListMyDomains(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.MyDomains())
}

// handleSetMyDomains replaces the entire list. PUT semantics — the
// body is the new full list; missing entries are removed.
func (s *Server) handleSetMyDomains(w http.ResponseWriter, r *http.Request) {
	var list []engine.DomainEntry
	if err := json.NewDecoder(r.Body).Decode(&list); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cleaned := make([]engine.DomainEntry, 0, len(list))
	seen := make(map[string]struct{}, len(list))
	for _, e := range list {
		name := strings.ToLower(strings.TrimSpace(e.Name))
		if name == "" || !isValidDomainLabel(name) {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		entry := engine.DomainEntry{Name: name}
		if e.Port > 0 && e.Port < 65536 {
			entry.Port = e.Port
		}
		if e.Proto != "" {
			entry.Proto = e.Proto
		}
		cleaned = append(cleaned, entry)
	}
	s.engine.SetMyDomains(cleaned)
	writeJSON(w, http.StatusOK, cleaned)
}

func isValidDomainLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return s[0] != '-' && s[len(s)-1] != '-'
}

// handleListDomains exposes the list to OTHER peers. Identical body to
// handleListMyDomains but mounted on the tunnel listener so cross-peer
// polling works. Mounted on the loopback listener too as a debug aid.
func (s *Server) handleListDomains(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.MyDomains())
}

// handleCACert serves the local CA's public cert in PEM form. Polled
// by peers to install us as a trusted root for HTTPS.
func (s *Server) handleCACert(w http.ResponseWriter, r *http.Request) {
	ca := s.engine.CA()
	if ca == nil {
		http.Error(w, "ca not initialized", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	_, _ = w.Write(ca.CertPEM)
}

// handleTrustedPeers returns the UI's view of which peer CAs we've
// installed locally — fingerprint, common name, install timestamp.
func (s *Server) handleTrustedPeers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.TrustedPeerCAs())
}

type startRequest struct {
	CampName string `json:"camp_name"`
	CampID   string `json:"camp_id"`
}

// handleStart accepts only camp identity from the UI now; everything else
// (utun addresses, UDP listen port, camp endpoint) uses sensible defaults
// that the engine fills in. Static-peer mode is no longer reachable from
// the UI — use the CLI if you need it.
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.CampName == "" || req.CampID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("camp_name and camp_id are required"))
		return
	}
	cfg := engine.Config{
		LocalIP: "10.99.0.1", // placeholder; camp overrides with sticky tunnel_ip
		Listen:  ":9000",
		Camp: &engine.CampConfig{
			URL:      "wss://f2f-camp.fly.dev/ws",
			StunAddr: "f2f-camp.fly.dev:3478",
			Name:     req.CampName,
			ID:       req.CampID,
		},
	}
	if err := s.engine.Start(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.engine.Status())
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.Stop(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, s.engine.Status())
}

type addInterceptRequest struct {
	Spec string `json:"spec"`
	Peer string `json:"peer"`
}

func (s *Server) handleAddIntercept(w http.ResponseWriter, r *http.Request) {
	var req addInterceptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	info, err := s.engine.AddIntercept(req.Spec, req.Peer)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleRemoveIntercept(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.engine.RemoveIntercept(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetActivePeer sets the user-selected active peer (catch-all
// tunnel destination and meet signalling target). Body: {tunnel_ip}.
// Empty string clears.
func (s *Server) handleSetActivePeer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TunnelIP string `json:"tunnel_ip"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.engine.SetActivePeer(req.TunnelIP); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch, unsubscribe := s.engine.Subscribe(64)
	defer unsubscribe()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	// Send an initial comment so the client knows the stream is alive.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case entry, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
