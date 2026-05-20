//go:build darwin

// Package engine owns the tunnel runtime: utun, UDP, routes, and (optionally)
// egress NAT setup. It exposes a Start/Stop lifecycle plus methods to mutate
// the intercept list while running, so that both the CLI and the web UI can
// drive the same core.
package engine

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vseplet/f2f/source/mac/internal/ca"
	internaldns "github.com/vseplet/f2f/source/mac/internal/dns"
	"github.com/vseplet/f2f/source/mac/internal/egress"
	"github.com/vseplet/f2f/source/mac/internal/keychain"
	"github.com/vseplet/f2f/source/mac/internal/packet"
	"github.com/vseplet/f2f/source/mac/internal/rendezvous"
	"github.com/vseplet/f2f/source/mac/internal/route"
	"github.com/vseplet/f2f/source/mac/internal/tunnel"
)

// tunnelSubnetCIDR is the /24 every camp lives in. Hardcoded — camp's
// hub uses the same prefix when allocating tunnel_ips.
const tunnelSubnetCIDR = "10.99.0.0/24"

// CampConfig points the engine at a rendezvous (camp) server: instead of
// the user supplying the peer's UDP endpoint via --peer, we discover our
// own external endpoint via STUN, register with camp under (Name, ID),
// and adopt the other peer in the same camp when it announces an endpoint.
type CampConfig struct {
	URL      string // wss://f2f-camp.fly.dev/ws
	Name     string // our identity within the camp
	ID       string // shared camp id (was previously called "room")
	StunAddr string // host:port for the UDP STUN probe (e.g. f2f-camp.fly.dev:3478)
}

// Config is the input to Start.
type Config struct {
	LocalIP      string      // utun local point-to-point address
	PeerIP       string      // utun remote point-to-point address (static mode only)
	Listen       string      // UDP listen address (":9000"), empty = no peer mode
	Peer         string      // UDP peer address ("host:9000"); ignored when Camp is set
	EgressIface  string      // physical interface for NAT; empty = auto-detect default route
	Camp         *CampConfig // optional: use a rendezvous server instead of static Peer
}

// Status is a point-in-time snapshot. It is computed; the underlying state
// changes between calls.
type Status struct {
	Running      bool   `json:"running"`
	UtunName     string `json:"utun_name,omitempty"`
	LocalIP      string `json:"local_ip,omitempty"`
	PeerIP       string `json:"peer_ip,omitempty"`         // active peer's tunnel_ip (camp mode) or static peer (legacy)
	ListenAddr   string `json:"listen_addr,omitempty"`
	PeerAddr     string `json:"peer_addr,omitempty"`       // active peer's UDP endpoint
	EgressActive bool   `json:"egress_active"`
	EgressIface  string `json:"egress_iface,omitempty"`
	EgressAnchor string `json:"egress_anchor,omitempty"`
	CampActive   bool   `json:"camp_active"`
	CampURL      string `json:"camp_url,omitempty"`
	CampName     string `json:"camp_name,omitempty"`
	CampID       string `json:"camp_id,omitempty"`
	CampPeerName string `json:"camp_peer_name,omitempty"` // active peer's name (display alias)
	CampReflex   string `json:"camp_reflex,omitempty"`    // our own external endpoint per STUN
	// ActivePeerTunnelIP is the user-selected peer the tunnel routes
	// catch-all traffic through. Empty when no one has been selected.
	ActivePeerTunnelIP string             `json:"active_peer_tunnel_ip,omitempty"`
	Peers              []PeerStatusInfo   `json:"peers"`
	Intercepts         []InterceptInfo    `json:"intercepts"`
	StartedAt          time.Time          `json:"started_at,omitempty"`
	TxBytes            uint64             `json:"tx_bytes"`
	RxBytes            uint64             `json:"rx_bytes"`
	TxPackets          uint64             `json:"tx_packets"`
	RxPackets          uint64             `json:"rx_packets"`
}

// PeerStatusInfo augments rendezvous.PeerInfo with our local reachability
// view: when we last received UDP from this peer, and whether it counts
// as "reachable" right now (within 30s window). One synthetic entry
// with Self=true represents us so the UI can render a single uniform
// table.
type PeerStatusInfo struct {
	Name        string        `json:"name"`
	TunnelIP    string        `json:"tunnel_ip"`
	PublicIP    string        `json:"public_ip,omitempty"`
	UDPPort     int           `json:"udp_port,omitempty"`
	UDPEndpoint string        `json:"udp_endpoint,omitempty"`
	JoinedAt    int64         `json:"joined_at,omitempty"`
	LastSeenMs  int64         `json:"last_seen_ms,omitempty"` // ms since last packet; 0 = never
	Online      bool          `json:"online"`                 // camp-side: announced recently
	Reachable   bool          `json:"reachable"`              // local: receiving UDP from this peer
	Active      bool          `json:"active"`
	Self        bool          `json:"self,omitempty"`
	Domains     []DomainEntry `json:"domains,omitempty"`
}

// InterceptInfo describes one intercept entry, what host routes it owns,
// and which peer its traffic is routed through. Peer is the peer's name
// in the camp; route lookups translate it to the current UDPAddr at
// send time.
type InterceptInfo struct {
	ID       string   `json:"id"`
	Spec     string   `json:"spec"`
	Peer     string   `json:"peer"`
	Prefixes []string `json:"prefixes"`
}

// DomainEntry is one (name, port, proto) record this engine — or another
// peer — publishes inside the camp's <camp_id>.f2f zone. Port and proto
// are advisory: DNS only carries the IP, the user types the port in
// their URLs. Name is the short label (e.g. "gitlab") and gets the
// camp-wide TLD appended at resolution time.
type DomainEntry struct {
	Name  string `json:"name"`
	Port  int    `json:"port,omitempty"`
	Proto string `json:"proto,omitempty"`
}

// peerState is our per-peer view: identity from camp + when we last
// received UDP from this peer. LastSeenMs starts at 0 and gets updated
// every time peerToTunLoop sees a packet whose source matches us.
// holePunchLoop reads LastSeenMs to decide between burst (1Hz) and
// keepalive (~25s) cadence.
//
// Online mirrors camp's view (announcing recently). Offline peers stay
// in the map so intercept bindings survive and the UI keeps showing
// them; their UDPAddr just stops getting refreshed.
//
// Domains is populated by domainPollLoop polling each peer's
// /api/domains over the tunnel. Stale (failed poll) → cleared; offline
// (camp says) → kept stale until next refresh.
type peerState struct {
	Name        string
	TunnelIP    string
	PublicIP    string
	UDPPort     int
	UDPEndpoint string
	JoinedAt    int64
	Online      bool
	LastSeenAt  int64
	Domains     []DomainEntry

	UDPAddr      *net.UDPAddr // current best-known UDP target (port can shift on NAT rebind)
	LastSeenMs   atomic.Int64 // epoch ms of last received packet from this peer; 0 = never
	LastPingMs   atomic.Int64 // epoch ms of last punch/keepalive we sent
}

