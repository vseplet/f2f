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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/vseplet/f2f/source/helper/clog"
	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/identity"
	"github.com/vseplet/f2f/source/helper/mesh/engine"
	"github.com/vseplet/f2f/source/helper/mesh/gossip"
	"github.com/vseplet/f2f/source/helper/platform"
	"github.com/vseplet/f2f/source/helper/services/calls"
	"github.com/vseplet/f2f/source/helper/services/camp"
	"github.com/vseplet/f2f/source/helper/services/dns"
	"github.com/vseplet/f2f/source/helper/services/drop"
	"github.com/vseplet/f2f/source/helper/services/firewall"
	"github.com/vseplet/f2f/source/helper/services/messenger"
	"github.com/vseplet/f2f/source/helper/services/notify"
	"github.com/vseplet/f2f/source/helper/services/pki"
	"github.com/vseplet/f2f/source/helper/services/tunnel"
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
	engine   *engine.Engine
	store    *config.Store
	firewall *firewall.Service
	pki      *pki.Service
	drop     *drop.Service
	calls    *calls.Service
	tunnel   *tunnel.Service
	camp     *camp.Service
	dns      *dns.Service
	msg      *messenger.Store
	notify   *notify.Service
	gossip   *gossip.Service
	addr     string
	srv      *http.Server

	mu        sync.Mutex
	tunnelSrv *http.Server // signal/domain listener on tunnel v4

	signals     *signalHub
	callSignals *signalHub // SSE hub for SFU signals → local browser
	signalHTTP  *http.Client
}

