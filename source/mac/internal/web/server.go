//go:build darwin

// Package web is the HTTP UI for f2f-mac. It serves an embedded SPA from
// assets/ and exposes a small REST + SSE API over the engine.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"regexp"
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
	tunnelSrv *http.Server // active while engine is running

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
// graph is a simple star: this node in the middle, the peer next to it,
// and each intercept as a leaf attached to the peer.
type topologyNode struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	Kind  string   `json:"kind"` // "self" | "peer" | "intercept"
	Spec  string   `json:"spec,omitempty"`
	IPs   []string `json:"ips,omitempty"`
}

type topologyEdge struct {
	Source  string `json:"source"`
	Target  string `json:"target"`
	TxBytes uint64 `json:"tx_bytes"`
	RxBytes uint64 `json:"rx_bytes"`
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
	selfLabel := st.UtunName
	if st.LocalIP != "" {
		selfLabel = selfLabel + " · " + st.LocalIP
	}
	t.Nodes = append(t.Nodes, topologyNode{ID: "self", Label: selfLabel, Kind: "self"})

	peerID := ""
	if st.PeerAddr != "" {
		peerID = "peer"
		t.Nodes = append(t.Nodes, topologyNode{ID: peerID, Label: st.PeerAddr, Kind: "peer"})
		// The aggregate tx/rx are reported on the self↔peer edge, since
		// every byte we count flows through it.
		t.Edges = append(t.Edges, topologyEdge{
			Source: "self", Target: peerID,
			TxBytes: st.TxBytes, RxBytes: st.RxBytes,
		})
	}

	// Hang each intercept off the peer so the visual flow reads
	// self → peer → destination.
	parent := "self"
	if peerID != "" {
		parent = peerID
	}
	for _, it := range st.Intercepts {
		id := "intercept:" + it.ID
		t.Nodes = append(t.Nodes, topologyNode{
			ID: id, Label: it.Spec, Kind: "intercept",
			Spec: it.Spec, IPs: it.Prefixes,
		})
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