// Engine is the long-lived tunnel runtime.
type Engine struct {
	mu sync.Mutex

	running bool
	cfg     Config

	tun      *tunnel.Tunnel
	udp      *net.UDPConn
	routes   *route.Manager
	egr      *egress.Egress
	dnsSrv   *internaldns.Server // local DNS for <camp_id>.f2f
	ca       *ca.CA              // local CA for the current camp_id
	announce *rendezvous.AnnounceClient // periodic UDP announce → camp
	// campAddr is the resolved UDP endpoint of the camp server. The main
	// read loop checks each incoming packet's source against this so we
	// can dispatch announce replies before they hit IP-version parsing.
	campAddr atomic.Pointer[net.UDPAddr]
	// campReflex is display-only.
	campReflex atomic.Pointer[string]
	// campPeers holds the latest peer-list snapshot from the camp HTTP
	// poller. The UI reads from this cache via /api/camp/peers so each
	// browser refresh costs zero camp requests.
	campPeers atomic.Pointer[[]rendezvous.PeerInfo]

	// peers tracks every peer we currently know about (via camp poll or
	// static config). Keyed by tunnel_ip. We send periodic hole-punch
	// pings to all of them so NAT mappings stay open for fast active-peer
	// switching. Protected by mu.
	peers map[string]*peerState
	// activeTunnelIP is the user-selected peer the tunnel routes
	// catch-all traffic through. Direct peer-to-peer-tunnel-IP packets
	// still flow regardless of selection.
	activeTunnelIP atomic.Pointer[string]
	// staticPeer is the legacy --peer mode endpoint (no camp). Kept for
	// backwards compat with the few static deployments; new code paths
	// should use the peers map.
	staticPeer       atomic.Pointer[net.UDPAddr]
	lastStaticPingMs atomic.Int64

	intercepts map[string]*InterceptInfo
	nextItemID uint64

	// myDomains is the list this peer publishes to others. Read by
	// /api/domains on the tunnel listener; written by the UI via
	// SetMyDomains. Atomic pointer keeps the read path lock-free.
	myDomains atomic.Pointer[[]DomainEntry]
	// tunnelHTTPPort is the port other peers expose their /api/domains
	// on (= our UI bind port, since both sides run f2f-mac). Wired by
	// main via SetTunnelHTTPPort.
	tunnelHTTPPort string

	// trustedPeerCAs tracks which peer CAs we've already installed in
	// the system keychain (by SHA-256 fingerprint of the cert). Avoids
	// repeated `security add-trusted-cert` calls (each one popping a
	// password prompt). Key is fingerprint (hex), value is metadata.
	trustedPeerCAs   map[string]TrustedPeerCA
	trustedPeerCAsMu sync.Mutex

	cancel  context.CancelFunc
	workers sync.WaitGroup
	started time.Time

	txBytes, rxBytes     atomic.Uint64
	txPackets, rxPackets atomic.Uint64

	tap *logTap

	// Hooks let the surrounding process (currently web.Server) react to
	// engine lifecycle without engine importing web. OnStarted fires
	// after utun + UDP are up and LocalIP is finalised; OnStopped fires
	// after Stop tears everything down.
	OnStarted func(localIP string)
	OnStopped func()
}

// TrustedPeerCA is one row in the UI's trusted-peers panel.
type TrustedPeerCA struct {
	PeerName    string `json:"peer_name"`
	CommonName  string `json:"common_name"`
	Fingerprint string `json:"fingerprint"`
	InstalledAt int64  `json:"installed_at"`
}

// New returns a fresh Engine. Start it to bring it up.
func New() *Engine {
	return &Engine{
		intercepts:     map[string]*InterceptInfo{},
		peers:          map[string]*peerState{},
		trustedPeerCAs: map[string]TrustedPeerCA{},
		tap:            newLogTap(),
	}
}

// TrustedPeerCAs returns a snapshot for the UI panel.
func (e *Engine) TrustedPeerCAs() []TrustedPeerCA {
	e.trustedPeerCAsMu.Lock()
	defer e.trustedPeerCAsMu.Unlock()
	out := make([]TrustedPeerCA, 0, len(e.trustedPeerCAs))
	for _, t := range e.trustedPeerCAs {
		out = append(out, t)
	}
	return out
}

// LogTap returns the io.Writer that should be added to log output so that
// in-engine messages are broadcast to subscribers. Callers typically wire
// it via log.SetOutput(io.MultiWriter(os.Stderr, e.LogTap())).
func (e *Engine) LogTap() *logTap { return e.tap }

// Subscribe returns a channel of log entries plus a cancel function.
func (e *Engine) Subscribe(buf int) (<-chan LogEntry, func()) {
	return e.tap.Subscribe(buf)
}