func New(eng *engine.Engine, store *config.Store, fwSvc *firewall.Service, pkiSvc *pki.Service, dnsSvc *dns.Service, dropSvc *drop.Service, callsSvc *calls.Service, tunnelSvc *tunnel.Service, campSvc *camp.Service, msgSvc *messenger.Store, notifySvc *notify.Service, gossipSvc *gossip.Service, addr string) *Server {
	s := &Server{
		engine:      eng,
		store:       store,
		firewall:    fwSvc,
		pki:         pkiSvc,
		dns:         dnsSvc,
		drop:        dropSvc,
		calls:       callsSvc,
		tunnel:      tunnelSvc,
		camp:        campSvc,
		msg:         msgSvc,
		notify:      notifySvc,
		gossip:      gossipSvc,
		addr:        addr,
		signals:     newSignalHub(),
		callSignals: newSignalHub(),
		signalHTTP: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
	callsSvc.OnLocalSignal = func(msg []byte) {
		s.callSignals.broadcast(msg)
	}
	return s
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
func (s *Server) BindTunnel(ip string) error {
	if ip == "" {
		return nil
	}
	_ = s.UnbindTunnel()
	_, port, err := net.SplitHostPort(s.addr)
	if err != nil {
		return fmt.Errorf("split bind addr %q: %w", s.addr, err)
	}
	addr := net.JoinHostPort(ip, port)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/signal/inbox", s.handleSignalInbox)
	mux.HandleFunc("GET /api/domains", s.handleListDomains)
	mux.HandleFunc("GET /api/ca-cert", s.handleCACert)
	mux.HandleFunc("GET /api/files", s.handleListFiles)
	mux.HandleFunc("GET /api/firewall", s.handleListFirewall)
	mux.HandleFunc("POST /api/call/signal", s.handleCallSignalInbound)
	mux.HandleFunc("GET /api/call/state", s.handleCallState)
	mux.HandleFunc("POST /api/call/join", s.handleCallJoinRemote)
	mux.HandleFunc("POST /api/call/leave", s.handleCallLeaveRemote)
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

// UnbindTunnel stops the tunnel-side listener.
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
	if devDir := os.Getenv("F2F_DEV_ASSETS"); devDir != "" {
		log.Printf("web: serving assets from disk (F2F_DEV_ASSETS=%s)", devDir)
		mux.Handle("/", http.FileServer(http.Dir(devDir)))
	} else {
		sub, err := fs.Sub(assetsFS, "assets")
		if err != nil {
			panic(err) // build-time; embed is wrong
		}
		mux.Handle("/", http.FileServer(http.FS(sub)))
	}

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
	mux.HandleFunc("DELETE /api/peer-domains/{peer}/{name}", s.handleRemovePeerDomain)
	mux.HandleFunc("GET /api/ca-cert", s.handleCACert)
	mux.HandleFunc("GET /api/my-ca", s.handleMyCA)
	mux.HandleFunc("GET /api/trusted-peers", s.handleTrustedPeers)
	mux.HandleFunc("POST /api/trusted-peers/{fp}/install", s.handleInstallTrustedPeer)
	mux.HandleFunc("DELETE /api/trusted-peers/{fp}", s.handleRemoveTrustedPeer)

	// Per-camp configuration. /api/camps is the global view (list +
	// last selected); /api/camp/{id} returns the full config file so
	// the UI can preview before switching.
	mux.HandleFunc("GET /api/camps", s.handleListCamps)
	mux.HandleFunc("GET /api/camp/{id}", s.handleGetCamp)

	// Firewall: default-deny inbound on utun + user-configurable
	// allow list (in addition to f2f's own ports). Loopback-only,
	// settings for *this* peer.
	mux.HandleFunc("GET /api/firewall", s.handleListFirewall)
	mux.HandleFunc("PUT /api/firewall", s.handleSetFirewall)

	// File sharing via BitTorrent (camp-only, no DHT/public trackers).
	// /api/files/mine — UI-facing CRUD for what we publish.
	// /api/files — read-only listing exposed on the tunnel listener
	// so peers can browse our catalog.
	mux.HandleFunc("GET /api/files/mine", s.handleListMyFiles)
	mux.HandleFunc("POST /api/files/mine", s.handleAddMyFile)
	mux.HandleFunc("POST /api/files/mine/upload", s.handleUploadMyFile)
	mux.HandleFunc("DELETE /api/files/mine/{hash}", s.handleRemoveMyFile)
	mux.HandleFunc("POST /api/files/download", s.handleAddDownload)
	mux.HandleFunc("GET /api/files/downloads", s.handleListDownloads)
	mux.HandleFunc("DELETE /api/files/downloads/{hash}", s.handleRemoveDownload)
	mux.HandleFunc("POST /api/files/reveal", s.handleRevealFile)
	mux.HandleFunc("GET /api/files", s.handleListFiles)

	// WebRTC signalling: outbox = browser → peer, inbox = peer → us,
	// stream = us → browser. Wire-format is opaque JSON blobs forwarded
	// verbatim; only the browsers care about their contents.
	mux.HandleFunc("POST /api/signal/outbox", s.handleSignalOutbox)
	mux.HandleFunc("POST /api/signal/inbox", s.handleSignalInbox)
	mux.HandleFunc("GET /api/signal/stream", s.handleSignalStream)

	// Notifications: recent list + live SSE stream for the UI.
	mux.HandleFunc("GET /api/notifications", s.handleNotifications)
	mux.HandleFunc("GET /api/notifications/stream", s.handleNotificationsStream)

	// Mesh: fabric-level NodeStates (platform + peer-view) replicated via gossip.
	mux.HandleFunc("GET /api/mesh", s.handleMesh)

	// Group calls (SFU-based). Browser-facing endpoints on the UI
	// server; /api/call/signal also registered on the tunnel listener
	// so remote peers can deliver SFU signals.
	mux.HandleFunc("GET /api/call/state", s.handleCallState)
	mux.HandleFunc("GET /api/call/list", s.handleCallList)
	mux.HandleFunc("POST /api/call/create", s.handleCallCreate)
	mux.HandleFunc("POST /api/call/join", s.handleCallJoin)
	mux.HandleFunc("POST /api/call/leave", s.handleCallLeave)
	mux.HandleFunc("POST /api/call/signal", s.handleCallSignal)
	mux.HandleFunc("GET /api/call/signal/stream", s.handleCallSignalStream)
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
	if !st.Running {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("engine not running"))
		return
	}
	// A signal can target a specific peer by pub via {"to":"<pub>"} —
	// preferred, no dependency on the active selection. Falls back to the
	// active peer when "to" is absent (legacy 1:1). The sender's pub is
	// tagged as "from" so the recipient can address replies back.
	var sig map[string]any
	_ = json.Unmarshal(body, &sig)
	toPub, _ := sig["to"].(string)
	var peerIP string
	for _, p := range st.Peers {
		if p.Self || p.Pub == "" {
			continue
		}
		if (toPub != "" && p.Pub == toPub) || (toPub == "" && p.Pub == st.ActivePeerPub) {
			peerIP = p.OverlayV4
			break
		}
	}
	if peerIP == "" {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("no target peer (unknown 'to' pub, and no active peer)"))
		return
	}
	if sig != nil {
		sig["from"] = st.IdentityPub
		delete(sig, "to")
		if b, mErr := json.Marshal(sig); mErr == nil {
			body = b
		}
	}
	body = rewriteMDNS(body, st.LocalIP)
	_, port, err := net.SplitHostPort(s.addr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("server addr %q: %w", s.addr, err))
		return
	}
	url := "http://" + net.JoinHostPort(peerIP, port) + "/api/signal/inbox"
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
	// Auto-select active peer by matching the caller's v4 overlay
	// address (pub-derived, unique per peer) to a known peer.
	if host, _, splitErr := net.SplitHostPort(r.RemoteAddr); splitErr == nil && host != "" {
		st := s.engine.Status()
		if st.Running {
			for _, p := range st.Peers {
				if p.Self || p.Pub == "" {
					continue
				}
				if p.OverlayV4 == host && p.Pub != st.ActivePeerPub {
					_ = s.engine.SetActivePeer(p.Pub)
					break
				}
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

// handleNotifications returns the buffered notifications (oldest-first).
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.notify.Recent())
}

// handleMesh returns every peer's gossip NodeState — the mesh-wide topology
// (who-sees-whom) + platform inventory.
func (s *Server) handleMesh(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.gossip.All())
}

