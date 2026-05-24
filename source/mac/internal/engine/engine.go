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
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vseplet/f2f/source/mac/internal/ca"
	"github.com/vseplet/f2f/source/mac/internal/config"
	internaldns "github.com/vseplet/f2f/source/mac/internal/dns"
	"github.com/vseplet/f2f/source/mac/internal/egress"
	"github.com/vseplet/f2f/source/mac/internal/firewall"
	"github.com/vseplet/f2f/source/mac/internal/identity"
	"github.com/vseplet/f2f/source/mac/internal/keychain"
	internaltorrent "github.com/vseplet/f2f/source/mac/internal/torrent"
	"github.com/vseplet/f2f/source/mac/internal/packet"
	"github.com/vseplet/f2f/source/mac/internal/peerping"
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
	// Identity (Ed25519) for the running camp. Pub is the full 32-byte
	// public key in hex; Fingerprint is the short SHA-256 prefix the
	// UI shows. Empty in static --peer mode.
	IdentityPub string `json:"identity_pub,omitempty"`
	IdentityFP  string `json:"identity_fp,omitempty"`
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
	// CampHealth surfaces UDP + HTTP liveness with the camp server,
	// used by the UI's camp-health section. Nil when camp mode is off.
	CampHealth *CampHealth `json:"camp_health,omitempty"`
	// Diagnostics is the runtime info dump for the diagnostics tab —
	// DNS counters, goroutines, etc. Always populated when Running.
	Diagnostics *Diagnostics `json:"diagnostics,omitempty"`
}

// CampHealth aggregates the UDP-announce and HTTP-poll signals against
// the camp server. UDP and HTTP travel different paths (different
// sockets, different transport), so split health makes asymmetric
// failures visible — e.g. HTTP fine + UDP wedged after sleep.
type CampHealth struct {
	UDPLastSentMs     int64  `json:"udp_last_sent_ms,omitempty"`
	UDPLastReplyMs    int64  `json:"udp_last_reply_ms,omitempty"`
	UDPRTTMs          int64  `json:"udp_rtt_ms,omitempty"`
	HTTPLastPollMs    int64  `json:"http_last_poll_ms,omitempty"`
	HTTPLastSuccessMs int64  `json:"http_last_success_ms,omitempty"`
	HTTPRTTMs         int64  `json:"http_rtt_ms,omitempty"`
	HTTPLastErr       string `json:"http_last_err,omitempty"`
	HTTPPeersCount    int    `json:"http_peers_count,omitempty"`
}