// Start brings the engine up with cfg. Returns an error if already running
// or if any setup step fails (in which case partial state has been rolled
// back).
func (e *Engine) Start(cfg Config) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.running {
		return errors.New("engine already running")
	}
	if cfg.Camp != nil {
		// With Camp, peer is auto-discovered; we still need a UDP socket
		// to receive on.
		if cfg.Listen == "" {
			return errors.New("Camp mode requires Listen")
		}
		if cfg.Camp.URL == "" || cfg.Camp.Name == "" || cfg.Camp.ID == "" || cfg.Camp.StunAddr == "" {
			return errors.New("Camp.{URL,Name,ID,StunAddr} all required")
		}
	} else if (cfg.Listen == "") != (cfg.Peer == "") {
		return errors.New("Listen and Peer must both be set or both be empty")
	}

	// Egress goes first so its rollback runs last on the way down.
	// Empty EgressIface means: auto-pick the default route's interface.
	// We always run egress in camp mode — the tunnel is useless without
	// a path to the internet.
	if cfg.EgressIface == "" {
		cfg.EgressIface = detectDefaultRouteIface()
	}
	if cfg.EgressIface != "" {
		subnet := netip.MustParsePrefix(tunnelSubnetCIDR)
		egr, err := egress.Open(cfg.EgressIface, subnet)
		if err != nil {
			return fmt.Errorf("egress setup: %w", err)
		}
		e.egr = egr
		log.Printf("egress: NAT %s → %s via pf anchor %q, ip.forwarding=1",
			subnet, cfg.EgressIface, egr.Anchor())
	} else {
		log.Printf("egress: could not detect default route iface; skipping NAT (peers won't reach internet through this node)")
	}

	// UDP socket.
	if cfg.Listen != "" {
		laddr, err := net.ResolveUDPAddr("udp", cfg.Listen)
		if err != nil {
			e.rollbackPartial()
			return fmt.Errorf("resolve listen: %w", err)
		}
		udp, err := net.ListenUDP("udp", laddr)
		if err != nil {
			e.rollbackPartial()
			return fmt.Errorf("listen udp: %w", err)
		}
		e.udp = udp
		// In Camp mode, peerPtr starts nil — campLoop adopts the peer
		// when it announces an endpoint. In static mode, resolve now.
		if cfg.Camp == nil && cfg.Peer != "" {
			initialPeer, err := net.ResolveUDPAddr("udp", cfg.Peer)
			if err != nil {
				e.rollbackPartial()
				return fmt.Errorf("resolve peer: %w", err)
			}
			e.staticPeer.Store(initialPeer)
		}
	}

	// Camp: announce ourselves over UDP on the same socket the tunnel
	// uses. The reply carries our camp-assigned tunnel IP (which utun
	// will be opened with below) and our public endpoint as observed
	// by camp — that doubles as STUN. The peer list comes from the
	// periodic HTTP poller started further down.
	var (
		localIP = cfg.LocalIP
		peerIP  = cfg.PeerIP
	)
	if cfg.Camp != nil {
		ac, err := rendezvous.NewAnnounceClient(e.udp, cfg.Camp.StunAddr, cfg.Camp.Name, cfg.Camp.ID)
		if err != nil {
			e.rollbackPartial()
			return fmt.Errorf("camp announce client: %w", err)
		}
		self, err := ac.AnnounceOnce(5 * time.Second)
		if err != nil {
			e.rollbackPartial()
			return fmt.Errorf("camp announce: %w", err)
		}
		if self.TunnelIP == "" {
			e.rollbackPartial()
			return fmt.Errorf("camp announce reply did not include tunnel_ip")
		}
		e.announce = ac
		e.campAddr.Store(ac.CampAddr())
		reflex := self.UDPEndpoint
		if reflex == "" {
			reflex = self.PublicIP
		}
		e.campReflex.Store(&reflex)
		localIP = self.TunnelIP
		log.Printf("camp: registered as %s in camp %s, tunnel_ip=%s reflex=%s", cfg.Camp.Name, cfg.Camp.ID, localIP, reflex)
	}

	// utun. In Camp mode the interface owns the whole 10.99.0.0/24
	// overlay; static mode keeps the legacy point-to-point form.
	var tun *tunnel.Tunnel
	if cfg.Camp != nil {
		t, err := tunnel.OpenSubnet(localIP, 24)
		if err != nil {
			e.rollbackPartial()
			return fmt.Errorf("open tunnel: %w", err)
		}
		tun = t
		log.Printf("opened %s (subnet=%s/24 mtu=%d)", tun.Name(), localIP, tunnel.MTU)
	} else {
		t, err := tunnel.Open(localIP, peerIP)
		if err != nil {
			e.rollbackPartial()
			return fmt.Errorf("open tunnel: %w", err)
		}
		tun = t
		log.Printf("opened %s (local=%s peer=%s mtu=%d)", tun.Name(), localIP, peerIP, tunnel.MTU)
	}
	e.tun = tun
	// Reflect the actual addresses we ended up using back into the
	// stored config so Status() shows ground truth, not user intent.
	cfg.LocalIP = localIP
	if cfg.Camp == nil {
		cfg.PeerIP = peerIP
	}
	if e.udp != nil {
		log.Printf("UDP listening on %s", e.udp.LocalAddr())
	}

	// Route table is empty at start; intercepts are added via UI / API
	// once peers are visible (peer assignment is mandatory).
	e.routes = route.New(tun.Name())

	// Workers.
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.started = time.Now()
	e.cfg = cfg
	e.running = true

	e.workers.Add(1)
	go e.tunToPeerLoop(ctx)
	if e.udp != nil {
		e.workers.Add(1)
		go e.peerToTunLoop(ctx)
	}
	e.workers.Add(1)
	go e.domainRefreshLoop(ctx)
	if e.announce != nil {
		// Periodic announce keeps both camp's last_seen fresh and the
		// camp-facing NAT mapping alive (matters under symmetric NAT).
		e.workers.Add(1)
		go func() {
			defer e.workers.Done()
			e.announce.Run(ctx, 20*time.Second)
		}()
		// Peer list poller. onUpdate runs in the poller goroutine.
		base, err := rendezvous.CampHTTPBase(cfg.Camp.URL)
		if err != nil {
			log.Printf("camp: %v (peer list disabled)", err)
		} else {
			poller := rendezvous.NewPeerListPoller(base, cfg.Camp.ID, e.applyPeerList)
			e.workers.Add(1)
			go func() {
				defer e.workers.Done()
				poller.Run(ctx, 30*time.Second)
			}()
		}
	}
	if e.udp != nil {
		e.workers.Add(1)
		go e.holePunchLoop(ctx)
	}
	if e.cfg.Camp != nil {
		e.workers.Add(1)
		go e.domainPollLoop(ctx)
	}
	// Local DNS resolver for <camp_id>.f2f. We bind to 127.0.0.1:5354
	// — 5353 is contended on macOS by mDNSResponder and any running
	// Bonjour/mDNS client (notably Chrome). Drops /etc/resolver pointing
	// macOS at us for the zone. Failures here are non-fatal — the rest
	// of the engine works without DNS.
	if e.cfg.Camp != nil {
		const dnsAddr = "127.0.0.1:5354"
		srv, err := internaldns.Open(dnsAddr, e.cfg.Camp.ID, e)
		if err != nil {
			log.Printf("dns: %v (resolver disabled)", err)
		} else {
			e.dnsSrv = srv
			if rerr := internaldns.WriteResolver(e.cfg.Camp.ID, dnsAddr); rerr != nil {
				log.Printf("dns: write resolver: %v", rerr)
			} else {
				log.Printf("dns: serving %s.f2f on %s", e.cfg.Camp.ID, dnsAddr)
			}
		}
	}
	// Local CA for HTTPS termination. Persisted under /var/lib/f2f/ca
	// so it survives restarts. Regenerated whenever camp_id changes
	// (NameConstraints in the cert pin it to one zone). Failures here
	// are non-fatal — HTTPS just won't work, HTTP still does.
	if e.cfg.Camp != nil {
		if err := e.ensureCA(); err != nil {
			log.Printf("ca: %v (https disabled)", err)
		}
		e.loadTrustedPeerCAs()
		e.workers.Add(1)
		go e.peerCAPollLoop(ctx)
	}
	if e.OnStarted != nil {
		e.OnStarted(cfg.LocalIP)
	}
	return nil
}

// caDir is where ca.crt/ca.key are persisted.
const caDir = "/var/lib/f2f/ca"

// ensureCA loads the on-disk CA, regenerates it if missing or pinned to
// a different camp_id, and installs the cert into the system keychain.
// Idempotent: safe to call repeatedly on Start.
func (e *Engine) ensureCA() error {
	loaded, err := ca.Load(caDir)
	if err != nil {
		log.Printf("ca: load: %v (will regenerate)", err)
		loaded = nil
	}
	if loaded != nil && !loaded.MatchesZone(e.cfg.Camp.ID) {
		log.Printf("ca: existing CA pinned to a different camp_id; rotating")
		_ = keychain.RemoveByCommonName(loaded.CommonName())
		loaded = nil
	}
	if loaded == nil {
		fresh, err := ca.Generate(e.cfg.Camp.ID)
		if err != nil {
			return fmt.Errorf("generate: %w", err)
		}
		if err := fresh.Save(caDir); err != nil {
			return fmt.Errorf("save: %w", err)
		}
		log.Printf("ca: generated %s (fp %s)", fresh.CommonName(), fresh.Fingerprint())
		loaded = fresh
	} else {
		log.Printf("ca: loaded %s (fp %s)", loaded.CommonName(), loaded.Fingerprint())
	}
	if err := keychain.AddTrustedRoot(ca.CertPath(caDir)); err != nil {
		log.Printf("ca: install in keychain: %v (https will show warnings)", err)
	}
	e.ca = loaded
	return nil
}