// handleNotificationsStream pushes new notifications to the browser over SSE.
func (s *Server) handleNotificationsStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, unsubscribe := s.notify.Subscribe()
	defer unsubscribe()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case n, ok := <-ch:
			if !ok {
				return
			}
			if b, err := json.Marshal(n); err == nil {
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
			}
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
	selfName := "you"
	if st.CampID != "" {
		if c, _ := s.store.SnapshotCamp(st.CampID); c != nil && c.Identity.Name != "" {
			selfName = c.Identity.Name
		}
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
		if p.OverlayV4 != "" {
			label += " · " + p.OverlayV4
		}
		t.Nodes = append(t.Nodes, topologyNode{ID: id, Label: label, Kind: "peer", Online: p.Online})
		t.Edges = append(t.Edges, topologyEdge{Source: "self", Target: id})
	}

	for _, it := range s.tunnel.List() {
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
	writeJSON(w, http.StatusOK, s.statusWithDomains())
}

// peerStatusView wraps engine.PeerStatusInfo with per-peer fields
// the engine no longer carries — domains (owned by services/dns)
// and files (owned by services/drop). Marshals as the original
// PeerStatusInfo shape plus extra fields, so the UI keeps working
// without changes.
type peerStatusView struct {
	engine.PeerStatusInfo
	Domains  []dns.Entry       `json:"domains,omitempty"`
	Files    []drop.PeerFile   `json:"files,omitempty"`
	Firewall []config.Firewall `json:"firewall,omitempty"`
}

// statusView re-serialises engine.Status with peerStatusView in
// place of the bare PeerStatusInfo, preserving the rest of the
// status shape verbatim.
type statusView struct {
	engine.Status
	Peers      []peerStatusView       `json:"peers"`
	Intercepts []tunnel.InterceptInfo `json:"intercepts"`
	// Camp metadata (URL/Name/ID/Label/PeerName) read from engine
	// config snapshot — engine.Status proper carries only runtime
	// state, not cfg metadata.
	CampURL      string `json:"camp_url,omitempty"`
	CampName     string `json:"camp_name,omitempty"`
	CampID       string `json:"camp_id,omitempty"`
	CampLabel    string `json:"camp_label,omitempty"`
	CampPeerName string `json:"camp_peer_name,omitempty"`
	// Camp connection signals (Active/Reflex/Health) come from
	// services/camp; web merges them into /api/status so the UI
	// keeps one endpoint.
	CampActive bool         `json:"camp_active"`
	CampReflex string       `json:"camp_reflex,omitempty"`
	CampHealth *camp.Health `json:"camp_health,omitempty"`
	// Sidebar tree fodder. Pulled in here so the left-rail tree can
	// render off a single /api/status response — saves N fetches per
	// 2s tick. Keep these to lightweight in-memory reads only.
	KnownCamps   []config.KnownCamp `json:"known_camps,omitempty"`
	Calls        []calls.State      `json:"calls,omitempty"`
	TrustedPeers []pki.PeerEntry    `json:"trusted_peers,omitempty"`
}

