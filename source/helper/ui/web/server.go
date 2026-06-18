// Package web is the HTTP UI for f2f-mac. It serves an embedded SPA from
// assets/ and exposes a small REST + SSE API over the engine.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/vseplet/f2f/source/helper/db"
	"github.com/vseplet/f2f/source/helper/db/blocks"
	"github.com/vseplet/f2f/source/helper/db/blocks/channels"
	"github.com/vseplet/f2f/source/helper/db/blocks/message"
	"github.com/vseplet/f2f/source/helper/clog"
	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/identity"
	"github.com/vseplet/f2f/source/helper/mesh/bus"
	"github.com/vseplet/f2f/source/helper/mesh/camp"
	"github.com/vseplet/f2f/source/helper/mesh/engine"
	"github.com/vseplet/f2f/source/helper/mesh/gossip"
	"github.com/vseplet/f2f/source/helper/platform"
	"github.com/vseplet/f2f/source/helper/services/calls"
	"github.com/vseplet/f2f/source/helper/services/dns"
	"github.com/vseplet/f2f/source/helper/services/drop"
	"github.com/vseplet/f2f/source/helper/services/firewall"
	"github.com/vseplet/f2f/source/helper/services/notify"
	"github.com/vseplet/f2f/source/helper/services/oidc"
	"github.com/vseplet/f2f/source/helper/services/pki"
	"github.com/vseplet/f2f/source/helper/services/shell"
	"github.com/vseplet/f2f/source/helper/services/tunnel"
	"github.com/vseplet/f2f/source/helper/services/vnc"

	"github.com/go-webauthn/webauthn/webauthn"
)

//go:embed assets
var assetsFS embed.FS

// busTypeSignal is the bus message type for p2p (1:1) WebRTC call signalling.
// busTypeSignalNext is the namespaced name we're migrating to: accept both
// during the wire rollout, keep requesting the old one, flip later.
const (
	busTypeSignal     = "signal"
	busTypeSignalNext = "call.p2p"
)

// Server wraps an Engine with an HTTP handler on the user-facing
// loopback bind (the full UI + all API endpoints). Peer↔peer traffic
// does not pass through here — it rides the QUIC bus (RegisterBus).
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
	db       *db.Service
	notify   *notify.Service
	gossip   *gossip.Service
	shell    *shell.Service
	vnc      *vnc.Service
	oidc     *oidc.Service
	blocks   *blocks.Manager
	channels *channels.Manager
	messages *message.Manager
	addr     string
	srv      *http.Server

	signals     *signalHub
	callSignals *signalHub   // SSE hub for SFU signals → local browser
	blockEvents *signalHub    // SSE hub: a block scope changed (remote sync) → browser
	msgEvents  *signalHub    // SSE hub: a new/edited chat message → browser (Message JSON)
	bus         *bus.Service // peer↔peer transport; nil until RegisterBus

	// profRegSess holds in-flight profile passkey registration ceremonies
	// (pub → WebAuthn session). Separate from OIDC's credstore: the profile
	// passkey's public credential lands in block.profile (synced), not in the
	// OIDC-local passkeys.json.
	profRegMu   sync.Mutex
	profRegSess map[string]*webauthn.SessionData
}

func New(eng *engine.Engine, store *config.Store, fwSvc *firewall.Service, pkiSvc *pki.Service, dnsSvc *dns.Service, dropSvc *drop.Service, callsSvc *calls.Service, tunnelSvc *tunnel.Service, campSvc *camp.Service, dbSvc *db.Service, notifySvc *notify.Service, gossipSvc *gossip.Service, shellSvc *shell.Service, vncSvc *vnc.Service, oidcSvc *oidc.Service, blocksMgr *blocks.Manager, channelsMgr *channels.Manager, messagesMgr *message.Manager, addr string) *Server {
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
		db:          dbSvc,
		notify:      notifySvc,
		gossip:      gossipSvc,
		shell:       shellSvc,
		vnc:         vncSvc,
		oidc:        oidcSvc,
		blocks:      blocksMgr,
		channels:    channelsMgr,
		messages:    messagesMgr,
		addr:        addr,
		signals:     newSignalHub(),
		callSignals: newSignalHub(),
		blockEvents: newSignalHub(),
		msgEvents:  newSignalHub(),
		profRegSess: make(map[string]*webauthn.SessionData),
	}
	callsSvc.OnLocalSignal = func(msg []byte) {
		s.callSignals.broadcast(msg)
	}
	return s
}