// Diagnostics is the catch-all runtime info displayed in the UI's
// diagnostics tab. Keep additions here purely additive — JSON omitempty
// means older UIs ignore unknown fields gracefully.
type Diagnostics struct {
	Goroutines    int    `json:"goroutines"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	UDPLocalAddr  string `json:"udp_local_addr,omitempty"`

	DNSTotal       int64 `json:"dns_total"`
	DNSNoError     int64 `json:"dns_noerror"`
	DNSNXDomain    int64 `json:"dns_nxdomain"`
	DNSRefused     int64 `json:"dns_refused"`
	DNSLastQueryMs int64 `json:"dns_last_query_ms,omitempty"`
	DNSResolverOK  bool  `json:"dns_resolver_ok"` // /etc/resolver/<id>.f2f present
}

// PeerStatusInfo augments rendezvous.PeerInfo with our local reachability
// view: when we last received UDP from this peer, and whether it counts
// as "reachable" right now (within 30s window). One synthetic entry
// with Self=true represents us so the UI can render a single uniform
// table.
type PeerStatusInfo struct {
	Name string `json:"name"`
	// Pub is the peer's Ed25519 pubkey in hex (64 chars). Empty for
	// peers that haven't announced one yet. Stable identity across
	// nickname changes — UI shows a fingerprint derived from it.
	Pub         string        `json:"pub,omitempty"`
	// Fp is the short SHA-256 fingerprint (16 hex chars) of Pub —
	// what the UI shows. Computed server-side so the browser doesn't
	// have to do crypto. Empty when Pub is empty.
	Fp          string        `json:"fp,omitempty"`
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
	Files       []PeerFile    `json:"files,omitempty"`
	// Firewall lists the peer's user-published open ports (without
	// built-ins). Polled from their tunnel-side /api/firewall.
	Firewall []FirewallPort `json:"firewall,omitempty"`
	// InCamp = camp server confirms peer is alive in its roster
	// (sent announce within ~60s). This is independent of whether
	// we can reach the peer ourselves — the Online flag above is the
	// local reachability view (we received UDP from them recently).
	InCamp bool `json:"in_camp"`
	// LastPongMs is the wall-clock ms of the most recent pong we got
	// from this peer (0 = never). Verified=true means we've had a pong
	// recently enough to trust the round-trip path is alive in BOTH
	// directions, distinct from Online which only tells us the peer
	// sends something our way.
	LastPongMs int64 `json:"last_pong_ms,omitempty"`
	RTTMs      int64 `json:"rtt_ms,omitempty"`
	Verified   bool  `json:"verified"`
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
//
// Health / HealthCheckedAt are populated by the engine's own health
// loop — never read from incoming PUTs. When this entry appears on
// another peer's machine (via /api/domains poll) the health is the
// owning peer's self-reported view of its own service.
type DomainEntry struct {
	Name string `json:"name"`
	// Host is the upstream the reverse-proxy dials. Blank means
	// 127.0.0.1 (the common case). Use this when the upstream binds
	// to localhost-IPv6-only (Node 17+ defaults), a non-default
	// loopback address, or a LAN host you want to publish through
	// this peer's camp domain.
	Host            string `json:"host,omitempty"`
	Port            int    `json:"port,omitempty"`
	Proto           string `json:"proto,omitempty"`
	Health          string `json:"health,omitempty"`            // "ok" | "fail" | "" (unknown)
	HealthCheckedAt int64  `json:"health_checked_at,omitempty"` // unix seconds
}

// upstreamHost returns the effective host the reverse-proxy and
// health-check should dial. Empty Host → 127.0.0.1 fallback so old
// records (created before this field existed) keep working.
func (d DomainEntry) upstreamHost() string {
	if d.Host == "" {
		return "127.0.0.1"
	}
	return d.Host
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
	Name string
	// Pub is the peer's Ed25519 hex pubkey — stable identity across
	// renames. Empty for peers we haven't yet seen with a pub (legacy
	// transitional window).
	Pub         string
	TunnelIP    string
	PublicIP    string
	UDPPort     int
	UDPEndpoint string
	JoinedAt    int64
	// InCamp = camp server sees this peer in the roster with a recent
	// announce. Set from rendezvous PeerInfo.Online on each camp HTTP
	// poll. Does NOT imply we can reach the peer ourselves — the
	// "online" semantic for that lives in IsOnline() below.
	InCamp     bool
	LastSeenAt int64
	Domains    []DomainEntry
	Files      []PeerFile     // populated by filesPollLoop
	Firewall   []FirewallPort // populated by peerFirewallPollLoop

	UDPAddr      *net.UDPAddr // current best-known UDP target (port can shift on NAT rebind)
	LastSeenMs   atomic.Int64 // epoch ms of last received packet from this peer; 0 = never
	LastPingMs   atomic.Int64 // epoch ms of last punch/keepalive we sent
}

// peerOnlineWindowMs is how long we consider a peer "online" after the
// last UDP packet from them. Roughly 1× hole-punch keepalive period
// plus slack — peers we expect to hear from punch us every 25s, so 30s
// avoids flapping on a single missed packet.
const peerOnlineWindowMs = 30000

// IsOnline reports whether we've received any UDP from the peer
// recently — our local view of reachability, independent of what
// camp says. Used by everything that actually has to send TCP / poll
// over the tunnel.
func (p *peerState) IsOnline() bool {
	if p == nil {
		return false
	}
	seen := p.LastSeenMs.Load()
	if seen == 0 {
		return false
	}
	return time.Now().UnixMilli()-seen < peerOnlineWindowMs
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
	fw       *firewall.Firewall      // default-deny on utun + user-allowed ports
	dnsSrv   *internaldns.Server     // local DNS for <camp_id>.f2f
	ca       *ca.CA                  // local CA for the current camp_id
	torrent  *internaltorrent.Client // BT client for camp file sharing
	announce *rendezvous.AnnounceClient // periodic UDP announce → camp
	poller   *rendezvous.PeerListPoller // periodic HTTP peer-list poll
	pinger   *peerping.Pinger           // round-trip ping/pong per peer
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
	// myDomainHealth is the latest TCP-dial result per published name.
	// healthCheckLoop writes it, MyDomains() merges it into output.
	myDomainHealth   map[string]domainHealth
	myDomainHealthMu sync.Mutex
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

	// userFirewall holds user-configured allow rules for the inbound
	// utun filter. Persisted; combined with builtinFirewallPorts when
	// applying the pf anchor. Mutated through SetUserFirewallPorts so
	// the anchor is kept in sync.
	userFirewall []FirewallPort

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

	// store is the singleton handle to $HOME/.f2f/. Lazily opened on
	// the first Start so test code that just calls New() doesn't touch
	// the filesystem.
	store *config.Store
	// camp mirrors the on-disk <camp_id>.config.json for the currently
	// running camp. nil when engine is stopped or in static mode.
	// Mutations under e.mu are followed by persistCampLocked.
	camp *config.Camp
	// identity is the per-camp Ed25519 keypair under
	// /var/lib/f2f/identity/<camp_id>/. Loaded (or generated) on Start
	// in camp mode; nil otherwise. Identifier the camp server will use
	// for sticky bindings and invite-signing once we wire it through
	// the protocol — for now it's just persisted so the keys exist
	// when we need them.
	identity *identity.Identity
}

// TrustedPeerCA is one row in the UI's trusted-peers panel.
type TrustedPeerCA struct {
	PeerName    string `json:"peer_name"`
	CommonName  string `json:"common_name"`
	Fingerprint string `json:"fingerprint"`
	InstalledAt int64  `json:"installed_at"`
}

// domainHealth is the latest health-check result for one local service.
type domainHealth struct {
	Status    string // "ok" | "fail"
	CheckedAt int64  // unix seconds
}

// PeerFile is one file entry from a peer's /api/files response,
// rehydrated into our shape (Path stripped — peer-facing data only).
type PeerFile struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	InfoHash string `json:"info_hash"`
	Magnet   string `json:"magnet"`
}

// New returns a fresh Engine. Start it to bring it up.
func New() *Engine {
	return &Engine{
		intercepts:     map[string]*InterceptInfo{},
		peers:          map[string]*peerState{},
		trustedPeerCAs: map[string]TrustedPeerCA{},
		myDomainHealth: map[string]domainHealth{},
		tap:            newLogTap(),
	}
}

// TrustedPeerCAs returns a snapshot for the UI panel, sorted by peer
// name so the list doesn't shuffle between polls.
func (e *Engine) TrustedPeerCAs() []TrustedPeerCA {
	e.trustedPeerCAsMu.Lock()
	defer e.trustedPeerCAsMu.Unlock()
	out := make([]TrustedPeerCA, 0, len(e.trustedPeerCAs))
	for _, t := range e.trustedPeerCAs {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PeerName == out[j].PeerName {
			return out[i].Fingerprint < out[j].Fingerprint
		}
		return out[i].PeerName < out[j].PeerName
	})
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

	// Open $HOME/.f2f/ + load (or create) the per-camp config. Camp
	// mode only — static --peer mode has no per-camp identity.
	if cfg.Camp != nil {
		if err := e.ensureStore(); err != nil {
			return fmt.Errorf("config store: %w", err)
		}
		c, err := e.loadOrCreateCamp(cfg.Camp.ID, cfg.Camp.Name)
		if err != nil {
			return fmt.Errorf("config load %s: %w", cfg.Camp.ID, err)
		}
		e.camp = c
		// Per-camp Ed25519 keypair. Lives under /var/lib/f2f/ (root,
		// 0700) so different camps can't correlate and "leaving" a
		// camp is rm -rf of that one subdir. Failures here are fatal
		// — without an identity we can't prove tunnel_ip ownership
		// to the camp server once that path is wired through.
		idDir := filepath.Join("/var/lib/f2f/identity", cfg.Camp.ID)
		id, err := identity.LoadOrGenerate(idDir)
		if err != nil {
			return fmt.Errorf("identity %s: %w", cfg.Camp.ID, err)
		}
		e.identity = id
		log.Printf("identity: camp %s pub=%s fp=%s", cfg.Camp.ID, id.PubHex(), id.Fingerprint())
		// Mirror pub/fingerprint into camp config so the UI can show
		// it offline. Private key stays under /var/lib/f2f/identity/.
		// Only writes when the pub changes (avoids touching the file
		// on every Start once the keypair is stable). Name is left
		// alone — it was set by loadOrCreateCamp.
		pub, fp := id.PubHex(), id.Fingerprint()
		if c.Identity.Pub != pub || c.Identity.Fingerprint != fp {
			c.Identity.Pub = pub
			c.Identity.Fingerprint = fp
			if err := e.store.SaveCamp(c.CampID, c); err != nil {
				log.Printf("identity: persist into camp config: %v", err)
			}
		}
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
		pub := ""
		if e.identity != nil {
			pub = e.identity.PubHex()
		}
		ac, err := rendezvous.NewAnnounceClient(e.udp, cfg.Camp.StunAddr, cfg.Camp.Name, cfg.Camp.ID, pub)
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

	// Firewall: default-deny inbound on the utun interface for
	// packets directed at OUR tunnel_ip (other peers' addresses and
	// egress-forwarded packets are unaffected). Allow only f2f-
	// internal ports + user-configured ones. Failure here is non-
	// fatal — tunnel still works, just without input filtering.
	e.userFirewall = userFirewallFromCamp(e.camp)
	fw, err := firewall.Open(tun.Name(), localIP, mergeFirewallRules(e.userFirewall))
	if err != nil {
		log.Printf("firewall: %v (input not filtered; any 0.0.0.0-bound service is exposed to camp)", err)
	} else {
		e.fw = fw
		log.Printf("firewall: pf anchor %q on %s scoped to %s/32, %d built-in + %d user rule(s)",
			fw.Anchor(), tun.Name(), localIP, len(builtinFirewallPorts), countEnabled(e.userFirewall))
	}

	// Route table is empty at start; intercepts are added via UI / API
	// once peers are visible (peer assignment is mandatory).
	e.routes = route.New(tun.Name())

	// Stamp cfg into the engine before any hydrate path runs — they
	// read e.cfg.Camp.Name to filter our own entry out of the peer
	// catalog. (Workers don't start until further down; e.running is
	// still false, so nothing observes the partial state.)
	e.cfg = cfg

	// Seed in-memory state from camp config — peer catalog so the UI
	// sees known nodes before the first poll; my-domains so we
	// re-announce them right away; intercepts restored after e.peers
	// is populated (so hasPeerNameLocked checks pass).
	if e.camp != nil {
		e.pruneSelfFromCatalogLocked()
		e.hydratePeersFromCatalog()
		domains := make([]DomainEntry, 0, len(e.camp.MyDomains))
		for _, d := range e.camp.MyDomains {
			domains = append(domains, DomainEntry{
				Name:  d.Name,
				Host:  d.Host,
				Port:  d.Port,
				Proto: d.Proto,
			})
		}
		e.myDomains.Store(&domains)
	}

	// Workers.
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.started = time.Now()
	e.running = true

	// Intercepts can be installed now that running=true (addInterceptLocked
	// guards on it). Failures are logged inside the helper.
	if e.camp != nil {
		e.restoreInterceptsFromCamp()
		e.upsertKnownCamp(e.camp.CampID, e.camp.Identity.Name)
	}

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
			e.poller = rendezvous.NewPeerListPoller(base, cfg.Camp.ID, e.applyPeerList)
			e.workers.Add(1)
			go func() {
				defer e.workers.Done()
				e.poller.Run(ctx, 30*time.Second)
			}()
		}
	}
	if e.udp != nil {
		e.workers.Add(1)
		go e.holePunchLoop(ctx)
	}
	if e.udp != nil {
		e.pinger = peerping.New(e.udp, e.pingerTargets)
		e.workers.Add(1)
		go func() {
			defer e.workers.Done()
			e.pinger.Run(ctx)
		}()
	}
	if e.cfg.Camp != nil {
		e.workers.Add(1)
		go e.domainPollLoop(ctx)
		e.workers.Add(1)
		go e.domainHealthLoop(ctx)
		e.workers.Add(1)
		go e.filesPollLoop(ctx)
		e.workers.Add(1)
		go e.peerFirewallPollLoop(ctx)
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
				// Flush macOS's resolver cache so any stale NXDOMAIN
				// pinned before our DNS was up gets dropped immediately.
				internaldns.FlushCache()
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
	// BitTorrent client for camp file sharing. Binds on <tunnel_ip>:6881
	// — only reachable through utun, never the public internet.
	// Start in a goroutine: anacrolix's NewClient can take a non-trivial
	// moment, and we don't want engine.Start to block on it (would
	// freeze the UI in "loading…").
	if e.cfg.Camp != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("torrent: PANIC during startup: %v (file sharing disabled)", r)
				}
			}()
			log.Printf("torrent: initialising client …")
			if err := e.startTorrent(); err != nil {
				log.Printf("torrent: %v (file sharing disabled)", err)
			}
		}()
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
	if keychain.IsInstalledByFingerprint(loaded.Fingerprint256Hex()) {
		log.Printf("ca: already in keychain (fp %s) — skipping install", loaded.Fingerprint())
	} else if err := keychain.AddTrustedRoot(ca.CertPath(caDir)); err != nil {
		log.Printf("ca: install in keychain: %v (https will show warnings)", err)
	} else {
		log.Printf("ca: installed in keychain (fp %s)", loaded.Fingerprint())
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

// torrentSharedDir / torrentDownloadsDir are the on-disk locations the
// BT client uses. The shared dir is per-user (engine runs as root via
// sudo, so we put it under $SUDO_USER's home so the UI can drag files
// into it). DownloadsDir we put under ~/Downloads/f2f-drops/ — peers
// sort into subfolders per sender at write time.
func (e *Engine) torrentSharedDir() string {
	home := userHome()
	return filepath.Join(home, "Library", "Application Support", "f2f", "shared")
}

func (e *Engine) torrentDownloadsDir() string {
	home := userHome()
	return filepath.Join(home, "Downloads", "f2f-drops")
}

// userHome returns the home of the invoking (non-root) user. Engine runs
// as root via sudo, but files should be owned/visible to the user.
func userHome() string {
	if su := os.Getenv("SUDO_USER"); su != "" {
		// /Users/<su>
		return filepath.Join("/Users", su)
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/tmp"
}

// chownToUser switches ownership of the path (recursively if it's a
// directory) to SUDO_USER. Engine runs as root via sudo, so anything
// anacrolix or our handlers create lands root-owned and the user
// can't delete/move it from Finder without re-elevating. Best-effort
// — failures are logged but not fatal.
func chownToUser(path string) {
	su := os.Getenv("SUDO_USER")
	if su == "" {
		return // not running under sudo, nothing to do
	}
	u, err := user.Lookup(su)
	if err != nil {
		log.Printf("chown: lookup %s: %v", su, err)
		return
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	// Walk and chown anything we own as root.
	_ = filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable, continue
		}
		if cerr := os.Lchown(p, uid, gid); cerr != nil {
			log.Printf("chown: %s: %v", p, cerr)
		}
		return nil
	})
}

func (e *Engine) startTorrent() error {
	opts := internaltorrent.Options{
		ListenAddr:   net.JoinHostPort(e.cfg.LocalIP, fmt.Sprint(internaltorrent.DefaultPort)),
		SharedDir:    e.torrentSharedDir(),
		DownloadsDir: e.torrentDownloadsDir(),
	}
	log.Printf("torrent: binding on %s …", opts.ListenAddr)
	t0 := time.Now()
	c, err := internaltorrent.New(opts)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.torrent = c
	e.mu.Unlock()
	log.Printf("torrent: ready in %v (shared=%s downloads=%s)",
		time.Since(t0).Round(time.Millisecond), opts.SharedDir, opts.DownloadsDir)
	// One-shot chown on the catalogs themselves so the user (not
	// root) can manage them from Finder. Future files anacrolix
	// creates are owned root — covered by chownLoop below.
	chownToUser(opts.SharedDir)
	chownToUser(opts.DownloadsDir)
	// Re-seed everything already in the shared dir from a previous
	// run. Without this, files survive on disk but anacrolix has no
	// knowledge of them — UI shows an empty "my shared files" list
	// after restart.
	go e.rescanSharedDir(c, opts.SharedDir)
	// Same idea for previously-downloaded files: replay the saved
	// magnets so anacrolix re-checks them on disk and resumes
	// seeding; UI's downloads section comes back populated.
	go e.restoreDownloads(c)
	// Periodic chown — anacrolix writes pieces to disk as root
	// because the engine runs under sudo; without this, completed
	// downloads in ~/Downloads/f2f-drops/ would need admin rights to
	// delete or move from Finder.
	go e.chownLoop(opts.SharedDir, opts.DownloadsDir)
	// Prune torrents whose backing files have been deleted (user
	// trashed them from Finder). Without this they keep appearing
	// in UI as "seeding" forever.
	go e.pruneLoop(c)
	return nil
}

// pruneLoop drops Download/Seed entries whose on-disk file is gone,
// and re-feeds peer addresses to active downloads so anacrolix has a
// chance to re-dial peers that were unreachable on the first try.
// Without re-feeding, a download that loses (or never makes) its
// peer connection stays stuck at 0% forever — DHT/PEX/trackers are
// disabled, anacrolix has no other way to find that peer again.
func (e *Engine) pruneLoop(c *internaltorrent.Client) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	e.pruneOnce(c) // immediate pass on start
	for range ticker.C {
		e.pruneOnce(c)
		e.refeedActiveDownloads(c)
	}
}

func (e *Engine) refeedActiveDownloads(c *internaltorrent.Client) {
	const stallAfter = 90 * time.Second
	now := time.Now()
	for _, d := range c.ListDownloads() {
		if d.Torrent == nil || d.Torrent.Info() == nil {
			// Still waiting for metadata — re-feed too, that's
			// exactly the stuck-at-0% case.
			c.FeedPeers(d)
			continue
		}
		total := d.Torrent.Info().TotalLength()
		done := d.Torrent.BytesCompleted()
		if total > 0 && done >= total {
			continue // complete
		}
		// Progress tracking: if BytesCompleted advanced since last
		// check, reset the stall clock. If it hasn't advanced and
		// enough time has passed, the peer is probably gone — drop
		// and re-add the magnet so anacrolix starts a fresh dial
		// cycle. This recovers from the "source peer restarted" case
		// that just re-feeding doesn't fix (anacrolix backs off
		// recently-disconnected peers).
		if done > d.LastBytes {
			d.LastBytes = done
			d.LastProgressAt = now
		}
		stalled := now.Sub(d.LastProgressAt) > stallAfter
		if stalled && d.Magnet != "" {
			log.Printf("downloads: %s stalled (%s with no progress) — drop+re-add",
				d.InfoHash, now.Sub(d.LastProgressAt).Round(time.Second))
			peers := append([]string(nil), d.Peers...)
			magnet := d.Magnet
			c.RemoveDownload(d.InfoHash)
			if _, err := c.AddDownload(magnet, peers); err != nil {
				log.Printf("downloads: stall recovery re-add %s: %v", d.InfoHash, err)
			}
			continue
		}
		c.FeedPeers(d)
	}
}

func (e *Engine) pruneOnce(c *internaltorrent.Client) {
	// Downloads: only prune entries that WERE complete (file existed
	// at the final path) and have since disappeared. We do NOT prune
	// in-progress downloads — anacrolix writes to "<name>.part" until
	// done and renames on completion, so the final path is absent
	// mid-flight. Pruning then would kill active transfers.
	removed := false
	saved := loadSavedDownloads()
	keep := saved[:0]
	for _, s := range saved {
		var d *internaltorrent.Download
		for _, x := range c.ListDownloads() {
			if x.InfoHash == s.InfoHash {
				d = x
				break
			}
		}
		if d == nil || d.Torrent == nil || d.Torrent.Info() == nil {
			// No info yet — keep, anacrolix is still bootstrapping.
			keep = append(keep, s)
			continue
		}
		// Treat "complete" as BytesCompleted >= total length (same
		// criterion the HTTP layer uses). Only completed torrents
		// have a stable final-path file to check for deletion.
		total := d.Torrent.Info().TotalLength()
		complete := total > 0 && d.Torrent.BytesCompleted() >= total
		if !complete {
			keep = append(keep, s)
			continue
		}
		path := c.DownloadPath(d)
		if path == "" {
			keep = append(keep, s)
			continue
		}
		if _, err := os.Stat(path); err != nil {
			log.Printf("downloads: file gone, dropping %s (%s)", s.InfoHash, path)
			c.RemoveDownload(s.InfoHash)
			removed = true
			continue
		}
		keep = append(keep, s)
	}
	if removed {
		if err := saveDownloads(keep); err != nil {
			log.Printf("downloads: persist after prune: %v", err)
		}
	}
	// Seeds (my shared files): always complete by construction (we
	// only AddSeed an existing file). Safe to prune on missing.
	for _, h := range c.ListSeeds() {
		if h.Path == "" {
			continue
		}
		if _, err := os.Stat(h.Path); err != nil {
			log.Printf("seeds: file gone, dropping %s (%s)", h.InfoHash, h.Path)
			_ = c.RemoveSeed(h.InfoHash)
		}
	}
}

// chownLoop walks shared and downloads dirs every few seconds and
// re-chowns anything still owned by root to SUDO_USER. Cheap on a
// small catalog; if it ever becomes expensive, we can move to
// "chown on torrent-complete" but periodic sweep is bullet-proof
// against any code path we forget.
func (e *Engine) chownLoop(dirs ...string) {
	for _, d := range dirs {
		chownToUser(d)
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			for _, d := range dirs {
				chownToUser(d)
			}
		}
	}
}

// rescanSharedDir walks SharedDir (one level deep, files only) and
// AddSeeds each. Errors per-file are logged and skipped — one bad
// file shouldn't drop the rest. Runs in a goroutine so a large
// catalog doesn't block engine.Start.
func (e *Engine) rescanSharedDir(c *internaltorrent.Client, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("torrent: rescan %s: %v", dir, err)
		return
	}
	added := 0
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		h, err := c.AddSeed(path)
		if err != nil {
			log.Printf("torrent: rescan %s: %v", name, err)
			continue
		}
		added++
		log.Printf("torrent: rescan re-seeded %s (%d bytes, info_hash=%s)", name, h.Size, h.InfoHash)
	}
	if added > 0 {
		log.Printf("torrent: rescan re-seeded %d file(s) from %s", added, dir)
	}
}

// Torrent returns the live BT client (nil if not running).
func (e *Engine) Torrent() *internaltorrent.Client {
	return e.torrent
}

// DownloadsDir exposes the on-disk path where incoming BT downloads
// land. Web layer uses it to scope the reveal-in-Finder endpoint.
func (e *Engine) DownloadsDir() string {
	return e.torrentDownloadsDir()
}

// savedDownload is one entry in downloads.json — enough info to
// re-register the torrent with anacrolix on next startup. Anacrolix
// re-hashes on-disk files and immediately seeds whatever is already
// there, so the user sees the download return.
type savedDownload struct {
	Magnet   string   `json:"magnet"`
	InfoHash string   `json:"info_hash"`
	Peers    []string `json:"peers,omitempty"`
}

func downloadsStatePath() string {
	return filepath.Join(userHome(), "Library", "Application Support", "f2f", "downloads.json")
}

func loadSavedDownloads() []savedDownload {
	data, err := os.ReadFile(downloadsStatePath())
	if err != nil {
		return nil
	}
	var out []savedDownload
	if err := json.Unmarshal(data, &out); err != nil {
		log.Printf("downloads: parse %s: %v", downloadsStatePath(), err)
		return nil
	}
	return out
}

func saveDownloads(list []savedDownload) error {
	path := downloadsStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// AddDownload wraps Torrent.AddDownload and persists the entry so it
// survives engine restart. Idempotent — re-adding the same info_hash
// (e.g. user re-clicks library row after restart) is a no-op from
// anacrolix's perspective and we dedupe the saved list by info_hash.
func (e *Engine) AddDownload(magnet string, peers []string) (*internaltorrent.Download, error) {
	t := e.torrent
	if t == nil {
		return nil, fmt.Errorf("torrent client not running")
	}
	d, err := t.AddDownload(magnet, peers)
	if err != nil {
		return nil, err
	}
	saved := loadSavedDownloads()
	for _, s := range saved {
		if s.InfoHash == d.InfoHash {
			return d, nil // already remembered
		}
	}
	saved = append(saved, savedDownload{
		Magnet: magnet, InfoHash: d.InfoHash, Peers: peers,
	})
	if err := saveDownloads(saved); err != nil {
		log.Printf("downloads: persist: %v", err)
	}
	return d, nil
}

// RemoveDownload cancels (or unseeds) a download by info_hash and
// drops it from downloads.json so it doesn't come back on restart.
// Returns true if anacrolix had an entry; false means we tried to
// remove something unknown — still drops from the persisted list as
// a courtesy. Files on disk are NOT deleted; pruneLoop handles that
// case when the user removes them via Finder.
func (e *Engine) RemoveDownload(infoHash string) bool {
	t := e.torrent
	removed := false
	if t != nil {
		removed = t.RemoveDownload(infoHash)
	}
	saved := loadSavedDownloads()
	kept := saved[:0]
	for _, s := range saved {
		if s.InfoHash == infoHash {
			continue
		}
		kept = append(kept, s)
	}
	if len(kept) != len(saved) {
		if err := saveDownloads(kept); err != nil {
			log.Printf("downloads: persist after remove: %v", err)
		}
	}
	return removed
}

// restoreDownloads re-registers every persisted download with the
// torrent client. Anacrolix re-checks on-disk pieces; complete files
// become available for seeding immediately and appear in /api/files/
// downloads with `complete=true` so the UI shows them.
func (e *Engine) restoreDownloads(c *internaltorrent.Client) {
	saved := loadSavedDownloads()
	if len(saved) == 0 {
		return
	}
	added := 0
	for _, s := range saved {
		if _, err := c.AddDownload(s.Magnet, s.Peers); err != nil {
			log.Printf("downloads: restore %s: %v", s.InfoHash, err)
			continue
		}
		added++
	}
	if added > 0 {
		log.Printf("downloads: restored %d previously-downloaded torrent(s)", added)
	}
}

// currentReflex returns our latest camp-observed external endpoint —
// the value the camp server most recently told us about ourselves on
// an announce reply. announce.HandlePacket keeps this fresh on every
// reply, so this reflects the live NAT mapping (matters after Wi-Fi
// switches, network changes etc.) instead of the boot-time value.
// Returns "" when announce is not running or no reply has arrived
// yet. e.campReflex is kept as a fallback for the bootstrap window
// before the first reply.
func (e *Engine) currentReflex() string {
	if e.announce != nil {
		if self := e.announce.Self(); self != nil {
			if self.UDPEndpoint != "" {
				return self.UDPEndpoint
			}
			if self.PublicIP != "" {
				return self.PublicIP
			}
		}
	}
	if r := e.campReflex.Load(); r != nil {
		return *r
	}
	return ""
}

// trustedPeersDir is where we cache peer CA certs (one file per peer)
// to recognise them across engine restarts without re-prompting.
const trustedPeersDir = "/var/lib/f2f/trusted-peers"

// builtinFirewallPorts are the ports f2f's own engine listens on over
// the tunnel — always allowed, regardless of user settings. Keep in
// sync with web.Server (HTTP API + reverse proxy ports) and the
// torrent client.
var builtinFirewallPorts = []firewall.PortRule{
	{Port: 2202, Protocol: "tcp"}, // HTTP API on tunnel listener
	{Port: 80, Protocol: "tcp"},   // HTTP reverse proxy
	{Port: 443, Protocol: "tcp"},  // HTTPS reverse proxy
	{Port: 6881, Protocol: "tcp"}, // BitTorrent peer wire
	{Port: 6881, Protocol: "udp"}, // BitTorrent (uTP)
}

// FirewallPort is the API shape — a user-configured allow rule with
// description and enabled flag (so users can toggle without losing
// the row).
type FirewallPort struct {
	Port        int    `json:"port"`
	Protocol    string `json:"protocol"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// FirewallActive reports whether the pf anchor for the inbound utun
// filter is currently loaded — true means user-toggled rules are
// actually enforced by the kernel, false means the engine isn't
// running (or pf-load failed at startup and we fell back to no
// filtering).
func (e *Engine) FirewallActive() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.fw != nil
}