// statusWithDomains assembles the /api/status response by joining
// engine.Status with the dns service's view of each peer's domain
// catalog. Self gets MyDomains; remote peers get their polled
// PeerDomains snapshot (empty until the first poll succeeds).
func (s *Server) statusWithDomains() statusView {
	st := s.engine.Status()
	peers := make([]peerStatusView, 0, len(st.Peers))
	mine := s.dns.MyDomains()
	for _, p := range st.Peers {
		v := peerStatusView{PeerStatusInfo: p}
		if p.Self {
			v.Domains = mine
			// Self files come from the local torrent seed list; self
			// firewall is user + builtin ports merged into one
			// peer-shape array so the sidebar tree can iterate every
			// peer uniformly.
			if t := s.drop.Client(); t != nil {
				for _, h := range t.ListSeeds() {
					v.Files = append(v.Files, drop.PeerFile{
						Name: h.Name, Size: h.Size,
						InfoHash: h.InfoHash, Magnet: h.Magnet,
					})
				}
			}
			v.Firewall = append(v.Firewall, s.firewall.BuiltinPorts()...)
			v.Firewall = append(v.Firewall, s.firewall.UserPorts()...)
		} else if p.Pub != "" {
			v.Domains = s.dns.PeerDomains(p.Pub)
			v.Files = s.drop.PeerFiles(p.Pub)
			v.Firewall = s.firewall.PeerPorts(p.Pub)
		}
		peers = append(peers, v)
	}
	reflex := s.camp.Reflex()
	// Self peer entry from engine doesn't carry UDPEndpoint anymore —
	// merge camp.Reflex() in here.
	if reflex != "" {
		for i := range peers {
			if peers[i].Self {
				peers[i].UDPEndpoint = reflex
				break
			}
		}
	}
	// Camp metadata from engine config snapshot. PeerName derived
	// from active peer.
	var (
		campURL, campName, campID, campLabel, peerName string
	)
	campID = st.CampID
	if campID != "" {
		if c, _ := s.store.SnapshotCamp(campID); c != nil {
			campURL = c.ServerURL
			campName = c.Identity.Name
			campLabel = identity.CampLabel(campID)
		}
	}
	if st.ActivePeerPub != "" {
		for _, p := range st.Peers {
			if p.Pub == st.ActivePeerPub && !p.Self {
				peerName = p.Name
				break
			}
		}
	}
	var health *camp.Health
	if st.Running && campID != "" {
		health = s.camp.HealthSnapshot()
	}
	var knownCamps []config.KnownCamp
	if gs, err := s.engine.ListCamps(); err == nil && gs != nil {
		knownCamps = gs.KnownCamps
	}
	return statusView{
		Status:       st,
		Peers:        peers,
		Intercepts:   s.tunnel.List(),
		CampURL:      campURL,
		CampName:     campName,
		CampID:       campID,
		CampLabel:    campLabel,
		CampPeerName: peerName,
		CampActive:   s.camp.Active(),
		CampReflex:   reflex,
		CampHealth:   health,
		KnownCamps:   knownCamps,
		Calls:        s.calls.AllCalls(),
		TrustedPeers: s.pki.ListPeerCAs(),
	}
}

// handleCampPeers serves the engine's view of the camp: peer list with
// reachability flags and the active selection. Reads from local engine
// state — no camp HTTP call here (the announce reply refreshes the
// cache every ~20s).
func (s *Server) handleCampPeers(w http.ResponseWriter, r *http.Request) {
	view := s.statusWithDomains()
	if !view.CampActive {
		writeJSON(w, http.StatusOK, map[string]any{"running": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"running": true,
		"you":     view.CampName,
		"camp_id": view.CampID,
		"active":  view.ActivePeerPub,
		"peers":   view.Peers,
	})
}

// handleListMyDomains returns this peer's own published domain list.
// UI-facing — served only on the loopback listener.
func (s *Server) handleListMyDomains(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.dns.MyDomains())
}

// handleSetMyDomains replaces the entire list. PUT semantics — the
// body is the new full list; missing entries are removed.
func (s *Server) handleSetMyDomains(w http.ResponseWriter, r *http.Request) {
	var list []dns.Entry
	if err := json.NewDecoder(r.Body).Decode(&list); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cleaned := make([]dns.Entry, 0, len(list))
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
		entry := dns.Entry{Name: name}
		if e.Port > 0 && e.Port < 65536 {
			entry.Port = e.Port
		}
		if e.Proto != "" {
			entry.Proto = e.Proto
		}
		if h := strings.TrimSpace(e.Host); h != "" {
			entry.Host = h
		}
		cleaned = append(cleaned, entry)
	}
	if err := s.dns.SetMyDomains(cleaned); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, cleaned)
}

// isValidDomainLabel accepts three forms used in MyDomains:
//   - simple:  "gitea"
//   - nested:  "gitea.mini"
//   - wildcard catch-all: "*.mini"
//
// Each dot-separated piece is checked as a DNS label.
func isValidDomainLabel(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	if strings.HasPrefix(s, "*.") {
		s = s[2:]
		if s == "" {
			return false
		}
	}
	for _, part := range strings.Split(s, ".") {
		if !isValidDNSPart(part) {
			return false
		}
	}
	return true
}

func isValidDNSPart(s string) bool {
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
	writeJSON(w, http.StatusOK, s.dns.MyDomains())
}