// CA returns the local certificate authority for issuing leaf certs
// on demand. nil if engine is not running in camp mode or CA setup
// failed.
func (e *Engine) CA() *ca.CA {
	return e.ca
}

// trustedPeersDir is where we cache peer CA certs (one file per peer)
// to recognise them across engine restarts without re-prompting.
const trustedPeersDir = "/var/lib/f2f/trusted-peers"

// loadTrustedPeerCAs reads the on-disk record of which peer CAs were
// already installed (so we don't keychain-install them again on every
// engine restart). Called once at engine.Start under camp mode.
func (e *Engine) loadTrustedPeerCAs() {
	entries, err := os.ReadDir(trustedPeersDir)
	if err != nil {
		return
	}
	e.trustedPeerCAsMu.Lock()
	defer e.trustedPeerCAsMu.Unlock()
	for _, en := range entries {
		if en.IsDir() || !strings.HasSuffix(en.Name(), ".crt") {
			continue
		}
		full := filepath.Join(trustedPeersDir, en.Name())
		body, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		block, _ := pem.Decode(body)
		if block == nil {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		fp := certFingerprint(cert)
		installedAt := int64(0)
		if fi, err := os.Stat(full); err == nil {
			installedAt = fi.ModTime().Unix()
		}
		e.trustedPeerCAs[fp] = TrustedPeerCA{
			PeerName:    strings.TrimSuffix(en.Name(), ".crt"),
			CommonName:  cert.Subject.CommonName,
			Fingerprint: fp,
			InstalledAt: installedAt,
		}
	}
}

func certFingerprint(cert *x509.Certificate) string {
	h := sha256.Sum256(cert.Raw)
	return fmt.Sprintf("%x", h[:8])
}

// peerCAPollLoop walks every online peer once per tick and pulls their
// /api/ca-cert. New CAs (fingerprint not in cache) get persisted on
// disk and installed into the system keychain via `security`. Each new
// CA install prompts the user once for their macOS password.
func (e *Engine) peerCAPollLoop(ctx context.Context) {
	defer e.workers.Done()
	const interval = 30 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		e.pollAllPeerCAs(ctx)
	}
}

func (e *Engine) pollAllPeerCAs(ctx context.Context) {
	type target struct {
		tunnelIP string
		name     string
	}
	var targets []target
	e.mu.Lock()
	for tip, p := range e.peers {
		if !p.Online {
			continue
		}
		targets = append(targets, target{tunnelIP: tip, name: p.Name})
	}
	port := e.tunnelHTTPPort
	e.mu.Unlock()
	if port == "" {
		return
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for _, t := range targets {
		url := "http://" + net.JoinHostPort(t.tunnelIP, port) + "/api/ca-cert"
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			continue
		}
		e.maybeInstallPeerCA(t.name, body)
	}
}

// maybeInstallPeerCA parses the PEM body, computes a fingerprint,
// and if not already known, persists to disk and runs `security
// add-trusted-cert`. Failures are logged but non-fatal.
func (e *Engine) maybeInstallPeerCA(peerName string, pem []byte) {
	cert, err := parseCACert(pem)
	if err != nil {
		log.Printf("ca: peer %s: parse: %v", peerName, err)
		return
	}
	fp := certFingerprint(cert)

	e.trustedPeerCAsMu.Lock()
	if _, seen := e.trustedPeerCAs[fp]; seen {
		e.trustedPeerCAsMu.Unlock()
		return
	}
	e.trustedPeerCAsMu.Unlock()

	if err := os.MkdirAll(trustedPeersDir, 0o755); err != nil {
		log.Printf("ca: mkdir %s: %v", trustedPeersDir, err)
		return
	}
	certPath := filepath.Join(trustedPeersDir, peerName+".crt")
	if err := os.WriteFile(certPath, pem, 0o644); err != nil {
		log.Printf("ca: write %s: %v", certPath, err)
		return
	}
	log.Printf("ca: installing peer %s CA %q (fp %s) — macOS will prompt for password", peerName, cert.Subject.CommonName, fp)
	if err := keychain.AddTrustedRoot(certPath); err != nil {
		log.Printf("ca: install peer %s: %v", peerName, err)
		return
	}
	e.trustedPeerCAsMu.Lock()
	e.trustedPeerCAs[fp] = TrustedPeerCA{
		PeerName:    peerName,
		CommonName:  cert.Subject.CommonName,
		Fingerprint: fp,
		InstalledAt: time.Now().Unix(),
	}
	e.trustedPeerCAsMu.Unlock()
	log.Printf("ca: trusted peer %s", peerName)
}