// BuiltinFirewallPorts returns the always-on f2f-internal allow list
// (read-only from the UI's perspective).
func (e *Engine) BuiltinFirewallPorts() []FirewallPort {
	out := make([]FirewallPort, len(builtinFirewallPorts))
	for i, r := range builtinFirewallPorts {
		out[i] = FirewallPort{
			Port:        r.Port,
			Protocol:    r.Protocol,
			Description: builtinPortLabel(r.Port, r.Protocol),
			Enabled:     true,
		}
	}
	return out
}

func builtinPortLabel(port int, proto string) string {
	switch {
	case port == 2202 && proto == "tcp":
		return "f2f HTTP API"
	case port == 80 && proto == "tcp":
		return "f2f HTTP proxy"
	case port == 443 && proto == "tcp":
		return "f2f HTTPS proxy"
	case port == 6881:
		return "f2f BitTorrent"
	}
	return ""
}

// UserFirewallPorts returns the user-configured allow list (a copy).
func (e *Engine) UserFirewallPorts() []FirewallPort {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]FirewallPort, len(e.userFirewall))
	copy(out, e.userFirewall)
	return out
}

// SetUserFirewallPorts replaces the user allow list, persists it
// into the per-camp config, and re-applies the pf anchor with
// built-in + enabled user rules. Idempotent — safe to call on every
// UI save. Returns ErrEngineNotRunning if the engine isn't up: the
// camp config is keyed by camp_id, so we need a running engine to
// know which file to write.
func (e *Engine) SetUserFirewallPorts(list []FirewallPort) error {
	cleaned := cleanUserFirewall(list)
	e.mu.Lock()
	if !e.running || e.camp == nil {
		e.mu.Unlock()
		return errors.New("engine not running")
	}
	e.userFirewall = cleaned
	e.camp.Firewall = userFirewallToCamp(cleaned)
	fw := e.fw
	e.persistCampLocked()
	e.mu.Unlock()
	if fw == nil {
		return nil // pf anchor failed at Start; will retry next Start
	}
	rules := mergeFirewallRules(cleaned)
	if err := fw.Apply(rules); err != nil {
		return fmt.Errorf("firewall: apply: %w", err)
	}
	return nil
}