// handleCACert serves the local CA's public cert in PEM form. Polled
// by peers to install us as a trusted root for HTTPS.
func (s *Server) handleCACert(w http.ResponseWriter, r *http.Request) {
	ca := s.pki.MyCA()
	if ca == nil {
		http.Error(w, "ca not initialized", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	_, _ = w.Write(ca.CertPEM)
}

func (s *Server) handleMyCA(w http.ResponseWriter, r *http.Request) {
	ca := s.pki.MyCA()
	if ca == nil {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"common_name": ca.CommonName(),
		"fingerprint": ca.Fingerprint(),
	})
}

// handleTrustedPeers returns the UI's view of discovered peer CAs —
// fingerprint, common name, install state.
func (s *Server) handleTrustedPeers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.pki.ListPeerCAs())
}

// handleInstallTrustedPeer adds a discovered peer CA to the system
// keychain (macOS prompts for the admin password). Triggered by the
// user clicking "install".
func (s *Server) handleInstallTrustedPeer(w http.ResponseWriter, r *http.Request) {
	fp := r.PathValue("fp")
	if fp == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("missing fingerprint"))
		return
	}
	if err := s.pki.InstallPeerCA(fp); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRemovePeerDomain drops one (peer, domain) entry from the
// peer-catalog in camp config and from the live peer's Domains. If
// the peer is still publishing the name, next poll re-adds it.
func (s *Server) handleRemovePeerDomain(w http.ResponseWriter, r *http.Request) {
	peer := r.PathValue("peer")
	name := r.PathValue("name")
	if peer == "" || name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("peer and name required"))
		return
	}
	if err := s.dns.RemovePeerDomain(peer, name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRemoveTrustedPeer drops one peer CA by fingerprint — file on
// disk, keychain entry, and config entry. 204 on success; 404 if the
// fingerprint isn't known.
func (s *Server) handleRemoveTrustedPeer(w http.ResponseWriter, r *http.Request) {
	fp := r.PathValue("fp")
	if fp == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("missing fingerprint"))
		return
	}
	if err := s.pki.RemovePeerCA(fp); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListCamps returns the global state file — last selected
// camp_id and the roster of every camp the user has ever joined.
// Powers the UI's camp dropdown / switcher.
func (s *Server) handleListCamps(w http.ResponseWriter, r *http.Request) {
	st, err := s.engine.ListCamps()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if st.KnownCamps == nil {
		st.KnownCamps = []config.KnownCamp{}
	}
	writeJSON(w, http.StatusOK, st)
}

// handleGetCamp returns the full on-disk config for one camp_id —
// intercepts, my-domains, firewall, trusted-peer fingerprints, peer
// catalog. 404 if no config exists yet for that id (i.e. the user
// hasn't started the engine with this camp_id).
func (s *Server) handleGetCamp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("missing camp_id"))
		return
	}
	c, err := s.store.SnapshotCamp(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if c == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("no config for camp %q", id))
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// handleListFirewall returns the inbound-utun allow list: built-in
// f2f ports (read-only) + user-configured ports. `active` reflects
// whether the pf anchor is loaded — false = engine stopped or pf
// load failed, in which case ports show as inactive in UI.
func (s *Server) handleListFirewall(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"active":  s.firewall.Active(),
		"builtin": s.firewall.BuiltinPorts(),
		"user":    s.firewall.UserPorts(),
	})
}