func parseCACert(p []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(p)
	if block == nil {
		return nil, fmt.Errorf("not a PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

// applyPeerList reconciles our peers map with the camp's current view
// and caches the snapshot for the UI. Every peer (except ourselves) is
// tracked so the holePunchLoop can keep NAT mappings open with all of
// them. Active selection is independent and driven by the UI.
func (e *Engine) applyPeerList(peers []rendezvous.PeerInfo) {
	snap := append([]rendezvous.PeerInfo(nil), peers...)
	e.campPeers.Store(&snap)

	var ourName string
	if cfg := e.cfg.Camp; cfg != nil {
		ourName = cfg.Name
	}

	seen := make(map[string]struct{}, len(peers))
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range peers {
		if p.Name == ourName || p.TunnelIP == "" {
			continue
		}
		var addr *net.UDPAddr
		if p.Online && p.UDPEndpoint != "" {
			a, err := net.ResolveUDPAddr("udp", p.UDPEndpoint)
			if err != nil {
				log.Printf("WARN: peer %s invalid endpoint %q: %v", p.Name, p.UDPEndpoint, err)
				continue
			}
			addr = a
		}
		existing, ok := e.peers[p.TunnelIP]
		if !ok {
			st := &peerState{
				Name:        p.Name,
				TunnelIP:    p.TunnelIP,
				PublicIP:    p.PublicIP,
				UDPPort:     p.UDPPort,
				UDPEndpoint: p.UDPEndpoint,
				JoinedAt:    p.JoinedAt,
				Online:      p.Online,
				LastSeenAt:  p.LastSeenAt,
				UDPAddr:     addr,
			}
			e.peers[p.TunnelIP] = st
			if p.Online {
				log.Printf("camp: peer %s @ %s joined (tunnel_ip=%s)", p.Name, addr, p.TunnelIP)
			} else {
				log.Printf("camp: peer %s known offline (tunnel_ip=%s)", p.Name, p.TunnelIP)
			}
		} else {
			existing.Name = p.Name
			existing.LastSeenAt = p.LastSeenAt
			if p.Online {
				existing.PublicIP = p.PublicIP
				existing.UDPPort = p.UDPPort
				existing.UDPEndpoint = p.UDPEndpoint
				if addr != nil && !sameUDPAddr(existing.UDPAddr, addr) {
					existing.UDPAddr = addr
				}
				if !existing.Online {
					log.Printf("camp: peer %s back online (tunnel_ip=%s)", p.Name, p.TunnelIP)
				}
			} else if existing.Online {
				log.Printf("camp: peer %s went offline (tunnel_ip=%s)", p.Name, p.TunnelIP)
			}
			existing.Online = p.Online
		}
		seen[p.TunnelIP] = struct{}{}
	}
	// Drop peers no longer in the camp roster at all (binding expired).
	// Going offline does NOT trigger removal — those still show up in
	// `peers` with Online=false from the camp side.
	active := e.activeTunnelIP.Load()
	for tip, st := range e.peers {
		if _, alive := seen[tip]; alive {
			continue
		}
		log.Printf("camp: peer %s @ %s removed (binding expired)", st.Name, st.UDPAddr)
		delete(e.peers, tip)
		if active != nil && *active == tip {
			e.activeTunnelIP.Store(nil)
		}
	}
}

// CampPeers returns the most recent peer-list snapshot from the camp
// poller. Empty slice if the engine isn't running or no poll has
// completed yet. The returned slice is a copy and safe to mutate.
func (e *Engine) CampPeers() []rendezvous.PeerInfo {
	p := e.campPeers.Load()
	if p == nil {
		return nil
	}
	out := make([]rendezvous.PeerInfo, len(*p))
	copy(out, *p)
	return out
}

// 1-byte UDP punch/keepalive packets are below our 20-byte IP minimum,
// so the receiving peer's peerToTunLoop drops them without touching
// utun. They exist purely to keep NAT mappings open.
// holePunchLoop sends 1-byte UDP packets to every known peer at an
// adaptive cadence: 1 Hz while the peer is unconfirmed (LastSeenMs ==
// 0 or stale by >25s), then once per ~25s as keepalive once we've
// seen a packet from them. The single tick drives both modes, so a
// peer that goes silent automatically reverts to burst mode.
// domainPollLoop walks every online peer once per tick and pulls their
// /api/domains list over HTTP-through-tunnel. The result is stashed on
// each peerState so the local DNS server can answer queries. We poll
// even peers we haven't seen "fresh" via punch — the tunnel listener
// is independent of the punch path and may still be reachable.
func (e *Engine) domainPollLoop(ctx context.Context) {
	defer e.workers.Done()
	const interval = 10 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Wait one tick before the first poll so peers have time to register.
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		e.pollAllPeerDomains(ctx)
	}
}

func (e *Engine) pollAllPeerDomains(ctx context.Context) {
	type target struct {
		tunnelIP string
		name     string
	}
	var targets []target
	e.mu.Lock()
	for tip, p := range e.peers {
		if !p.Online {
			continue
		}
		targets = append(targets, target{tunnelIP: tip, name: p.Name})
	}
	port := domainPollPort(e)
	e.mu.Unlock()
	if port == "" {
		return
	}

	client := &http.Client{Timeout: 3 * time.Second}
	for _, t := range targets {
		url := "http://" + net.JoinHostPort(t.tunnelIP, port) + "/api/domains"
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			// Network blip; clear stale list so we don't keep resolving
			// names the peer might have already removed.
			e.mu.Lock()
			if p, ok := e.peers[t.tunnelIP]; ok {
				p.Domains = nil
			}
			e.mu.Unlock()
			continue
		}
		var list []DomainEntry
		err = json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if err != nil {
			continue
		}
		e.mu.Lock()
		if p, ok := e.peers[t.tunnelIP]; ok {
			p.Domains = list
		}
		e.mu.Unlock()
	}
}

// domainPollPort returns the port our peers' tunnel-side HTTP listener
// is on. Same port we host UI on — currently engine doesn't know that
// directly (the UI cmd holds it), so we expose it via a hook field set
// by main. Empty disables polling.
func domainPollPort(e *Engine) string {
	if e.tunnelHTTPPort != "" {
		return e.tunnelHTTPPort
	}
	return ""
}

func (e *Engine) SetTunnelHTTPPort(port string) {
	e.tunnelHTTPPort = port
}

func (e *Engine) holePunchLoop(ctx context.Context) {
	defer e.workers.Done()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	const (
		burstMs     = 1000  // probe cadence while unconfirmed / stale
		keepaliveMs = 25000 // probe cadence once we've seen the peer
		freshMs     = 30000 // anything older than this counts as stale
	)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UnixMilli()
			e.mu.Lock()
			targets := make([]*peerState, 0, len(e.peers))
			for _, p := range e.peers {
				// Skip peers without a known UDP target: camp marked them
				// offline, or we've never observed their endpoint. Punch
				// resumes the moment they reappear in applyPeerList.
				if p.UDPAddr == nil {
					continue
				}
				targets = append(targets, p)
			}
			e.mu.Unlock()
			for _, p := range targets {
				seen := p.LastSeenMs.Load()
				lastSent := p.LastPingMs.Load()
				cadence := int64(burstMs)
				if seen != 0 && now-seen < freshMs {
					cadence = keepaliveMs
				}
				if now-lastSent < cadence {
					continue
				}
				if _, err := e.udp.WriteToUDP([]byte{0}, p.UDPAddr); err != nil {
					if ctx.Err() == nil {
						log.Printf("WARN: punch %s: %v", p.Name, err)
					}
					continue
				}
				p.LastPingMs.Store(now)
			}
			// Static --peer mode (legacy): single keepalive every 25s
			// to the configured static endpoint, no peer-state tracking.
			if sp := e.staticPeer.Load(); sp != nil && len(targets) == 0 {
				lastSent := e.lastStaticPingMs.Load()
				if now-lastSent >= keepaliveMs {
					_, _ = e.udp.WriteToUDP([]byte{0}, sp)
					e.lastStaticPingMs.Store(now)
				}
			}
		}
	}
}