// mergeFirewallRules combines built-in + enabled user entries into
// the kernel-level rule list passed to pf.
func mergeFirewallRules(user []FirewallPort) []firewall.PortRule {
	out := make([]firewall.PortRule, 0, len(builtinFirewallPorts)+len(user))
	out = append(out, builtinFirewallPorts...)
	for _, p := range user {
		if !p.Enabled {
			continue
		}
		out = append(out, firewall.PortRule{Port: p.Port, Protocol: p.Protocol})
	}
	return out
}

func cleanUserFirewall(in []FirewallPort) []FirewallPort {
	seen := make(map[string]struct{}, len(in))
	out := make([]FirewallPort, 0, len(in))
	for _, p := range in {
		proto := strings.ToLower(strings.TrimSpace(p.Protocol))
		if proto != "tcp" && proto != "udp" {
			continue
		}
		if p.Port <= 0 || p.Port > 65535 {
			continue
		}
		key := fmt.Sprintf("%d/%s", p.Port, proto)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, FirewallPort{
			Port:        p.Port,
			Protocol:    proto,
			Description: strings.TrimSpace(p.Description),
			Enabled:     p.Enabled,
		})
	}
	return out
}

// userFirewallFromCamp converts the on-disk shape to the engine's
// in-memory FirewallPort. Returns a deduplicated/cleaned slice.
func userFirewallFromCamp(c *config.Camp) []FirewallPort {
	if c == nil {
		return nil
	}
	out := make([]FirewallPort, 0, len(c.Firewall))
	for _, p := range c.Firewall {
		out = append(out, FirewallPort{
			Port:        p.Port,
			Protocol:    p.Protocol,
			Description: p.Description,
			Enabled:     p.Enabled,
		})
	}
	return cleanUserFirewall(out)
}