// Addr returns the configured bind address.
func (s *Server) Addr() string { return s.addr }

// RegisterBus wires the server to the QUIC bus: inbound meet
// signalling arrives as "signal" messages (instead of the tunnel
// listener's POST /api/signal/inbox), and outbound signalling /
// call proxying prefer the bus over HTTP. Call once from main.
func (s *Server) RegisterBus(b *bus.Service) {
	s.bus = b
	signalHandler := func(fromPub string, payload []byte) ([]byte, error) {
		// Auto-select the sender as active peer — same convenience as
		// handleSignalInbox, but keyed by the bus-attested pub instead
		// of the RemoteAddr overlay IP.
		st := s.engine.Status()
		if st.Running && fromPub != "" && fromPub != st.ActivePeerPub {
			for _, p := range st.Peers {
				if !p.Self && p.Pub == fromPub {
					_ = s.engine.SetActivePeer(fromPub)
					break
				}
			}
		}
		// A fresh offer is the start of a new p2p call — ring it as a
		// notification. ICE-restart offers carry no "fresh" flag, so they
		// don't re-ring an established call.
		if s.notify != nil && fromPub != "" {
			var sig struct {
				Kind  string `json:"kind"`
				Fresh bool   `json:"fresh"`
			}
			if json.Unmarshal(payload, &sig) == nil && sig.Kind == "offer" && sig.Fresh {
				s.notify.Push(notify.Notification{
					Kind:  "call",
					Title: s.peerName(fromPub) + " is calling",
					From:  fromPub,
					Route: "channel:" + fromPub, // a DM is the degenerate channel
				})
			}
		}
		s.signals.broadcast(payload)
		return nil, nil
	}
	b.Handle(busTypeSignal, signalHandler)
	b.Handle(busTypeSignalNext, signalHandler) // accept the new name during rollout
}

// peerName resolves a peer pub to its display name from the engine roster,
// falling back to a short fingerprint. Used to title call notifications.
func (s *Server) peerName(pub string) string {
	for _, p := range s.engine.Status().Peers {
		if p.Pub == pub {
			if p.Name != "" {
				return p.Name
			}
			break
		}
	}
	if len(pub) > 12 {
		return pub[:12]
	}
	return pub
}

// profileFullName returns the peer's first+last from its block.profile (scope
// "profiles", synced), or "" if there's no profile/name. Profiles replicate to
// every camp member, so any peer can name any author it sees.
func (s *Server) profileFullName(pub string) string {
	if pub == "" {
		return ""
	}
	c := s.loadProfileContent(pub)
	return strings.TrimSpace(c.First + " " + c.Last)
}

// authorName resolves a peer pub to its display name for chat: the profile's
// full name when set, else the roster/peer name.
func (s *Server) authorName(pub string) string {
	if n := s.profileFullName(pub); n != "" {
		return n
	}
	return s.peerName(pub)
}