// Stop tears everything down in reverse order. Idempotent.
func (e *Engine) Stop() error {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return nil
	}
	cancel := e.cancel
	tun := e.tun
	udp := e.udp
	routes := e.routes
	egr := e.egr
	dnsSrv := e.dnsSrv
	var dnsCampID string
	if e.cfg.Camp != nil {
		dnsCampID = e.cfg.Camp.ID
	}
	e.mu.Unlock()

	// Local DNS first — drop the /etc/resolver file so macOS stops
	// routing queries our way as soon as Stop begins, then shut the
	// listener down. Failures here are advisory.
	if dnsCampID != "" {
		if err := internaldns.RemoveResolver(dnsCampID); err != nil {
			log.Printf("dns: remove resolver: %v", err)
		}
	}
	if dnsSrv != nil {
		_ = dnsSrv.Close()
	}

	cancel()
	// Close UDP first; this aborts the peerToTun worker. It is independent
	// of utun and routes, so it's safe to do early. The announce loop and
	// peer-list poller respond to ctx cancellation above.
	if udp != nil {
		_ = udp.Close()
	}

	var errs []error
	// Routes have to be deleted *while utun is still up*. Once tun.Close()
	// removes the interface, the kernel evicts its routes anyway, but our
	// `-interface utunN` delete commands then fail with "bad address" and
	// (more importantly) our -reject routes — which live on lo0, not on
	// utun — would never get deleted.
	if routes != nil {
		for _, err := range routes.Cleanup() {
			errs = append(errs, err)
		}
	}

	// Now tear utun down. The tunToPeer worker will see Read fail and exit.
	if tun != nil {
		_ = tun.Close()
	}
	e.workers.Wait()

	if egr != nil {
		if err := egr.Close(); err != nil {
			errs = append(errs, fmt.Errorf("egress: %w", err))
		}
	}

	e.mu.Lock()
	e.running = false
	e.tun = nil
	e.udp = nil
	e.routes = nil
	e.egr = nil
	e.dnsSrv = nil
	e.ca = nil
	e.announce = nil
	e.campAddr.Store(nil)
	e.campPeers.Store(nil)
	e.campReflex.Store(nil)
	e.peers = map[string]*peerState{}
	e.activeTunnelIP.Store(nil)
	e.staticPeer.Store(nil)
	e.lastStaticPingMs.Store(0)
	e.intercepts = map[string]*InterceptInfo{}
	e.txBytes.Store(0)
	e.rxBytes.Store(0)
	e.txPackets.Store(0)
	e.rxPackets.Store(0)
	e.mu.Unlock()
	if e.OnStopped != nil {
		e.OnStopped()
	}
	return errors.Join(errs...)
}

// Status returns a snapshot of the current state.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()

	st := Status{
		Running:      e.running,
		EgressActive: e.egr != nil,
		StartedAt:    e.started,
		TxBytes:      e.txBytes.Load(),
		RxBytes:      e.rxBytes.Load(),
		TxPackets:    e.txPackets.Load(),
		RxPackets:    e.rxPackets.Load(),
	}
	if e.tun != nil {
		st.UtunName = e.tun.Name()
	}
	if e.running {
		st.LocalIP = e.cfg.LocalIP
		st.ListenAddr = e.cfg.Listen
		if e.cfg.Camp == nil {
			// Static --peer mode — legacy, single peer.
			st.PeerIP = e.cfg.PeerIP
			if p := e.staticPeer.Load(); p != nil {
				st.PeerAddr = p.String()
			}
		}
		if e.egr != nil {
			st.EgressIface = e.cfg.EgressIface
			st.EgressAnchor = e.egr.Anchor()
		}
		if e.cfg.Camp != nil {
			st.CampActive = e.announce != nil
			st.CampURL = e.cfg.Camp.URL
			st.CampName = e.cfg.Camp.Name
			st.CampID = e.cfg.Camp.ID
			if r := e.campReflex.Load(); r != nil {
				st.CampReflex = *r
			}
			if active := e.activeTunnelIP.Load(); active != nil {
				st.ActivePeerTunnelIP = *active
				if p, ok := e.peers[*active]; ok {
					st.PeerIP = p.TunnelIP
					st.CampPeerName = p.Name
					if p.UDPAddr != nil {
						st.PeerAddr = p.UDPAddr.String()
					}
				}
			}
			st.Peers = e.peersStatusLocked()
		}
	}
	st.Intercepts = make([]InterceptInfo, 0, len(e.intercepts))
	for _, info := range e.intercepts {
		st.Intercepts = append(st.Intercepts, *info)
	}
	return st
}

// peersStatusLocked is a helper for Status — must be called with e.mu
// held. Builds the per-peer view used by both /api/status (raw) and
// the UI proxy. Includes a Self=true entry up front so the UI doesn't
// have to fabricate one.
func (e *Engine) peersStatusLocked() []PeerStatusInfo {
	const reachableWindowMs = 30000
	now := time.Now().UnixMilli()
	active := ""
	if a := e.activeTunnelIP.Load(); a != nil {
		active = *a
	}
	out := make([]PeerStatusInfo, 0, len(e.peers)+1)
	if e.cfg.Camp != nil {
		selfEndpoint := ""
		if r := e.campReflex.Load(); r != nil {
			selfEndpoint = *r
		}
		out = append(out, PeerStatusInfo{
			Name:        e.cfg.Camp.Name,
			TunnelIP:    e.cfg.LocalIP,
			UDPEndpoint: selfEndpoint,
			JoinedAt:    e.started.UnixMilli(),
			Online:      true,
			Reachable:   true,
			Self:        true,
			Domains:     e.MyDomains(),
		})
	}
	for _, p := range e.peers {
		seen := p.LastSeenMs.Load()
		out = append(out, PeerStatusInfo{
			Name:        p.Name,
			TunnelIP:    p.TunnelIP,
			PublicIP:    p.PublicIP,
			UDPPort:     p.UDPPort,
			UDPEndpoint: p.UDPEndpoint,
			JoinedAt:    p.JoinedAt,
			LastSeenMs:  seen,
			Online:      p.Online,
			Reachable:   p.Online && seen != 0 && now-seen < reachableWindowMs,
			Active:      p.TunnelIP == active,
			Domains:     append([]DomainEntry(nil), p.Domains...),
		})
	}
	return out
}

// SetActivePeer is the UI hook for selecting which peer the tunnel's
// catch-all traffic and the meet signalling go to. tunnelIP must match
// a peer currently in the peers map; empty string clears the selection.
func (e *Engine) SetActivePeer(tunnelIP string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if tunnelIP == "" {
		e.activeTunnelIP.Store(nil)
		log.Printf("camp: active peer cleared")
		return nil
	}
	p, ok := e.peers[tunnelIP]
	if !ok {
		return fmt.Errorf("no peer with tunnel_ip %s", tunnelIP)
	}
	e.activeTunnelIP.Store(&tunnelIP)
	log.Printf("camp: active peer = %s (%s)", p.Name, tunnelIP)
	return nil
}

// MyDomains returns a copy of the local-published domain list, never nil.
func (e *Engine) MyDomains() []DomainEntry {
	p := e.myDomains.Load()
	if p == nil {
		return []DomainEntry{}
	}
	out := make([]DomainEntry, len(*p))
	copy(out, *p)
	return out
}

// SetMyDomains replaces the local-published list atomically. Other peers
// pick up the change on their next /api/domains poll (~10s).
func (e *Engine) SetMyDomains(list []DomainEntry) {
	dup := make([]DomainEntry, len(list))
	copy(dup, list)
	e.myDomains.Store(&dup)
}

// PeerDomains returns a snapshot of every known peer's domains, indexed
// by the IP to resolve them to. Other peers' names map to their
// tunnel_ip; our own names map to 127.0.0.1 — looking up a service we
// host on its own tunnel_ip would round-trip through utun → engine →
// drop (engine has no route to "self"), so loopback is the only address
// that lets local apps reach our own published services.
func (e *Engine) PeerDomains() map[string][]internaldns.DomainEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string][]internaldns.DomainEntry, len(e.peers)+1)
	for tip, p := range e.peers {
		if len(p.Domains) == 0 {
			continue
		}
		out[tip] = toDNSEntries(p.Domains)
	}
	if mine := e.MyDomains(); len(mine) > 0 {
		out["127.0.0.1"] = toDNSEntries(mine)
	}
	return out
}

