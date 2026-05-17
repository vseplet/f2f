//go:build darwin

// Package web is the HTTP UI for f2f-mac. It serves an embedded SPA from
// assets/ and exposes a small REST + SSE API over the engine.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/vseplet/f2f/source/mac/internal/engine"
)

//go:embed assets
var assetsFS embed.FS

// Server wraps an Engine with an HTTP handler.
type Server struct {
	engine *engine.Engine
	addr   string
	srv    *http.Server

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
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
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
	mux.HandleFunc("POST /api/inbound-allow", s.handleAddInboundAllow)
	mux.HandleFunc("DELETE /api/inbound-allow/{id}", s.handleRemoveInboundAllow)
	mux.HandleFunc("GET /api/ifaces", s.handleIfaces)
	mux.HandleFunc("GET /api/topology", s.handleTopology)
	mux.HandleFunc("GET /api/log/stream", s.handleLogStream)

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
// and broadcasts them to local browser subscribers.
func (s *Server) handleSignalInbox(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
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

type startRequest struct {
	LocalIP      string   `json:"local_ip"`
	PeerIP       string   `json:"peer_ip"`
	Listen       string   `json:"listen"`
	Peer         string   `json:"peer"`
	Intercepts   []string `json:"intercepts"`
	InboundAllow []string `json:"inbound_allow"`
	EgressIface  string   `json:"egress_iface"`
	EgressSubnet string   `json:"egress_subnet"`
	CampURL      string   `json:"camp_url"`
	CampStun     string   `json:"camp_stun"`
	CampName     string   `json:"camp_name"`
	CampRoom     string   `json:"camp_room"`
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cfg := engine.Config{
		LocalIP:      req.LocalIP,
		PeerIP:       req.PeerIP,
		Listen:       req.Listen,
		Peer:         req.Peer,
		Intercepts:   req.Intercepts,
		InboundAllow: req.InboundAllow,
		EgressIface:  req.EgressIface,
		EgressSubnet: req.EgressSubnet,
	}
	if req.CampName != "" && req.CampRoom != "" {
		url := req.CampURL
		if url == "" {
			url = "wss://f2f-camp.fly.dev/ws"
		}
		stun := req.CampStun
		if stun == "" {
			stun = "f2f-camp.fly.dev:3478"
		}
		cfg.Camp = &engine.CampConfig{
			URL:      url,
			StunAddr: stun,
			Name:     req.CampName,
			Room:     req.CampRoom,
		}
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
}

func (s *Server) handleAddIntercept(w http.ResponseWriter, r *http.Request) {
	var req addInterceptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	info, err := s.engine.AddIntercept(req.Spec)
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

func (s *Server) handleAddInboundAllow(w http.ResponseWriter, r *http.Request) {
	var req addInterceptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	info, err := s.engine.AddInboundAllow(req.Spec)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleRemoveInboundAllow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.engine.RemoveInboundAllow(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type ifaceInfo struct {
	Name      string `json:"name"`
	IP        string `json:"ip,omitempty"`
	IsDefault bool   `json:"is_default,omitempty"`
}

func (s *Server) handleIfaces(w http.ResponseWriter, r *http.Request) {
	ifs, err := net.Interfaces()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defaultName := defaultRouteIface()
	out := []ifaceInfo{}
	for _, iface := range ifs {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if strings.HasPrefix(iface.Name, "utun") {
			continue
		}
		info := ifaceInfo{Name: iface.Name, IsDefault: iface.Name == defaultName}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				info.IP = ipn.IP.String()
				break
			}
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}

// defaultRouteIface returns the interface name of the IPv4 default route,
// or "" if it can't be determined. We never pick a utun* (avoids loops if
// another VPN owns the default route).
func defaultRouteIface() string {
	out, err := exec.Command("/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "interface:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(line, "interface:"))
		if strings.HasPrefix(name, "utun") {
			return ""
		}
		return name
	}
	return ""
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