// userFirewallToCamp is the inverse — engine shape → on-disk shape.
func userFirewallToCamp(list []FirewallPort) []config.Firewall {
	out := make([]config.Firewall, 0, len(list))
	for _, p := range list {
		out = append(out, config.Firewall{
			Port:        p.Port,
			Protocol:    p.Protocol,
			Description: p.Description,
			Enabled:     p.Enabled,
		})
	}
	return out
}

func countEnabled(list []FirewallPort) int {
	n := 0
	for _, p := range list {
		if p.Enabled {
			n++
		}
	}
	return n
}

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
//
// First poll fires after a short delay (let camp peer-list populate),
// then every 30s.
func (e *Engine) peerCAPollLoop(ctx context.Context) {
	defer e.workers.Done()
	// Give camp poller a tick to discover peers before we try to talk
	// to them. 5s is enough — first announce + first /api/id/<camp>
	// usually complete in <2s.
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Second):
	}
	e.pollAllPeerCAs(ctx)
	ticker := time.NewTicker(30 * time.Second)
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
		if !p.IsOnline() {
			continue
		}
		targets = append(targets, target{tunnelIP: tip, name: p.Name})
	}
	port := e.tunnelHTTPPort
	e.mu.Unlock()
	if port == "" {
		log.Printf("ca-poll: tunnel HTTP port not set (engine running without UI?) — skipping")
		return
	}
	if len(targets) == 0 {
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
			log.Printf("ca-poll: peer %s: GET %s: %v", t.name, url, err)
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if err != nil {
			log.Printf("ca-poll: peer %s: read body: %v", t.name, err)
			continue
		}
		if resp.StatusCode != 200 {
			log.Printf("ca-poll: peer %s: HTTP %d (peer running an old f2f-mac without /api/ca-cert?)", t.name, resp.StatusCode)
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
	full256 := strings.ToUpper(fmt.Sprintf("%x", sha256.Sum256(cert.Raw)))
	if keychain.IsInstalledByFingerprint(full256) {
		log.Printf("ca: peer %s CA already in keychain (fp %s) — no prompt", peerName, fp)
	} else {
		log.Printf("ca: installing peer %s CA %q (fp %s) — macOS will prompt for password", peerName, cert.Subject.CommonName, fp)
		if err := keychain.AddTrustedRoot(certPath); err != nil {
			log.Printf("ca: install peer %s: %v", peerName, err)
			return
		}
	}
	e.trustedPeerCAsMu.Lock()
	entry := TrustedPeerCA{
		PeerName:    peerName,
		CommonName:  cert.Subject.CommonName,
		Fingerprint: fp,
		InstalledAt: time.Now().Unix(),
	}
	e.trustedPeerCAs[fp] = entry
	e.trustedPeerCAsMu.Unlock()
	e.persistTrustedPeerToCamp(entry)
	log.Printf("ca: trusted peer %s", peerName)
}

// persistTrustedPeerToCamp upserts the trusted-CA metadata into camp
// config. PEM bytes stay under trustedPeersDir — config carries only
// fingerprint + display fields so the UI can list and (eventually)
// remove CAs without re-reading the keychain.
func (e *Engine) persistTrustedPeerToCamp(t TrustedPeerCA) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.camp == nil {
		return
	}
	for i, ex := range e.camp.TrustedPeers {
		if ex.Fingerprint == t.Fingerprint {
			e.camp.TrustedPeers[i] = config.TrustedPeer{
				PeerName:    t.PeerName,
				CommonName:  t.CommonName,
				Fingerprint: t.Fingerprint,
				InstalledAt: t.InstalledAt,
			}
			e.persistCampLocked()
			return
		}
	}
	e.camp.TrustedPeers = append(e.camp.TrustedPeers, config.TrustedPeer{
		PeerName:    t.PeerName,
		CommonName:  t.CommonName,
		Fingerprint: t.Fingerprint,
		InstalledAt: t.InstalledAt,
	})
	e.persistCampLocked()
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
				Pub:         p.Pub,
				TunnelIP:    p.TunnelIP,
				PublicIP:    p.PublicIP,
				UDPPort:     p.UDPPort,
				UDPEndpoint: p.UDPEndpoint,
				JoinedAt:    p.JoinedAt,
				InCamp:      p.Online,
				LastSeenAt:  p.LastSeenAt,
				UDPAddr:     addr,
			}
			e.peers[p.TunnelIP] = st
			if p.Online {
				log.Printf("camp: peer %s @ %s entered roster (tunnel_ip=%s)", p.Name, addr, p.TunnelIP)
			} else {
				log.Printf("camp: peer %s in roster but stale (tunnel_ip=%s)", p.Name, p.TunnelIP)
			}
		} else {
			existing.Name = p.Name
			// Once a peer announces a pub we trust the camp-provided value
			// and remember it across subsequent polls (camp may omit pub
			// for offline-only entries).
			if p.Pub != "" {
				existing.Pub = p.Pub
			}
			existing.LastSeenAt = p.LastSeenAt
			if p.Online {
				existing.PublicIP = p.PublicIP
				existing.UDPPort = p.UDPPort
				existing.UDPEndpoint = p.UDPEndpoint
				if addr != nil && !sameUDPAddr(existing.UDPAddr, addr) {
					existing.UDPAddr = addr
				}
				if !existing.InCamp {
					log.Printf("camp: peer %s back in roster (tunnel_ip=%s)", p.Name, p.TunnelIP)
				}
			} else {
				// Camp evicted the peer (no announce in ~60s) but kept
				// the sticky binding. Drop the endpoint we cached for
				// punch/forwarding — when peer comes back, camp will
				// publish a fresh UDPEndpoint and we'll resolve again.
				existing.UDPAddr = nil
				existing.UDPEndpoint = ""
				existing.PublicIP = ""
				existing.UDPPort = 0
				if existing.InCamp {
					log.Printf("camp: peer %s left roster (tunnel_ip=%s)", p.Name, p.TunnelIP)
				}
			}
			existing.InCamp = p.Online
		}
		seen[p.TunnelIP] = struct{}{}
	}
	// Peers not in the latest poll: camp dropped them from the roster
	// entirely (binding expired on their side). We KEEP them in e.peers
	// as offline ghosts so the UI shows historical nodes — same as if
	// camp still reported them with Online=false. holePunchLoop already
	// skips peers without UDPAddr, so this is safe.
	active := e.activeTunnelIP.Load()
	for tip, st := range e.peers {
		if _, alive := seen[tip]; alive {
			continue
		}
		if st.InCamp || st.UDPAddr != nil {
			log.Printf("camp: peer %s @ %s no longer in roster", st.Name, st.UDPAddr)
		}
		st.InCamp = false
		st.UDPAddr = nil
		st.UDPEndpoint = ""
		st.PublicIP = ""
		st.UDPPort = 0
		if active != nil && *active == tip {
			e.activeTunnelIP.Store(nil)
		}
	}
	// Merge the snapshot into the persistent catalog so the UI sees
	// known nodes (incl. currently-offline) on the next engine start.
	if e.camp != nil {
		e.mergePeerSnapshotLocked(peers)
		e.persistCampLocked()
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
// filesPollLoop walks online peers every minute and pulls /api/files.
// The returned list is cached on peerState.Files so the UI's "camp
// library" can show what's available. We don't talk BT yet — that's
// triggered when the user clicks download.
func (e *Engine) filesPollLoop(ctx context.Context) {
	defer e.workers.Done()
	// Give the camp poll a chance to populate peers first.
	select {
	case <-ctx.Done():
		return
	case <-time.After(7 * time.Second):
	}
	e.pollAllPeerFiles(ctx)
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		e.pollAllPeerFiles(ctx)
	}
}