func toDNSEntries(in []DomainEntry) []internaldns.DomainEntry {
	out := make([]internaldns.DomainEntry, len(in))
	for i, e := range in {
		out[i] = internaldns.DomainEntry{Name: e.Name, Port: e.Port, Proto: e.Proto}
	}
	return out
}

// AddIntercept resolves spec, installs its host routes via utun, and binds
// the entry to the named peer. peer must be a name currently in the camp
// peers map; if not, the intercept is rejected. Requires Running.
func (e *Engine) AddIntercept(spec, peer string) (InterceptInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running {
		return InterceptInfo{}, errors.New("engine not running")
	}
	if peer == "" {
		return InterceptInfo{}, errors.New("intercept peer is required")
	}
	if !e.hasPeerNameLocked(peer) {
		return InterceptInfo{}, fmt.Errorf("peer %q is not in the camp", peer)
	}
	return e.addInterceptLocked(spec, peer)
}

// hasPeerNameLocked reports whether any peer in the camp currently has
// this name. Called with e.mu held.
func (e *Engine) hasPeerNameLocked(name string) bool {
	for _, p := range e.peers {
		if p.Name == name {
			return true
		}
	}
	return false
}

// RemoveIntercept deletes all routes installed for the given entry ID.
func (e *Engine) RemoveIntercept(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running {
		return errors.New("engine not running")
	}
	info, ok := e.intercepts[id]
	if !ok {
		return fmt.Errorf("intercept %q not found", id)
	}
	for _, prefStr := range info.Prefixes {
		// Strip the " (reject)" annotation we add for IPv6 entries — the
		// route manager picks the right delete syntax automatically.
		prefStr = strings.TrimSuffix(prefStr, " (reject)")
		p, err := netip.ParsePrefix(prefStr)
		if err != nil {
			continue
		}
		if err := e.routes.Remove(p); err != nil {
			log.Printf("WARN: remove route %s: %v", prefStr, err)
		}
	}
	delete(e.intercepts, id)
	log.Printf("removed intercept %s (%s)", id, info.Spec)
	return nil
}

func isDomainSpec(spec string) bool {
	if _, err := netip.ParsePrefix(spec); err == nil {
		return false
	}
	if _, err := netip.ParseAddr(spec); err == nil {
		return false
	}
	return true
}

// addInterceptLocked must be called with e.mu held and e.running == true.
// peer is the camp-peer name traffic for this intercept is routed to.
func (e *Engine) addInterceptLocked(spec, peer string) (InterceptInfo, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return InterceptInfo{}, errors.New("empty intercept spec")
	}
	prefixes, err := resolveSpec(spec)
	if err != nil {
		return InterceptInfo{}, err
	}
	if len(prefixes) == 0 {
		return InterceptInfo{}, fmt.Errorf("%q: no addresses", spec)
	}

	e.nextItemID++
	id := "i" + strconv.FormatUint(e.nextItemID, 10)
	info := &InterceptInfo{ID: id, Spec: spec, Peer: peer}
	for _, p := range prefixes {
		// IPv6 destinations get a -reject route instead of being sent into
		// the utun: our tunnel is IPv4-only, and forwarding IPv6 packets
		// into a v4-only utun results in OS picking en0 as source and the
		// traffic bypassing us. With -reject the app gets ECONNREFUSED
		// instantly and (in browsers) Happy Eyeballs falls back to the
		// matching A record, which IS routed through the tunnel.
		if p.Addr().Is6() {
			if err := e.routes.AddReject(p); err != nil {
				log.Printf("WARN: route -reject %s: %v", p, err)
				continue
			}
			info.Prefixes = append(info.Prefixes, p.String()+" (reject)")
			log.Printf("route %s → reject (IPv6 fallback to IPv4)", p)
			continue
		}
		if err := e.routes.Add(p); err != nil {
			log.Printf("WARN: route %s: %v", p, err)
			continue
		}
		info.Prefixes = append(info.Prefixes, p.String())
		log.Printf("route %s → %s", p, e.tun.Name())
	}
	if len(info.Prefixes) == 0 {
		return InterceptInfo{}, fmt.Errorf("%q: all route adds failed", spec)
	}
	e.intercepts[id] = info
	return *info, nil
}

func (e *Engine) domainRefreshLoop(ctx context.Context) {
	defer e.workers.Done()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.refreshDomainRoutes()
		}
	}
}

func (e *Engine) refreshDomainRoutes() {
	e.mu.Lock()
	type entry struct {
		id  string
		spec string
		old []string
	}
	var domains []entry
	for id, info := range e.intercepts {
		if isDomainSpec(info.Spec) {
			domains = append(domains, entry{id: id, spec: info.Spec, old: append([]string(nil), info.Prefixes...)})
		}
	}
	e.mu.Unlock()

	for _, d := range domains {
		newPrefixes, err := resolveSpec(d.spec)
		if err != nil {
			log.Printf("WARN: refresh %s: %v", d.spec, err)
			continue
		}

		newSet := make(map[string]netip.Prefix, len(newPrefixes))
		for _, p := range newPrefixes {
			newSet[p.String()] = p
		}
		oldSet := make(map[string]struct{}, len(d.old))
		for _, s := range d.old {
			oldSet[strings.TrimSuffix(s, " (reject)")] = struct{}{}
		}
		changed := len(newSet) != len(oldSet)
		if !changed {
			for s := range oldSet {
				if _, ok := newSet[s]; !ok {
					changed = true
					break
				}
			}
		}
		if !changed {
			continue
		}

		e.mu.Lock()
		info, ok := e.intercepts[d.id]
		if !ok {
			e.mu.Unlock()
			continue
		}
		for _, prefStr := range info.Prefixes {
			prefStr = strings.TrimSuffix(prefStr, " (reject)")
			if p, err := netip.ParsePrefix(prefStr); err == nil {
				if err := e.routes.Remove(p); err != nil {
					log.Printf("WARN: refresh remove route %s: %v", p, err)
				}
			}
		}
		info.Prefixes = nil
		for _, p := range newPrefixes {
			if p.Addr().Is6() {
				if err := e.routes.AddReject(p); err != nil {
					log.Printf("WARN: refresh route -reject %s: %v", p, err)
					continue
				}
				info.Prefixes = append(info.Prefixes, p.String()+" (reject)")
				continue
			}
			if err := e.routes.Add(p); err != nil {
				log.Printf("WARN: refresh route %s: %v", p, err)
				continue
			}
			info.Prefixes = append(info.Prefixes, p.String())
		}
		log.Printf("refreshed routes for %s → %s", d.spec, strings.Join(info.Prefixes, ", "))
		e.mu.Unlock()
	}
}

func resolveSpec(spec string) ([]netip.Prefix, error) {
	if p, err := netip.ParsePrefix(spec); err == nil {
		return []netip.Prefix{p}, nil
	}
	if a, err := netip.ParseAddr(spec); err == nil {
		return []netip.Prefix{netip.PrefixFrom(a, a.BitLen())}, nil
	}
	ips, err := net.LookupIP(spec)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", spec, err)
	}
	out := make([]netip.Prefix, 0, len(ips))
	for _, ip := range ips {
		a, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
		log.Printf("resolved %s → %s", spec, a)
	}
	return out, nil
}