// handleSetFirewall replaces the user-configured allow list. Built-in
// rules cannot be modified. Body shape: {"user": [{port, protocol,
// description, enabled}, ...]}.
func (s *Server) handleSetFirewall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		User []config.Firewall `json:"user"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.firewall.SetUserPorts(body.User); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active":  s.firewall.Active(),
		"builtin": s.firewall.BuiltinPorts(),
		"user":    s.firewall.UserPorts(),
	})
}

// fileEntry is the JSON shape returned by /api/files and friends.
type fileEntry struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	InfoHash string `json:"info_hash"`
	Magnet   string `json:"magnet"`
	Path     string `json:"path,omitempty"` // omitted from peer-facing response
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	t := s.drop.Client()
	if t == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("torrent client not running"))
		return
	}
	out := []fileEntry{}
	for _, h := range t.ListSeeds() {
		out = append(out, fileEntry{
			Name: h.Name, Size: h.Size,
			InfoHash: h.InfoHash, Magnet: h.Magnet,
			// Path intentionally omitted on peer-facing path; we strip
			// below for tunnel listener requests.
			Path: h.Path,
		})
	}
	// Hide local filesystem path from peer-facing tunnel-listener requests.
	if !isLoopback(r.RemoteAddr) {
		for i := range out {
			out[i].Path = ""
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListMyFiles(w http.ResponseWriter, r *http.Request) {
	// Same as handleListFiles but always includes Path (UI is on loopback).
	s.handleListFiles(w, r)
}

type addMyFileReq struct {
	Path string `json:"path"`
}

func (s *Server) handleAddMyFile(w http.ResponseWriter, r *http.Request) {
	var req addMyFileReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t := s.drop.Client()
	if t == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("torrent client not running"))
		return
	}
	h, err := t.AddSeed(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, fileEntry{
		Name: h.Name, Size: h.Size,
		InfoHash: h.InfoHash, Magnet: h.Magnet, Path: h.Path,
	})
}

// handleUploadMyFile saves an uploaded file into the shared directory
// and starts seeding it. Body is multipart/form-data with a single
// "file" part. Used by the UI's drag-and-drop area so the user doesn't
// have to type filesystem paths.
func (s *Server) handleUploadMyFile(w http.ResponseWriter, r *http.Request) {
	t := s.drop.Client()
	if t == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("torrent client not running"))
		return
	}
	// 8GiB cap on a single upload — generous, our overlay isn't a CDN.
	if err := r.ParseMultipartForm(8 << 30); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	defer file.Close()
	dstPath := filepath.Join(t.SharedDir(), filepath.Base(hdr.Filename))
	dst, err := os.Create(dstPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := io.Copy(dst, file); err != nil {
		dst.Close()
		os.Remove(dstPath)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	dst.Close()
	h, err := t.AddSeed(dstPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, fileEntry{
		Name: h.Name, Size: h.Size,
		InfoHash: h.InfoHash, Magnet: h.Magnet, Path: h.Path,
	})
}

func (s *Server) handleRemoveMyFile(w http.ResponseWriter, r *http.Request) {
	t := s.drop.Client()
	if t == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("torrent client not running"))
		return
	}
	if err := t.RemoveSeed(r.PathValue("hash")); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type addDownloadReq struct {
	Magnet string   `json:"magnet"`
	Peers  []string `json:"peers"`
}

func (s *Server) handleAddDownload(w http.ResponseWriter, r *http.Request) {
	var req addDownloadReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	d, err := s.drop.AddDownload(req.Magnet, req.Peers)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, fileEntry{
		Name: d.Name, Size: d.Size, InfoHash: d.InfoHash,
	})
}

func (s *Server) handleListDownloads(w http.ResponseWriter, r *http.Request) {
	t := s.drop.Client()
	if t == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("torrent client not running"))
		return
	}
	out := []map[string]any{}
	for _, d := range t.ListDownloads() {
		row := map[string]any{
			"info_hash":  d.InfoHash,
			"name":       d.Name,
			"size":       d.Size,
			"started_at": d.StartedAt.Unix(),
			// Source peers fed at AddDownload time. Used by the UI to
			// show "from peer-X" even before torrent metadata arrives,
			// so users can tell what they were trying to download.
			"peers": d.Peers,
		}
		// fetching_metadata = magnet added, but anacrolix hasn't pulled
		// the .torrent yet (source peer offline or didn't connect).
		// Without this the UI can't distinguish "active 0%" from "stuck
		// waiting on metadata forever" — shows up as just an info_hash.
		if d.Torrent == nil || d.Torrent.Info() == nil {
			row["fetching_metadata"] = true
		} else {
			info := d.Torrent.Info()
			total := info.TotalLength()
			done := d.Torrent.BytesCompleted()
			row["bytes_completed"] = done
			row["bytes_missing"] = total - done
			// Compare BytesCompleted to total length — robust check
			// that doesn't depend on BytesMissing's internal piece
			// bitmap state (which can briefly mis-report when a new
			// torrent is being added in parallel).
			complete := total > 0 && done >= total
			// t.Seeding() is anacrolix's own authority on "is this
			// torrent currently uploading without wanting anything".
			// Use it for the seeding flag rather than re-deriving.
			seeding := d.Torrent.Seeding() && complete
			row["complete"] = complete
			row["seeding"] = seeding
			if complete {
				row["path"] = t.DownloadPath(d)
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRemoveDownload cancels an in-progress download (or removes a
// completed one from seeding). info_hash in URL path. 204 on success.
// Files on disk are kept — anacrolix marks them as no longer managed
// but doesn't unlink. The engine's pruneLoop catches up later.
func (s *Server) handleRemoveDownload(w http.ResponseWriter, r *http.Request) {
	t := s.drop.Client()
	if t == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("torrent client not running"))
		return
	}
	hash := r.PathValue("hash")
	if hash == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("missing info_hash"))
		return
	}
	// Engine.RemoveDownload also drops the entry from downloads.json
	// so it doesn't come back on restart. It returns false for unknown
	// info_hashes — we still 204 to keep DELETE idempotent.
	_ = t
	s.drop.RemoveDownload(hash)
	w.WriteHeader(http.StatusNoContent)
}

// handleRevealFile runs `open -R <path>` so macOS selects the file in
// Finder. Restricted to paths under SharedDir or DownloadsDir — we
// don't want this endpoint to double as a generic "open anything on
// the filesystem" backdoor.
func (s *Server) handleRevealFile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	abs, err := filepath.Abs(body.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t := s.drop.Client()
	if t == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("torrent client not running"))
		return
	}
	shared, _ := filepath.Abs(t.SharedDir())
	downloads, _ := filepath.Abs(s.drop.DownloadsDir())
	if !strings.HasPrefix(abs, shared+string(filepath.Separator)) &&
		!strings.HasPrefix(abs, downloads+string(filepath.Separator)) {
		writeError(w, http.StatusForbidden, fmt.Errorf("path outside f2f-managed dirs"))
		return
	}
	if _, err := os.Stat(abs); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err := platform.RevealInFileManager(abs); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	return host == "127.0.0.1" || host == "::1"
}

type startRequest struct {
	CampName  string `json:"camp_name,omitempty"`
	CampID    string `json:"camp_id"`
	CampLabel string `json:"camp_label,omitempty"`
}

// handleStart accepts only camp identity from the UI now; everything else
// (utun addresses, UDP listen port, camp endpoint) uses sensible defaults
// that the engine fills in. Static-peer mode is no longer reachable from
// the UI — use the CLI if you need it.
//
// camp_name is optional when a per-camp config already exists on disk
// (the engine reads it from $HOME/.f2f/<camp_id>.config.json). It is
// required on the first start for a given camp_id.
//
// If the engine is already running with a different camp_id, we stop
// it first — gives the UI a one-click "switch camp" affordance.
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// Two flows in one endpoint:
	//   - join/resume: req.CampID populated (a known camp on disk or a
	//     full <pub>_<label> id pasted from an invite).
	//   - create: req.CampID empty, req.CampLabel populated → engine
	//     generates identity and derives ID = <pub>_<label>.
	// req.CampName is the user's nickname inside the camp either way.
	if req.CampID == "" && req.CampLabel == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("camp_id or camp_label is required"))
		return
	}
	if cur := s.engine.Status(); cur.Running {
		if req.CampID != "" && cur.CampID == req.CampID {
			writeJSON(w, http.StatusOK, s.statusWithDomains())
			return
		}
		if err := s.engine.Stop(); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("stop current: %w", err))
			return
		}
	}
	name := req.CampName
	if name == "" && req.CampID != "" {
		if existing, err := s.store.SnapshotCamp(req.CampID); err == nil && existing != nil {
			name = existing.Identity.Name
		}
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("camp_name required"))
		return
	}
	cfg := engine.Config{
		LocalIP:   "100.64.0.1", // placeholder; engine derives the real overlay-IP from pub on Start
		Listen:    ":9000",
		CampID:    req.CampID,
		CampName:  name,
		CampLabel: req.CampLabel,
	}
	if err := s.engine.Start(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.statusWithDomains())
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.Stop(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, s.statusWithDomains())
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
	info, err := s.tunnel.Add(req.Spec, req.Peer)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleRemoveIntercept(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.tunnel.Remove(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetActivePeer sets the user-selected active peer (catch-all
// tunnel destination and meet signalling target). Body: {pub}. Empty
// string clears.
func (s *Server) handleSetActivePeer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Pub string `json:"pub"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.engine.SetActivePeer(req.Pub); err != nil {
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

	ch, unsubscribe := clog.Subscribe(64)
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

// --- Group call (SFU) handlers ---

func (s *Server) handleCallState(w http.ResponseWriter, r *http.Request) {
	st := s.calls.LocalCall()
	if st == nil {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleCallList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.calls.AllCalls())
}

func (s *Server) handleCallCreate(w http.ResponseWriter, r *http.Request) {
	cs, err := s.calls.Create()
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, cs)
}