func (e *Engine) pollAllPeerFiles(ctx context.Context) {
	type target struct {
		tunnelIP string
		name     string
	}
	var targets []target
	e.mu.Lock()
	for tip, p := range e.peers {
		if !p.IsOnline() {
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
		url := "http://" + net.JoinHostPort(t.tunnelIP, port) + "/api/files"
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		var files []PeerFile
		err = json.NewDecoder(resp.Body).Decode(&files)
		resp.Body.Close()
		if err != nil {
			continue
		}
		e.mu.Lock()
		if p, ok := e.peers[t.tunnelIP]; ok {
			p.Files = files
		}
		e.mu.Unlock()
	}
}

// peerFirewallPollLoop walks online peers every 30s and pulls their
// /api/firewall (we only keep the user-configured allow list — the
// built-in list is identical for every f2f peer). Cached on
// peerState.Firewall and mirrored into the catalog so it survives
// engine restart.
func (e *Engine) peerFirewallPollLoop(ctx context.Context) {
	defer e.workers.Done()
	select {
	case <-ctx.Done():
		return
	case <-time.After(9 * time.Second): // small jitter vs files/domain loops
	}
	e.pollAllPeerFirewall(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		e.pollAllPeerFirewall(ctx)
	}
}

func (e *Engine) pollAllPeerFirewall(ctx context.Context) {
	type target struct {
		tunnelIP string
		name     string
	}
	var targets []target
	e.mu.Lock()
	for tip, p := range e.peers {
		if !p.IsOnline() {
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
		url := "http://" + net.JoinHostPort(t.tunnelIP, port) + "/api/firewall"
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			// Same policy as domain poll: keep stale list on transient
			// failure, the UI's peer-online flag conveys "we lost touch".
			continue
		}
		var body struct {
			User []FirewallPort `json:"user"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		e.mu.Lock()
		if p, ok := e.peers[t.tunnelIP]; ok {
			p.Firewall = body.User
		}
		e.persistPeerFirewallLocked(t.tunnelIP, body.User)
		e.mu.Unlock()
	}
}

// persistPeerFirewallLocked mirrors a peer's published firewall list
// into the camp catalog. Caller holds e.mu.
func (e *Engine) persistPeerFirewallLocked(tunnelIP string, fw []FirewallPort) {
	if e.camp == nil {
		return
	}
	out := make([]config.Firewall, 0, len(fw))
	for _, p := range fw {
		out = append(out, config.Firewall{
			Port:        p.Port,
			Protocol:    p.Protocol,
			Description: p.Description,
			Enabled:     p.Enabled,
		})
	}
	for i := range e.camp.PeerCatalog {
		if e.camp.PeerCatalog[i].TunnelIP == tunnelIP {
			e.camp.PeerCatalog[i].Firewall = out
			e.persistCampLocked()
			return
		}
	}
}

// domainHealthLoop TCP-dials each published domain on 127.0.0.1:<port>
// every few seconds and stamps the result onto myDomainHealth. The
// status flows out through MyDomains() — into /api/my-domains for the
// UI, and into /api/domains so OTHER peers see whether our services
// are actually up.
func (e *Engine) domainHealthLoop(ctx context.Context) {
	defer e.workers.Done()
	const interval = 8 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		e.checkMyDomainsHealth(ctx)
	}
}

func (e *Engine) checkMyDomainsHealth(ctx context.Context) {
	domains := e.MyDomains()
	now := time.Now().Unix()
	for _, d := range domains {
		if d.Port == 0 {
			continue
		}
		status := "fail"
		addr := net.JoinHostPort(d.upstreamHost(), strconv.Itoa(d.Port))
		dialer := net.Dialer{Timeout: 2 * time.Second}
		if conn, err := dialer.DialContext(ctx, "tcp", addr); err == nil {
			_ = conn.Close()
			status = "ok"
		}
		e.myDomainHealthMu.Lock()
		e.myDomainHealth[d.Name] = domainHealth{Status: status, CheckedAt: now}
		e.myDomainHealthMu.Unlock()
	}
}

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
		if !p.IsOnline() {
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
			// Transient network failure (handshake didn't complete,
			// peer momentarily unreachable, etc.) — keep the previously
			// seen list as-is so the UI doesn't flicker. The peer-online
			// flag from /api/status already tells the user the peer is
			// having trouble; we surface "service down vs peer down" via
			// the gray (offline) / red (online + health=fail) / green
			// (online + health=ok) tri-state on the UI side.
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
		e.persistPeerDomainsLocked(t.tunnelIP, list)
		e.mu.Unlock()
	}
}

// persistPeerDomainsLocked mirrors a peer's published domain list into
// camp config so we keep it across engine restarts. Called with e.mu
// held. Upserts by peer tunnel_ip; appends a fresh Peer row if the
// catalog doesn't have one yet (shouldn't normally happen — applyPeerList
// makes the catalog entry first — but be defensive).
func (e *Engine) persistPeerDomainsLocked(tunnelIP string, domains []DomainEntry) {
	if e.camp == nil {
		return
	}
	out := make([]config.Domain, 0, len(domains))
	for _, d := range domains {
		out = append(out, config.Domain{
			Name:  d.Name,
			Host:  d.Host,
			Port:  d.Port,
			Proto: d.Proto,
		})
	}
	for i := range e.camp.PeerCatalog {
		if e.camp.PeerCatalog[i].TunnelIP == tunnelIP {
			e.camp.PeerCatalog[i].Domains = out
			e.persistCampLocked()
			return
		}
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
		// wakeJumpMs is the wall-clock gap between ticks above which we
		// assume the host slept. Real ticks land at ~1s; anything past
		// 30s only happens after suspend/resume.
		wakeJumpMs = 30000
	)
	var prevTickMs int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UnixMilli()
			// Wake-from-sleep detection: a tick that lands >wakeJumpMs
			// after the previous one means the host suspended. The
			// upstream NAT binding tied to our current local port is
			// almost certainly stale on the outbound side — rebinding
			// the same port wouldn't refresh it. Easiest reliable cure
			// is a full Stop/Start cycle on an ephemeral local port,
			// which forces a brand-new 5-tuple at the provider's NAT
			// and a fresh reflex in camp; peers pick up the new endpoint
			// on their next camp poll.
			if prevTickMs != 0 && now-prevTickMs > wakeJumpMs {
				log.Printf("wake: clock jumped %ds, restarting on a fresh ephemeral port", (now-prevTickMs)/1000)
				go e.restartOnEphemeralPort()
				return
			}
			prevTickMs = now
			e.mu.Lock()
			targets := make([]*peerState, 0, len(e.peers))
			tunIPs := make([]string, 0, len(e.peers))
			for ip, p := range e.peers {
				// Skip peers without a known UDP target: camp marked them
				// offline, or we've never observed their endpoint. Punch
				// resumes the moment they reappear in applyPeerList.
				if p.UDPAddr == nil {
					continue
				}
				targets = append(targets, p)
				tunIPs = append(tunIPs, ip)
			}
			e.mu.Unlock()
			for i, p := range targets {
				seen := p.LastSeenMs.Load()
				lastSent := p.LastPingMs.Load()
				cadence := int64(burstMs)
				// Healthy = peer is sending us packets AND our last ping
				// got a pong recently. Either signal stale → burst. This
				// covers the asymmetric case: peer's keepalive reaches us
				// fine but our pings get lost (NAT binding lapsed our
				// way), so receive-only freshness isn't enough.
				if seen != 0 && now-seen < freshMs {
					pongFresh := false
					if e.pinger != nil {
						if r, ok := e.pinger.Result(tunIPs[i]); ok && r.LastPongMs != 0 && now-r.LastPongMs < freshMs {
							pongFresh = true
						}
					}
					if pongFresh {
						cadence = keepaliveMs
					}
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

// restartOnEphemeralPort tears the engine down and brings it back up
// with Listen=":0". Used by the wake-from-sleep detector in
// holePunchLoop. Must run in its own goroutine — Stop() waits on the
// worker pool that holePunchLoop is part of, so a synchronous call
// would deadlock.
func (e *Engine) restartOnEphemeralPort() {
	e.mu.Lock()
	cfg := e.cfg
	e.mu.Unlock()
	cfg.Listen = ":0"
	if err := e.Stop(); err != nil {
		log.Printf("wake: stop: %v", err)
		return
	}
	if err := e.Start(cfg); err != nil {
		log.Printf("wake: start: %v", err)
	}
}

// diagnosticsLocked builds the Diagnostics snapshot from various
// subsystems. Caller must hold e.mu.
func (e *Engine) diagnosticsLocked() *Diagnostics {
	d := &Diagnostics{
		Goroutines: runtime.NumGoroutine(),
	}
	if !e.started.IsZero() {
		d.UptimeSeconds = int64(time.Since(e.started).Seconds())
	}
	if e.udp != nil {
		d.UDPLocalAddr = e.udp.LocalAddr().String()
	}
	if e.dnsSrv != nil {
		s := e.dnsSrv.Stats()
		d.DNSTotal = s.Total
		d.DNSNoError = s.NoError
		d.DNSNXDomain = s.NXDomain
		d.DNSRefused = s.Refused
		d.DNSLastQueryMs = s.LastQueryMs
	}
	if e.cfg.Camp != nil {
		d.DNSResolverOK = internaldns.ResolverFileExists(e.cfg.Camp.ID)
	}
	return d
}

// campHealthLocked builds the CampHealth snapshot from the announce
// client and the HTTP poller. Caller must hold e.mu.
func (e *Engine) campHealthLocked() *CampHealth {
	h := &CampHealth{}
	if e.announce != nil {
		h.UDPLastSentMs = e.announce.LastSentMs()
		h.UDPLastReplyMs = e.announce.LastReplyMs()
		h.UDPRTTMs = e.announce.LastRTTMs()
	}
	if e.poller != nil {
		s := e.poller.Stats()
		h.HTTPLastPollMs = s.LastPollMs
		h.HTTPLastSuccessMs = s.LastSuccessMs
		h.HTTPRTTMs = s.LastRTTMs
		h.HTTPLastErr = s.LastErr
		h.HTTPPeersCount = s.PeersCount
	}
	return h
}

// pingerTargets snapshots the current peers with a known UDP endpoint.
// Called from the Pinger goroutine on every tick.
func (e *Engine) pingerTargets() []peerping.Target {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]peerping.Target, 0, len(e.peers))
	for tunnelIP, p := range e.peers {
		if p.UDPAddr == nil {
			continue
		}
		out = append(out, peerping.Target{Key: tunnelIP, Addr: p.UDPAddr})
	}
	return out
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
	fw := e.fw
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

	if fw != nil {
		if err := fw.Close(); err != nil {
			errs = append(errs, fmt.Errorf("firewall: %w", err))
		}
	}
	if egr != nil {
		if err := egr.Close(); err != nil {
			errs = append(errs, fmt.Errorf("egress: %w", err))
		}
	}

	e.mu.Lock()
	if e.torrent != nil {
		_ = e.torrent.Close()
	}
	e.running = false
	e.tun = nil
	e.udp = nil
	e.routes = nil
	e.egr = nil
	e.fw = nil
	e.dnsSrv = nil
	e.ca = nil
	e.torrent = nil
	e.announce = nil
	e.poller = nil
	e.pinger = nil
	e.campAddr.Store(nil)
	e.campPeers.Store(nil)
	e.campReflex.Store(nil)
	e.peers = map[string]*peerState{}
	e.activeTunnelIP.Store(nil)
	e.staticPeer.Store(nil)
	e.lastStaticPingMs.Store(0)
	e.intercepts = map[string]*InterceptInfo{}
	e.camp = nil
	e.identity = nil
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
			st.CampReflex = e.currentReflex()
			if e.identity != nil {
				st.IdentityPub = e.identity.PubHex()
				st.IdentityFP = e.identity.Fingerprint()
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
			st.CampHealth = e.campHealthLocked()
		}
		st.Diagnostics = e.diagnosticsLocked()
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
	active := ""
	if a := e.activeTunnelIP.Load(); a != nil {
		active = *a
	}
	var pingResults map[string]peerping.Result
	if e.pinger != nil {
		pingResults = e.pinger.All()
	}
	now := time.Now().UnixMilli()
	out := make([]PeerStatusInfo, 0, len(e.peers)+1)
	if e.cfg.Camp != nil {
		selfEndpoint := e.currentReflex()
		selfPub, selfFp := "", ""
		if e.identity != nil {
			selfPub = e.identity.PubHex()
			selfFp = e.identity.Fingerprint()
		}
		out = append(out, PeerStatusInfo{
			Name:        e.cfg.Camp.Name,
			Pub:         selfPub,
			Fp:          selfFp,
			TunnelIP:    e.cfg.LocalIP,
			UDPEndpoint: selfEndpoint,
			JoinedAt:    e.started.UnixMilli(),
			InCamp:      true,
			Online:      true,
			Reachable:   true,
			Verified:    true,
			Self:        true,
			Domains:     e.MyDomains(),
		})
	}
	// Sort peer-keys so the UI list is stable across refreshes —
	// Go map iteration is randomised and the camp peer-table would
	// otherwise shuffle every poll.
	keys := make([]string, 0, len(e.peers))
	for k := range e.peers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := e.peers[k]
		seen := p.LastSeenMs.Load()
		online := p.IsOnline()
		var lastPong, rtt int64
		var verified bool
		if r, ok := pingResults[p.TunnelIP]; ok {
			lastPong = r.LastPongMs
			rtt = r.LastRTTMs
			// Verified = round-trip confirmed within the online window
			// (same 30s threshold so the two signals line up).
			verified = lastPong > 0 && now-lastPong < peerOnlineWindowMs
		}
		out = append(out, PeerStatusInfo{
			Name:        p.Name,
			Pub:         p.Pub,
			Fp:          identity.FingerprintHex(p.Pub),
			TunnelIP:    p.TunnelIP,
			PublicIP:    p.PublicIP,
			UDPPort:     p.UDPPort,
			UDPEndpoint: p.UDPEndpoint,
			JoinedAt:    p.JoinedAt,
			LastSeenMs:  seen,
			Online:      online,
			Reachable:   online, // kept as alias for backward compat; same semantic now
			InCamp:      p.InCamp,
			Active:      p.TunnelIP == active,
			Domains:     sortedDomains(p.Domains),
			Files:       sortedFiles(p.Files),
			Firewall:    append([]FirewallPort(nil), p.Firewall...),
			LastPongMs:  lastPong,
			RTTMs:       rtt,
			Verified:    verified,
		})
	}
	return out
}

func sortedDomains(in []DomainEntry) []DomainEntry {
	out := append([]DomainEntry(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sortedFiles(in []PeerFile) []PeerFile {
	out := append([]PeerFile(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].InfoHash < out[j].InfoHash
		}
		return out[i].Name < out[j].Name
	})
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
// Each entry has its current health stamped on (own snapshot from
// healthCheckLoop).
func (e *Engine) MyDomains() []DomainEntry {
	p := e.myDomains.Load()
	if p == nil {
		return []DomainEntry{}
	}
	out := make([]DomainEntry, len(*p))
	copy(out, *p)
	e.myDomainHealthMu.Lock()
	defer e.myDomainHealthMu.Unlock()
	for i := range out {
		if h, ok := e.myDomainHealth[out[i].Name]; ok {
			out[i].Health = h.Status
			out[i].HealthCheckedAt = h.CheckedAt
		}
	}
	return out
}

// SetMyDomains replaces the local-published list atomically. Health
// state for removed names is dropped so the UI doesn't show stale
// "ok" indicators. Other peers pick up the change on their next
// /api/domains poll (~10s). Persists into camp config so the list
// survives engine restart.
func (e *Engine) SetMyDomains(list []DomainEntry) {
	dup := make([]DomainEntry, len(list))
	copy(dup, list)
	e.myDomains.Store(&dup)
	keep := make(map[string]struct{}, len(dup))
	for _, d := range dup {
		keep[d.Name] = struct{}{}
	}
	e.myDomainHealthMu.Lock()
	for name := range e.myDomainHealth {
		if _, ok := keep[name]; !ok {
			delete(e.myDomainHealth, name)
		}
	}
	e.myDomainHealthMu.Unlock()
	e.mu.Lock()
	if e.camp != nil {
		e.camp.MyDomains = make([]config.Domain, 0, len(dup))
		for _, d := range dup {
			e.camp.MyDomains = append(e.camp.MyDomains, config.Domain{
				Name:  d.Name,
				Host:  d.Host,
				Port:  d.Port,
				Proto: d.Proto,
			})
		}
		e.persistCampLocked()
	}
	e.mu.Unlock()
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
		// Only resolve names of peers we can actually reach right now.
		// peer.Domains is kept across reconnects (cache survives camp
		// polls and engine restarts so the UI can show last-known
		// state for offline peers), but the resolver itself should
		// not hand out an IP that the engine has no UDP target for —
		// the kernel would then route the apps' SYNs into utun and
		// tunToPeerLoop would dump them with "drop-no-route". Better
		// the browser get NXDOMAIN and fail fast.
		if !p.IsOnline() || p.UDPAddr == nil {
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
// Persists the (spec, peer) pair into camp config on success.
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
	info, err := e.addInterceptLocked(spec, peer)
	if err != nil {
		return info, err
	}
	if e.camp != nil {
		dup := false
		for _, it := range e.camp.Intercepts {
			if it.Spec == info.Spec && it.Peer == info.Peer {
				dup = true
				break
			}
		}
		if !dup {
			e.camp.Intercepts = append(e.camp.Intercepts, config.Intercept{
				Spec: info.Spec,
				Peer: info.Peer,
			})
			e.persistCampLocked()
		}
	}
	return info, nil
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

// RemoveIntercept deletes all routes installed for the given entry ID
// and drops the matching (spec, peer) entry from camp config.
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
	if e.camp != nil {
		kept := e.camp.Intercepts[:0]
		for _, it := range e.camp.Intercepts {
			if it.Spec == info.Spec && it.Peer == info.Peer {
				continue
			}
			kept = append(kept, it)
		}
		e.camp.Intercepts = kept
		e.persistCampLocked()
	}
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

		// Round-trip ping/pong with peers. Handled before the IP-shape
		// filter so its JSON payload (first byte '{', "version"=0x7)
		// doesn't trip the drop log below. Counts as a LastSeen signal
		// because we ran the identification above first.
		if e.pinger != nil && e.pinger.HandlePacket(pkt, from) {
			continue
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
	if e.fw != nil {
		_ = e.fw.Close()
		e.fw = nil
	}
	if e.egr != nil {
		_ = e.egr.Close()
		e.egr = nil
	}
	e.announce = nil
	e.poller = nil
	e.pinger = nil
}

func sameUDPAddr(a, b *net.UDPAddr) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Port == b.Port && a.IP.Equal(b.IP)
}