func (e *Engine) tunToPeerLoop(ctx context.Context) {
	defer e.workers.Done()
	hasPeer := e.udp != nil
	for {
		pkt, err := e.tun.Read()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("tun read stopped: %v", err)
			}
			return
		}
		if len(pkt) == 0 {
			continue
		}
		summary := packet.Summary(pkt)
		action := "drop"
		if !hasPeer {
			log.Printf("[%s] %s [%s]", e.tun.Name(), summary, action)
			continue
		}
		// Two routing modes:
		//   - If dst is a known peer's tunnel_ip (10.99.0.X) → send to
		//     that peer directly. Lets meet and direct-IP traffic flow
		//     even without an active peer selected.
		//   - Otherwise (catch-all destinations) → send to the active
		//     peer if any. No active = drop with "drop-no-active".
		// Static --peer mode is handled by the third branch.
		peerAddr := e.routeFor(pkt)
		if peerAddr == nil {
			if e.cfg.Camp != nil {
				action = "drop-no-route"
			} else {
				action = "drop-no-peer"
			}
			log.Printf("[%s] %s [%s]", e.tun.Name(), summary, action)
			continue
		}
		if n, werr := e.udp.WriteToUDP(pkt, peerAddr); werr != nil {
			if ctx.Err() == nil {
				log.Printf("WARN: udp send: %v", werr)
			}
			action = "→peer-failed"
		} else {
			e.txBytes.Add(uint64(n))
			e.txPackets.Add(1)
			action = "→peer"
		}
		log.Printf("[%s] %s [%s]", e.tun.Name(), summary, action)
	}
}

// routeFor decides where an outgoing tunnel packet goes.
//
// Camp mode:
//   1. dst is a known peer's tunnel_ip → that peer (meet, direct).
//   2. dst is covered by an intercept → that intercept's bound peer.
//   3. otherwise → drop (no implicit catch-all peer).
//
// Static mode: always to the configured static peer.
func (e *Engine) routeFor(pkt []byte) *net.UDPAddr {
	if e.cfg.Camp == nil {
		return e.staticPeer.Load()
	}
	dst := packet.ExtractDst(pkt)
	if !dst.IsValid() {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if p, ok := e.peers[dst.String()]; ok && p.UDPAddr != nil {
		return p.UDPAddr
	}
	target := e.interceptPeerForLocked(dst)
	if target == "" {
		return nil
	}
	for _, p := range e.peers {
		if p.Name == target && p.UDPAddr != nil {
			return p.UDPAddr
		}
	}
	return nil
}

// interceptPeerForLocked returns the bound peer name for the first
// intercept whose prefix contains dst, or "" if none match. Called with
// e.mu held.
func (e *Engine) interceptPeerForLocked(dst netip.Addr) string {
	for _, info := range e.intercepts {
		for _, prefStr := range info.Prefixes {
			prefStr = strings.TrimSuffix(prefStr, " (reject)")
			p, err := netip.ParsePrefix(prefStr)
			if err != nil {
				continue
			}
			if p.Contains(dst) {
				return info.Peer
			}
		}
	}
	return ""
}

func (e *Engine) peerToTunLoop(ctx context.Context) {
	defer e.workers.Done()
	buf := make([]byte, tunnel.MTU)
	for {
		n, from, err := e.udp.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("udp read stopped: %v", err)
			}
			return
		}
		pkt := buf[:n]
		// Camp announce replies arrive on this same socket. Dispatch
		// them first so they don't get treated as tunnel data.
		if ca := e.campAddr.Load(); ca != nil && sameUDPAddr(ca, from) {
			if e.announce != nil && e.announce.HandlePacket(pkt) {
				continue
			}
		}
		// Identify which peer sent this and refresh LastSeen *before*
		// any IP-shape filter — that way 1-byte hole-punch and
		// keepalive packets also count as "peer is alive" signals, not
		// just real IP traffic. Two identification paths:
		//   1. The source UDP addr matches a peer's known UDPAddr —
		//      cheap and works as long as NAT hasn't rebound.
		//   2. The packet is a full IPv4 frame whose src tunnel_ip
		//      matches a peer — authoritative across NAT port shifts;
		//      also updates the stored UDPAddr to track the rebind.
		if e.cfg.Camp != nil {
			now := time.Now().UnixMilli()
			e.mu.Lock()
			var hit *peerState
			for _, p := range e.peers {
				if sameUDPAddr(p.UDPAddr, from) {
					hit = p
					break
				}
			}
			if hit == nil && n >= 20 && pkt[0]>>4 == 4 {
				if srcIP, ok := ipv4Src(pkt); ok {
					if p, present := e.peers[srcIP]; present {
						hit = p
						if !sameUDPAddr(p.UDPAddr, from) {
							log.Printf("camp: peer %s endpoint %s → %s", p.Name, p.UDPAddr, from)
							p.UDPAddr = from
						}
					}
				}
			}
			if hit != nil {
				hit.LastSeenMs.Store(now)
			}
			e.mu.Unlock()
		} else {
			if cur := e.staticPeer.Load(); !sameUDPAddr(cur, from) {
				log.Printf("peer address updated: %s → %s", cur, from)
				e.staticPeer.Store(from)
			}
		}

		// Anything not shaped like an IPv4/IPv6 packet — hole-punch
		// markers, random scans, our own keepalives reflected — gets
		// dropped here before it can fail utun.Write.
		if n < 20 {
			continue
		}
		version := pkt[0] >> 4
		if version != 4 && version != 6 {
			log.Printf("[udp %s] drop non-IP byte=0x%02x (%d bytes)", from, pkt[0], n)
			continue
		}
		summary := packet.Summary(pkt)

		if werr := e.tun.Write(pkt); werr != nil {
			if ctx.Err() == nil {
				log.Printf("WARN: utun write from %s: %v", from, werr)
			}
			log.Printf("[udp %s] %s [→utun-failed]", from, summary)
		} else {
			e.rxBytes.Add(uint64(n))
			e.rxPackets.Add(1)
			log.Printf("[udp %s] %s [→utun]", from, summary)
		}
	}
}

// ipv4Src extracts the IPv4 source address from a packet, or returns
// ("", false) for non-IPv4 / too-short input.
func ipv4Src(pkt []byte) (string, bool) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return "", false
	}
	return net.IPv4(pkt[12], pkt[13], pkt[14], pkt[15]).String(), true
}

// rollbackPartial cleans up whatever Start managed to bring up before
// failing. Called with e.mu held.
func (e *Engine) rollbackPartial() {
	if e.tun != nil {
		_ = e.tun.Close()
		e.tun = nil
	}
	if e.udp != nil {
		_ = e.udp.Close()
		e.udp = nil
	}
	if e.routes != nil {
		_ = e.routes.Cleanup()
		e.routes = nil
	}
	if e.egr != nil {
		_ = e.egr.Close()
		e.egr = nil
	}
	e.announce = nil
}

func sameUDPAddr(a, b *net.UDPAddr) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Port == b.Port && a.IP.Equal(b.IP)
}