func (s *Server) handleCallJoin(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		TunnelIP string `json:"tunnel_ip"`
		Name     string `json:"name"`
		SFUHost  string `json:"sfu_host"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	st := s.engine.Status()
	// If the SFU host is remote, proxy the join request through the tunnel.
	if req.SFUHost != "" && req.SFUHost != st.LocalIP {
		// Can't host and join simultaneously — end any local call first.
		// Otherwise our browser receives SFU signals from both our local
		// SFU and the remote one, corrupting the single PeerConnection.
		s.calls.End()
		s.calls.SetJoinedSFUHost(req.SFUHost)
		s.proxyCallJoinToHost(w, req.SFUHost, req.Name)
		return
	}

	if err := s.calls.Join(req.TunnelIP, req.Name); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, s.calls.LocalCall())
}

func (s *Server) proxyCallJoinToHost(w http.ResponseWriter, sfuHost, name string) {
	_, port, _ := net.SplitHostPort(s.addr)
	if port == "" {
		port = "2202"
	}
	url := "http://" + net.JoinHostPort(sfuHost, port) + "/api/call/join"
	body, _ := json.Marshal(map[string]string{"name": name})
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("proxy join to %s: %w", sfuHost, err))
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// handleCallJoinRemote is exposed on the tunnel listener so a remote peer
// can register itself as a call participant.
func (s *Server) handleCallJoinRemote(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if host == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("cannot determine caller IP"))
		return
	}
	if err := s.calls.Join(host, req.Name); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, s.calls.LocalCall())
}

func (s *Server) handleCallLeave(w http.ResponseWriter, r *http.Request) {
	st := s.engine.Status()
	sfuHost := s.calls.JoinedSFUHost()

	// If we joined a remote SFU, proxy leave to the host.
	if sfuHost != "" && sfuHost != st.LocalIP {
		s.calls.ClearJoinedSFUHost()
		_, port, _ := net.SplitHostPort(s.addr)
		if port == "" {
			port = "2202"
		}
		url := "http://" + net.JoinHostPort(sfuHost, port) + "/api/call/leave"
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Post(url, "application/json", nil)
		if err == nil {
			resp.Body.Close()
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	s.calls.Leave(st.LocalIP)
	w.WriteHeader(http.StatusNoContent)
}

// handleCallLeaveRemote is on the tunnel listener — a remote peer leaves.
func (s *Server) handleCallLeaveRemote(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if host != "" {
		s.calls.Leave(host)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCallSignal receives WebRTC signals for the SFU. On the tunnel
// listener the sender is a remote peer; on the loopback listener it's
// the local browser. If the SFU is remote, the loopback handler proxies
// the signal to the SFU host through the tunnel.
func (s *Server) handleCallSignal(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	from := ""
	if host, _, splitErr := net.SplitHostPort(r.RemoteAddr); splitErr == nil {
		from = host
	}
	isLocal := from == "" || from == "127.0.0.1" || from == "::1"

	// If browser is sending a signal and the SFU is on a remote peer,
	// proxy the request there.
	if isLocal {
		st := s.engine.Status()
		sfuHost := s.calls.JoinedSFUHost()
		if sfuHost != "" && sfuHost != st.LocalIP {
			s.proxyCallSignalToHost(w, sfuHost, body)
			return
		}
		from = st.LocalIP
	}

	resp, err := s.calls.HandleSignal(from, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if resp != nil {
		// Don't broadcast to SSE — the browser gets the answer from the
		// POST response. SFU-initiated signals (renegotiation offers,
		// candidates) arrive via deliverSFUSignal → handleCallSignalInbound.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(resp)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) proxyCallSignalToHost(w http.ResponseWriter, sfuHost string, body []byte) {
	_, port, _ := net.SplitHostPort(s.addr)
	if port == "" {
		port = "2202"
	}
	url := "http://" + net.JoinHostPort(sfuHost, port) + "/api/call/signal"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("proxy signal to %s: %w", sfuHost, err))
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	// Don't broadcast the proxy response to SSE — the browser already
	// gets it from the POST response. SFU-initiated offers/candidates
	// arrive separately via deliverSFUSignal → handleCallSignalInbound.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// handleCallSignalInbound is registered on the tunnel listener. It handles:
//
//  1. Signals FROM the SFU (from: "sfu") → always broadcast to local browser SSE.
//     This covers both: SFU host receiving its own renegotiation offers, and
//     remote peers receiving SFU offers/candidates.
//  2. Signals FROM a remote browser (no from: "sfu") → dispatch to local SFU.
func (s *Server) handleCallSignalInbound(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// Check if this signal originates from the SFU itself.
	var peek struct {
		From string `json:"from"`
	}
	_ = json.Unmarshal(body, &peek)

	if peek.From == "sfu" {
		// SFU → browser: broadcast to local SSE so the browser can
		// process renegotiation offers and ICE candidates.
		s.callSignals.broadcast(body)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Browser/peer → SFU: dispatch to the local SFU engine.
	from := ""
	if host, _, splitErr := net.SplitHostPort(r.RemoteAddr); splitErr == nil {
		from = host
	}

	if s.calls.SFU() != nil {
		resp, err := s.calls.HandleSignal(from, body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if resp != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(resp)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCallSignalStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, unsubscribe := s.callSignals.subscribe()
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