// pubForOverlayIP resolves a peer's overlay v4 to its pub via the
// engine roster (empty if unknown). Used to address bus messages when
// the caller only has the peer's tunnel IP.
func (s *Server) pubForOverlayIP(ip string) string {
	if ip == "" {
		return ""
	}
	for _, p := range s.engine.Status().Peers {
		if !p.Self && p.OverlayV4 == ip {
			return p.Pub
		}
	}
	return ""
}

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
	if devDir := os.Getenv("F2F_DEV_ASSETS"); devDir != "" {
		clog.Info("web", "serving assets from disk (F2F_DEV_ASSETS=%s)", devDir)
		mux.Handle("/", http.FileServer(http.Dir(devDir)))
	} else {
		sub, err := fs.Sub(assetsFS, "assets")
		if err != nil {
			panic(err) // build-time; embed is wrong
		}
		mux.Handle("/", http.FileServer(http.FS(sub)))
	}

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/intercepts", s.handleAddIntercept)
	mux.HandleFunc("DELETE /api/intercepts/{id}", s.handleRemoveIntercept)
	mux.HandleFunc("POST /api/peers/active", s.handleSetActivePeer)
	mux.HandleFunc("DELETE /api/peers/{pub}", s.handleForgetPeer)
	mux.HandleFunc("GET /api/topology", s.handleTopology)
	mux.HandleFunc("GET /api/log/stream", s.handleLogStream)
	mux.HandleFunc("GET /api/oidc", s.handleOIDCInfo)
	mux.HandleFunc("POST /api/oidc/clients", s.handleOIDCCreateClient)
	mux.HandleFunc("DELETE /api/oidc/clients/{id}", s.handleOIDCDeleteClient)
	// Notes are generic blocks (text) in a "note:<conv>" scope — the block
	// engine surfaced under a meaningful entity name. No public /api/blocks.
	mux.HandleFunc("GET /api/profile", s.handleProfileGet)
	mux.HandleFunc("POST /api/profile", s.handleProfileSave)
	mux.HandleFunc("POST /api/profile/device", s.handleDeviceRename)
	mux.HandleFunc("POST /api/profile/passkey/begin", s.handlePasskeyBegin)
	mux.HandleFunc("POST /api/profile/passkey/finish", s.handlePasskeyFinish)
	mux.HandleFunc("GET /api/notes/scope", s.handleNotesScope)
	mux.HandleFunc("GET /api/notes", s.handleNotesList)
	mux.HandleFunc("POST /api/notes", s.handleNotesCreate)
	mux.HandleFunc("POST /api/notes/update", s.handleNotesUpdate)
	mux.HandleFunc("POST /api/notes/move", s.handleNotesMove)
	mux.HandleFunc("POST /api/notes/delete", s.handleNotesDelete)
	mux.HandleFunc("POST /api/notes/merge", s.handleNotesMerge)
	mux.HandleFunc("POST /api/notes/attach", s.handleNotesAttach)
	mux.HandleFunc("POST /api/notes/share", s.handleNotesShare)
	// Channels (channel blocks) and messages (message blocks) — per-entity API.
	mux.HandleFunc("GET /api/channels", s.handleChannelsList)
	mux.HandleFunc("POST /api/channels", s.handleChannelsCreate)
	mux.HandleFunc("POST /api/channels/rename", s.handleChannelsRename)
	mux.HandleFunc("POST /api/channels/members", s.handleChannelsMembers)
	mux.HandleFunc("POST /api/channels/delete", s.handleChannelsDelete)
	mux.HandleFunc("POST /api/channels/dm", s.handleChannelsDM)
	mux.HandleFunc("GET /api/messages", s.handleMessagesList)
	mux.HandleFunc("POST /api/messages", s.handleMessagesPost)
	mux.HandleFunc("POST /api/messages/share", s.handleMessagesShare)
	mux.HandleFunc("POST /api/messages/clear", s.handleMessagesClear)
	mux.HandleFunc("GET /api/events", s.handleEventStream)
	mux.HandleFunc("POST /api/db/query", s.handleDBQuery)
	mux.HandleFunc("GET /api/camp/peers", s.handleCampPeers)

	// Remote terminal (services/shell over the bus). /peers lists camp peers
	// whose shell is open to us; /ws bridges a browser xterm.js to a bus
	// stream. Loopback-only — never exposed on the tunnel listener.
	mux.HandleFunc("GET /api/shell/peers", s.handleShellPeers)
	mux.HandleFunc("GET /api/shell/ws", s.handleShellWS)

	// Remote desktop (services/vnc over the bus). /peers lists camp peers
	// with a reachable VNC server; /ws bridges a browser noVNC to a bus
	// stream. Loopback-only.
	mux.HandleFunc("GET /api/vnc/peers", s.handleVncPeers)
	mux.HandleFunc("GET /api/vnc/ws", s.handleVncWS)

	// Local DNS / domain management. /api/my-domains is the UI side
	// (read/write our own list); peers pull the catalog over the bus
	// ("domains" message type), /api/domains stays as a debug aid.
	mux.HandleFunc("GET /api/my-domains", s.handleListMyDomains)
	mux.HandleFunc("PUT /api/my-domains", s.handleSetMyDomains)
	mux.HandleFunc("GET /api/domains", s.handleListDomains)
	mux.HandleFunc("DELETE /api/peer-domains/{peer}/{name}", s.handleRemovePeerDomain)
	mux.HandleFunc("GET /api/ca-cert", s.handleCACert)
	mux.HandleFunc("GET /api/my-ca", s.handleMyCA)
	mux.HandleFunc("GET /api/trusted-peers", s.handleTrustedPeers)
	mux.HandleFunc("POST /api/trusted-peers/{fp}/install", s.handleInstallTrustedPeer)
	mux.HandleFunc("DELETE /api/trusted-peers/{fp}", s.handleRemoveTrustedPeer)

	// Firewall: default-deny inbound on utun + user-configurable
	// allow list (in addition to f2f's own ports). Loopback-only,
	// settings for *this* peer.
	mux.HandleFunc("GET /api/firewall", s.handleListFirewall)
	mux.HandleFunc("PUT /api/firewall", s.handleSetFirewall)

	// File sharing via BitTorrent (camp-only, no DHT/public trackers).
	// /api/files/mine — UI-facing CRUD for what we publish.
	// /api/files — read-only listing; peers browse our catalog over
	// the bus ("files" message type).
	mux.HandleFunc("GET /api/files/mine", s.handleListMyFiles)
	mux.HandleFunc("POST /api/files/mine", s.handleAddMyFile)
	mux.HandleFunc("POST /api/files/mine/upload", s.handleUploadMyFile)
	mux.HandleFunc("DELETE /api/files/mine/{hash}", s.handleRemoveMyFile)
	mux.HandleFunc("POST /api/files/download", s.handleAddDownload)
	mux.HandleFunc("GET /api/files/downloads", s.handleListDownloads)
	mux.HandleFunc("DELETE /api/files/downloads/{hash}", s.handleRemoveDownload)
	mux.HandleFunc("POST /api/files/reveal", s.handleRevealFile)
	mux.HandleFunc("GET /api/files", s.handleListFiles)

	// WebRTC signalling: outbox = browser → peer (forwarded over the
	// bus), stream = us → browser; peer → us arrives as a "signal" bus
	// message (RegisterBus). Wire-format is opaque JSON blobs forwarded
	// verbatim; only the browsers care about their contents.
	mux.HandleFunc("POST /api/signal/outbox", s.handleSignalOutbox)
	mux.HandleFunc("GET /api/signal/stream", s.handleSignalStream)

	// Notifications: recent list + live SSE stream for the UI.
	mux.HandleFunc("GET /api/notifications", s.handleNotifications)
	mux.HandleFunc("DELETE /api/notifications", s.handleClearNotifications)
	mux.HandleFunc("GET /api/notifications/stream", s.handleNotificationsStream)

	// Mesh: fabric-level NodeStates (platform + peer-view) replicated via gossip.
	mux.HandleFunc("GET /api/mesh", s.handleMesh)

	// Group calls (SFU-based). Browser-facing endpoints only; remote
	// peers deliver SFU signals over the bus (call.* message types).
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
// browser and forwards it as-is to the peer over the bus ("signal"
// message type).
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
	var peerPub string
	for _, p := range st.Peers {
		if p.Self || p.Pub == "" {
			continue
		}
		if (toPub != "" && p.Pub == toPub) || (toPub == "" && p.Pub == st.ActivePeerPub) {
			peerPub = p.Pub
			break
		}
	}
	if peerPub == "" {
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
	if s.bus == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bus not running"))
		return
	}
	rctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	_, busErr := s.bus.Request(rctx, peerPub, busTypeSignal, body)
	cancel()
	if busErr != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("forward to peer: %w", busErr))
		return
	}
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

// handleClearNotifications drops every notification in the active camp.
func (s *Server) handleClearNotifications(w http.ResponseWriter, r *http.Request) {
	if err := s.notify.Clear(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	ProfileName string            `json:"profile_name,omitempty"` // first+last from the peer's block.profile, "" if none
	Domains     []dns.Entry       `json:"domains,omitempty"`
	Files       []drop.PeerFile   `json:"files,omitempty"`
	Firewall    []config.Firewall `json:"firewall,omitempty"`
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
	// mesh/camp; web merges them into /api/status so the UI
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
		v := peerStatusView{PeerStatusInfo: p, ProfileName: s.profileFullName(p.Pub)}
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
	if gs, err := s.store.LoadState(); err == nil && gs != nil {
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
// Loopback-only; the peer-facing catalog (without Path) is served by
// the drop service's "files" bus handler.
type fileEntry struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	InfoHash string `json:"info_hash"`
	Magnet   string `json:"magnet"`
	Path     string `json:"path,omitempty"`
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
			Path: h.Path,
		})
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
			// Whether the source peer is reachable right now: lets the UI
			// say "source offline" instead of a hopeful "fetching…" when
			// nobody can serve the metadata.
			row["source_online"] = s.drop.SourceOnline(d.InfoHash)
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
		Path     string `json:"path"`
		InfoHash string `json:"info_hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// Reveal by info_hash (seeding or downloaded file): resolve its local path.
	if body.Path == "" && body.InfoHash != "" {
		if p := s.drop.PathForInfoHash(body.InfoHash); p != "" {
			body.Path = p
		} else {
			writeError(w, http.StatusNotFound, fmt.Errorf("file not present locally"))
			return
		}
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

// Camp lifecycle (start / stop / create / switch) is owned by package
// cli now — the web UI is read-only for camps. The old POST /api/start
// and /api/stop endpoints were removed with the SPA's camp switcher.

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

// handleForgetPeer drops a peer from the live map and the persisted camp
// catalog. Intended for offline ghosts the user wants off the list — the
// camp only re-sends a peer once it's active again, so a forgotten offline
// peer stays gone. 204 on success; 404 if it wasn't there.
func (s *Server) handleForgetPeer(w http.ResponseWriter, r *http.Request) {
	pub := r.PathValue("pub")
	if pub == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("missing pub"))
		return
	}
	found := s.engine.ForgetPeer(pub)
	if campID := s.engine.Status().CampID; campID != "" {
		_ = s.store.UpdateCamp(campID, func(c *config.Camp) {
			kept := c.PeerCatalog[:0]
			for _, p := range c.PeerCatalog {
				if p.Pub != pub {
					kept = append(kept, p)
				}
			}
			c.PeerCatalog = kept
		})
	}
	if !found {
		w.WriteHeader(http.StatusNotFound)
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
	// Optional body: {channel} — binds the call to a messenger channel so
	// its id is "<channel>/<initiator_pub>" and joiners match on it.
	var req struct {
		Channel string `json:"channel"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // empty body = unbound call
	cs, err := s.calls.Create(req.Channel)
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
	pub := s.pubForOverlayIP(sfuHost)
	if s.bus == nil || pub == "" {
		writeError(w, http.StatusBadGateway, fmt.Errorf("proxy join: no bus route to %s", sfuHost))
		return
	}
	body, _ := json.Marshal(map[string]string{"name": name})
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	respBody, err := s.bus.Request(rctx, pub, calls.BusTypeJoin, body)
	cancel()
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("proxy join to %s: %w", sfuHost, err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(respBody)
}

func (s *Server) handleCallLeave(w http.ResponseWriter, r *http.Request) {
	st := s.engine.Status()
	sfuHost := s.calls.JoinedSFUHost()

	// If we joined a remote SFU, proxy leave to the host over the bus.
	if sfuHost != "" && sfuHost != st.LocalIP {
		s.calls.ClearJoinedSFUHost()
		if s.bus != nil {
			if pub := s.pubForOverlayIP(sfuHost); pub != "" {
				rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if _, err := s.bus.Request(rctx, pub, calls.BusTypeLeave, nil); err != nil {
					clog.Warn("call", "leave %s: %v", sfuHost, err)
				}
				cancel()
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	s.calls.Leave(st.LocalIP)
	w.WriteHeader(http.StatusNoContent)
}

// handleCallSignal receives WebRTC signals for the SFU from the local
// browser. If the SFU is remote, the signal is proxied to the SFU host
// over the bus; peer-originated signals arrive via the calls service's
// call.signal bus handler instead.
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
	// Don't broadcast the proxy response to SSE — the browser already
	// gets it from the POST response. SFU-initiated offers/candidates
	// arrive separately via deliverSignal → calls.BusTypeSignal.
	pub := s.pubForOverlayIP(sfuHost)
	if s.bus == nil || pub == "" {
		writeError(w, http.StatusBadGateway, fmt.Errorf("proxy signal: no bus route to %s", sfuHost))
		return
	}
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	respBody, err := s.bus.Request(rctx, pub, calls.BusTypeSignal, body)
	cancel()
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("proxy signal to %s: %w", sfuHost, err))
		return
	}
	if len(respBody) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(respBody)
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
