// Package engine owns the tunnel runtime: utun, UDP, routes, and (optionally)
// egress NAT setup. It exposes a Start/Stop lifecycle plus methods to mutate
// the intercept list while running, so that both the CLI and the web UI can
// drive the same core.
package engine

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/vseplet/f2f/source/helper/ca"
	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/engine/egress"
	"github.com/vseplet/f2f/source/helper/engine/awg"
	"github.com/vseplet/f2f/source/helper/engine/obfenv"
	"github.com/vseplet/f2f/source/helper/engine/pair"
	"github.com/vseplet/f2f/source/helper/identity"
	internaltorrent "github.com/vseplet/f2f/source/helper/torrent"
	"github.com/vseplet/f2f/source/helper/engine/overlay"
	"github.com/vseplet/f2f/source/helper/engine/packet"
	"github.com/vseplet/f2f/source/helper/engine/rendezvous"
	"github.com/vseplet/f2f/source/helper/engine/route"
	"github.com/vseplet/f2f/source/helper/engine/tunnel"
	"github.com/vseplet/f2f/source/helper/platform"
)

// tunnelSubnetCIDR is the CGNAT /10 the overlay carves per-peer
// landing pads out of. Each mac picks its own alias in here
// deterministically from its pub (see overlay.PubToV4Addr). Hardcoded
// — camp's
// hub uses the same prefix when allocating tunnel_ips.
const tunnelSubnetCIDR = overlay.V4Subnet

// packetLogEnabled gates per-packet tunnel logging (the [utun]/[udp]
// per-packet lines). Off by default — these flood the log and bury
// everything else. Enable with F2F_PACKET_LOG=1.
var packetLogEnabled = os.Getenv("F2F_PACKET_LOG") == "1"

func packetLog(format string, args ...any) {
	if packetLogEnabled {
		log.Printf(format, args...)
	}
}

// awgDebugEnabled gates AWG-integration diagnostics: every multiplex
// decision in peerToTunLoop, UAPI blobs sent to Device on SyncPeers,
// slot ranges at startup. Enable with F2F_AWG_DEBUG=1.
var awgDebugEnabled = os.Getenv("F2F_AWG_DEBUG") == "1"

func awgDebug(format string, args ...any) {
	if awgDebugEnabled {
		log.Printf(format, args...)
	}
}

// CampConfig points the engine at a rendezvous (camp) server: instead of
// the user supplying the peer's UDP endpoint via --peer, we discover our
// own external endpoint via STUN, register with camp under (Name, ID),
// and adopt the other peer in the same camp when it announces an endpoint.
type CampConfig struct {
	URL      string // wss://f2f-camp.fly.dev/ws
	Name     string // our identity within the camp
	ID       string // shared camp id; empty triggers the "create new camp" path (Label required)
	Label    string // human-friendly camp label, used only when ID is empty to derive ID = <pub>_<label>
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
	CampActive   bool   `json:"camp_active"`
	CampURL      string `json:"camp_url,omitempty"`
	CampName     string `json:"camp_name,omitempty"`
	CampID       string `json:"camp_id,omitempty"`
	// CampLabel is the human-friendly suffix of CampID — what we use
	// as the DNS zone (`<label>.f2f`) and what the UI shows. For new
	// camps CampID is `<creator_pub_hex>_<label>`; for legacy free-form
	// camps it equals CampID itself.
	CampLabel    string `json:"camp_label,omitempty"`
	CampPeerName string `json:"camp_peer_name,omitempty"` // active peer's name (display alias)
	CampReflex   string `json:"camp_reflex,omitempty"`    // our own external endpoint per STUN
	// Identity (Ed25519) for the running camp. Pub is the full 32-byte
	// public key in hex; Fingerprint is the short SHA-256 prefix the
	// UI shows. Empty in static --peer mode.
	IdentityPub string `json:"identity_pub,omitempty"`
	IdentityFP  string `json:"identity_fp,omitempty"`
	// ActivePeerPub is the user-selected peer the tunnel routes
	// catch-all traffic through. Empty when no one has been selected.
	ActivePeerPub string `json:"active_peer_pub,omitempty"`
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
	PublicIP    string        `json:"public_ip,omitempty"`
	UDPPort     int           `json:"udp_port,omitempty"`
	UDPEndpoint string        `json:"udp_endpoint,omitempty"`
	JoinedAt    int64         `json:"joined_at,omitempty"`
	LastSeenMs  int64         `json:"last_seen_ms,omitempty"` // ms since last packet; 0 = never
	Online      bool          `json:"online"`                 // camp-side: announced recently
	Reachable   bool          `json:"reachable"`              // local: receiving UDP from this peer
	Active      bool          `json:"active"`
	Self        bool          `json:"self,omitempty"`
	Files       []PeerFile    `json:"files,omitempty"`
	// Firewall lists the peer's user-published open ports (without
	// built-ins). Polled from their tunnel-side /api/firewall.
	Firewall []config.Firewall `json:"firewall,omitempty"`
	// InCamp = camp server confirms peer is alive in its roster
	// (sent announce within ~60s). This is independent of whether
	// we can reach the peer ourselves — the Online flag above is the
	// local reachability view (we received UDP from them recently).
	InCamp bool `json:"in_camp"`
	// LastPongMs is the wall-clock ms of the most recent crypto-attested
	// signal from this peer (= LastValidResMs — last valid pair_res to
	// our pair_req). 0 = never. Kept under the historical "Pong" name
	// for UI backward compat; semantically it's now "last pair_res".
	LastPongMs int64 `json:"last_pong_ms,omitempty"`
	RTTMs      int64 `json:"rtt_ms,omitempty"`
	// Verified is an alias for Paired below — kept for UI backward compat.
	// New UI code should switch to Paired / HalfPaired.
	Verified bool `json:"verified"`
	// Paired = both pair_req AND pair_res are fresh (<30s). The strict
	// "🟢 paired" status — bidirectional crypto-attestation confirmed.
	Paired bool `json:"paired"`
	// HalfPaired = exactly one of pair_req / pair_res is fresh. The
	// "🟡 half-paired" status — connection works one way only (e.g. we
	// hear them, they don't hear us, or vice versa).
	HalfPaired bool `json:"half_paired"`
	// OverlayV4 is the per-peer 100.64.X.Y address derived from pub.
	// Present for any peer whose Pub is known; empty for legacy peers
	// announced without a pub. Used for BT peer addresses and display.
	OverlayV4 string `json:"overlay_v4,omitempty"`
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
	// Pub is the peer's Ed25519 hex pubkey — stable identity, also
	// used as the e.peers map key.
	Pub         string
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
	Files      []PeerFile        // populated by filesPollLoop
	Firewall   []config.Firewall // populated by peerFirewallPollLoop

	UDPAddr      *net.UDPAddr // current best-known UDP target (port can shift on NAT rebind)
	LastSeenMs   atomic.Int64 // epoch ms of last received packet from this peer; 0 = never
	LastPingMs   atomic.Int64 // epoch ms of last punch/keepalive we sent

	// WGPub is the peer's X25519 transport pubkey, learned from a verified
	// hello-handshake (engine/hello). Empty until first valid hello arrives
	// — engine treats empty as "no AWG handshake possible yet, peer hasn't
	// announced its WG identity". Mutated under e.mu.
	WGPub string

	// Pair-handshake state (engine/pair). All atomic — read by status
	// builders without holding e.mu.
	//
	//   LastValidReqMs — last valid pair_req received from this peer.
	//                    Bumped after signature verification in handlePairReq.
	//   LastValidResMs — last valid pair_res received from this peer that
	//                    echoed one of our own sent_ms.
	//   LastSentReqMs  — sent_ms of the most recent pair_req WE sent to
	//                    this peer. Used to match incoming pair_res echoes;
	//                    a res with EchoMs != LastSentReqMs is treated as
	//                    stale (e.g. crossed with NAT rebind) and ignored
	//                    for RTT purposes (still bumps LastValidResMs).
	//   LastRTTMs      — last round-trip time in ms, computed from a
	//                    pair_res whose echo_ms matched LastSentReqMs.
	//
	// "Paired" = both LastValidReqMs and LastValidResMs are fresh — see
	// PeerStatusInfo for the actual threshold.
	LastValidReqMs atomic.Int64
	LastValidResMs atomic.Int64
	LastSentReqMs  atomic.Int64
	LastRTTMs      atomic.Int64
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

// pairFreshMs is the freshness window for pair-handshake signals. Same
// 30s as peerOnlineWindowMs — pair_req keepalive is 25s in steady state,
// so 30s comfortably covers one missed packet without flapping.
const pairFreshMs = 30000

// IsPaired reports whether we have a crypto-attested bidirectional
// connection with this peer right now: their pair_req arrived recently
// AND our pair_req got answered by their pair_res recently. Both
// signals together ≈ "🟢 paired" in the UI.
func (p *peerState) IsPaired() bool {
	if p == nil {
		return false
	}
	now := time.Now().UnixMilli()
	req := p.LastValidReqMs.Load()
	res := p.LastValidResMs.Load()
	return req > 0 && now-req < pairFreshMs && res > 0 && now-res < pairFreshMs
}

// IsHalfPaired reports specifically: their pair_req reaches us
// recently, but our pair_req gets no fresh pair_res from them. This
// is "they're alive and trying to talk to me, but I can't confirm
// the path back" — the meaningful orange state.
//
// We deliberately do NOT treat the inverse (res fresh, req stale) as
// half-paired. If their pair_req keepalive stopped, the peer is
// effectively gone from our side — leftover res from earlier rounds
// is stale data, not a "half-working" connection. That case maps to
// 🔴 unreachable.
func (p *peerState) IsHalfPaired() bool {
	if p == nil {
		return false
	}
	now := time.Now().UnixMilli()
	req := p.LastValidReqMs.Load()
	res := p.LastValidResMs.Load()
	reqFresh := req > 0 && now-req < pairFreshMs
	resFresh := res > 0 && now-res < pairFreshMs
	return reqFresh && !resFresh
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
	ca       *ca.CA                  // local CA for the current camp_id
	torrent  *internaltorrent.Client // BT client for camp file sharing
	announce *rendezvous.AnnounceClient // periodic UDP announce → camp
	poller   *rendezvous.PeerListPoller // periodic HTTP peer-list poll
	// obfenv carries the camp-wide obfuscation parameters (camp_key, magic
	// header ranges H1..H8) derived deterministically from cfg.Camp.ID.
	// Built once at Start in camp mode; nil in static --peer mode.
	obfenv *obfenv.Camp
	// awgBind is the conn.Bind implementation that amneziawg-go's Device
	// uses to send/receive its packets over our shared UDP socket.
	// peerToTunLoop forwards H1..H4-magic packets into it via Deliver.
	awgBind *awg.Bind
	// awgDevice is the amneziawg-go device. When non-nil it OWNS utun:
	// engine must not Read/Write the underlying TUN fd directly while
	// awgDevice is active. Peers are pushed into it via SyncPeers,
	// triggered by pair-handshake success and camp polls.
	awgDevice *awg.Device
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
	// activePub is the user-selected peer the tunnel routes
	// catch-all traffic through. Direct peer-to-peer-tunnel-IP packets
	// still flow regardless of selection.
	activePub atomic.Pointer[string]
	// staticPeer is the legacy --peer mode endpoint (no camp). Kept for
	// backwards compat with the few static deployments; new code paths
	// should use the peers map.
	staticPeer       atomic.Pointer[net.UDPAddr]
	lastStaticPingMs atomic.Int64

	intercepts map[string]*InterceptInfo
	nextItemID uint64

	// tunnelHTTPPort is the port other peers expose their /api/domains
	// on (= our UI bind port, since both sides run f2f-mac). Wired by
	// main via SetTunnelHTTPPort.
	tunnelHTTPPort string

	// defaultListen is the UDP address autostart binds the peer
	// transport to. Wired by main via SetDefaultListen (default
	// ":9000") so multiple helpers on one host can pick disjoint
	// ports without recompiling.
	defaultListen string

	cancel  context.CancelFunc
	workers sync.WaitGroup
	started time.Time

	txBytes, rxBytes     atomic.Uint64
	txPackets, rxPackets atomic.Uint64

	tap *logTap

	// call holds the active group call state (SFU + participants).
	// nil when no call is in progress.
	call atomic.Value // *callCtx
	// remoteCalls holds calls discovered on remote peers via polling.
	remoteCalls atomic.Value // *[]CallState
	// joinedSFUHost is the tunnel IP of the remote SFU we joined.
	// Empty when not in a remote call.
	joinedSFUHost atomic.Value // *string

	// OnLocalSFUSignal, when set, delivers SFU signals destined for the
	// local browser directly (bypassing HTTP to tunnel IP).
	OnLocalSFUSignal func(msg []byte)

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

// PeerFile is one file entry from a peer's /api/files response,
// rehydrated into our shape (Path stripped — peer-facing data only).
type PeerFile struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	InfoHash string `json:"info_hash"`
	Magnet   string `json:"magnet"`
}

// New returns a fresh Engine with the given config Store. The Store
// is shared with services that need to persist their own slices of
// camp config (firewall rules, trusted peers, MyDomains, ...).
// Passing the Store explicitly lets main.go own the lifecycle and
// keeps the engine from being the sole gatekeeper of disk state.
func New(store *config.Store) *Engine {
	return &Engine{
		store:      store,
		intercepts: map[string]*InterceptInfo{},
		peers:      map[string]*peerState{},
		tap:        newLogTap(),
	}
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
		if cfg.Camp.URL == "" || cfg.Camp.Name == "" || cfg.Camp.StunAddr == "" {
			return errors.New("Camp.{URL,Name,StunAddr} all required")
		}
		// camp_id is optional iff we have a label — empty id means
		// "create a new camp": generate identity first, derive id from
		// the pub. ID populated here so the rest of Start sees a normal
		// fully-formed CampConfig.
		if cfg.Camp.ID == "" {
			label := strings.TrimSpace(cfg.Camp.Label)
			if label == "" {
				return errors.New("camp create: Camp.Label required when ID is empty")
			}
			if !validCampLabel(label) {
				return errors.New("camp create: Label must match [A-Za-z0-9_.-]+")
			}
			id, err := identity.Generate()
			if err != nil {
				return fmt.Errorf("camp create: identity: %w", err)
			}
			cfg.Camp.ID = id.PubHex() + "_" + label
			idDir := filepath.Join("/var/lib/f2f/identity", cfg.Camp.ID)
			if err := id.Save(idDir); err != nil {
				return fmt.Errorf("camp create: save identity: %w", err)
			}
			e.identity = id
			log.Printf("camp create: new camp id=%s pub=%s", cfg.Camp.ID, id.PubHex())
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
		// If we already created+saved one above (camp-create path),
		// LoadOrGenerate finds it on disk and returns it.
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

		// Camp-wide obfuscation: camp_key + magic-header ranges (H1..H8),
		// derived deterministically from camp_id. Every member of this
		// camp computes the same values; camp_id never leaves the invite
		// chain so an outside observer can't derive them.
		e.obfenv = obfenv.NewCamp(cfg.Camp.ID)
		log.Printf("pair: control envelope ready for camp %s", cfg.Camp.ID)
	}

	// Egress goes first so its rollback runs last on the way down.
	// Empty EgressIface means: auto-pick the default route's interface.
	// We always run egress in camp mode — the tunnel is useless without
	// a path to the internet.
	if cfg.EgressIface == "" {
		if iface, err := platform.DefaultEgressInterface(); err != nil {
			log.Printf("egress: %v; skipping NAT (peers won't reach internet through this node)", err)
		} else {
			cfg.EgressIface = iface
		}
	}
	if cfg.EgressIface != "" {
		subnet := netip.MustParsePrefix(tunnelSubnetCIDR)
		egr, err := egress.Open(cfg.EgressIface, subnet)
		if err != nil {
			return fmt.Errorf("egress setup: %w", err)
		}
		e.egr = egr
		log.Printf("egress: NAT %s → %s, ip-forwarding=1", subnet, cfg.EgressIface)
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
		// AWG bind sits in front of the shared UDP socket. At this step
		// Device isn't created yet — Bind just buffers any inbound AWG-
		// shaped packets that peerToTunLoop forwards via Deliver. The
		// next step (Device wiring) will Open() this Bind and start
		// draining the inbox.
		if e.obfenv != nil {
			e.awgBind = awg.New(e.udp)
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
		e.announce = ac
		e.campAddr.Store(ac.CampAddr())
		reflex := self.UDPEndpoint
		if reflex == "" {
			reflex = self.PublicIP
		}
		e.campReflex.Store(&reflex)
		// v4 alias is derived from our own pub — unique per-peer so
		// intercept replies routed back through this overlay don't
		// land on the egress peer's own utun (would happen with a
		// shared address). Camp-assigned tunnel_ip is ignored.
		if e.identity != nil {
			a, derr := overlay.PubToV4Addr(e.identity.PubHex())
			if derr != nil {
				e.rollbackPartial()
				return fmt.Errorf("derive v4 alias: %w", derr)
			}
			localIP = a.String()
		}
		log.Printf("camp: registered as %s in camp %s, reflex=%s (utun v4 alias=%s)", cfg.Camp.Name, cfg.Camp.ID, reflex, localIP)
	}

	// utun. In Camp mode the interface owns the whole 10.99.0.0/24
	// overlay; static mode keeps the legacy point-to-point form.
	var tun *tunnel.Tunnel
	if cfg.Camp != nil {
		t, err := tunnel.OpenSubnet(localIP, 10)
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

	// AWG device: takes exclusive ownership of utun + our Bind. After
	// this returns, engine MUST NOT call tun.Read / tun.Write directly
	// — Device reads outgoing IP packets from utun, encrypts them,
	// hands them to Bind.Send; the reverse path is Bind.Deliver →
	// Device decrypts → utun.Write. No peers yet; awgSyncPeers below
	// pushes them in once pair-handshake verifies them.
	if cfg.Camp != nil && e.awgBind != nil && e.identity != nil && e.obfenv != nil {
		awgDev, err := awg.Start(tun.Device(), e.awgBind, e.identity, e.obfenv)
		if err != nil {
			e.rollbackPartial()
			return fmt.Errorf("awg device: %w", err)
		}
		e.awgDevice = awgDev
		log.Printf("awg: device up — encrypted transport ready for paired peers")
		// Diagnostic snapshot of derived parameters — both peers must see
		// the SAME values for AWG packets to be classifiable on receive.
		// Camp_id derives all of these; if two ends see different camp_id
		// strings their slot ranges and magic headers diverge and traffic
		// silently drops at our discriminator. Enable with F2F_AWG_DEBUG=1
		// — and even without the flag we always log the snapshot once at
		// startup since it's a single line and useful for verification.
		for slot, name := range []string{"h1", "h2", "h3", "h4"} {
			start, end := e.obfenv.SlotRange(obfenv.Slot(slot))
			log.Printf("awg: %s slot [0x%08x..0x%08x) configured magic=%d", name, start, end, start)
		}
	}

	// Inbound utun firewall is now owned by services/firewall and
	// installed by main.go after eng.OnStarted — keeps OS-touching
	// lifecycle (utun, UDP, AWG) inside engine, and userland services
	// (CRUD, persist, pf wiring) outside.

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

	// tunToPeerLoop reads outgoing IP packets from utun and sends
	// them as plaintext UDP to peers. When AWG device is active, it
	// owns utun and does this job (with encryption) — running our
	// loop in parallel would race on the same fd.
	if e.awgDevice == nil {
		e.workers.Add(1)
		go e.tunToPeerLoop(ctx)
	}
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
	if e.cfg.Camp != nil {
		// Domain catalog poll + service health-check live in services/dns now.
		e.workers.Add(1)
		go e.filesPollLoop(ctx)
		e.workers.Add(1)
		go e.peerFirewallPollLoop(ctx)
		e.workers.Add(1)
		go e.callPollLoop(ctx)
	}
	// Local DNS resolver, MyDomains catalog, peer-domain poll, and
	// service-health check are owned by services/dns now — main.go
	// drives the lifecycle off eng.OnStarted/OnStopped.
	// Local CA for HTTPS termination. Persisted under /var/lib/f2f/ca
	// so it survives restarts. Regenerated whenever camp_id changes
	// (NameConstraints in the cert pin it to one zone). Failures here
	// are non-fatal — HTTPS just won't work, HTTP still does.
	if e.cfg.Camp != nil {
		if err := e.ensureCA(); err != nil {
			log.Printf("ca: %v (https disabled)", err)
		}
		// Peer-CA discovery + keychain install was moved to
		// services/trust — main.go owns the lifecycle, hooked off
		// OnStarted/OnStopped so the per-camp on-disk cache is
		// reloaded on every Start.
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
	// Drop e.mu before invoking the OnStarted callback — handlers
	// commonly call back into the engine (Status, CampFirewall,
	// TrustedPeersDir, ...) which all acquire e.mu, and holding it
	// across user code would deadlock. The deferred Unlock at the
	// top of Start still balances correctly: Unlock here, Lock back
	// to satisfy the defer's no-op release.
	cb := e.OnStarted
	if cb != nil {
		e.mu.Unlock()
		cb(cfg.LocalIP)
		e.mu.Lock()
	}
	return nil
}

// caDir is where ca.crt/ca.key are persisted.
const caDir = "/var/lib/f2f/ca"

// ensureCA loads the on-disk CA, regenerates it if missing or pinned to
// a different camp_id, and installs the cert into the system trust store.
// Idempotent: safe to call repeatedly on Start.
func (e *Engine) ensureCA() error {
	loaded, err := ca.Load(caDir)
	if err != nil {
		log.Printf("ca: load: %v (will regenerate)", err)
		loaded = nil
	}
	if loaded != nil && !loaded.MatchesZone(identity.CampLabel(e.cfg.Camp.ID)) {
		log.Printf("ca: existing CA pinned to a different camp_id; rotating")
		_ = loaded.RemoveSystemTrust()
		loaded = nil
	}
	if loaded == nil {
		fresh, err := ca.Generate(identity.CampLabel(e.cfg.Camp.ID))
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
	if loaded.IsSystemTrusted() {
		log.Printf("ca: already in system trust store (fp %s) — skipping install", loaded.Fingerprint())
	} else if err := loaded.EnsureSystemTrust(ca.CertPath(caDir)); err != nil {
		log.Printf("ca: install in system trust store: %v (https will show warnings)", err)
	} else {
		log.Printf("ca: installed in system trust store (fp %s)", loaded.Fingerprint())
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
// BT client uses. Both are per-camp: a torrent only makes sense in the
// camp it was added to (peer tunnel_ips are camp-local), and mixing
// state across camps causes stale-peer dial loops after switching.
//
// shared lives under the platform's app-support dir (under camp_id, so
// new-format ids with a 64-hex prefix get full isolation). downloads
// goes to a user-visible ~/Downloads/f2f-drops/<short> so people can
// open it from their file manager without staring at hex.
func (e *Engine) torrentSharedDir() string {
	home := userHome()
	return filepath.Join(platform.AppSupportDir(home), "f2f", e.campStateDirSegment(), "shared")
}

func (e *Engine) torrentDownloadsDir() string {
	home := userHome()
	return filepath.Join(home, "Downloads", "f2f-drops", e.campUserVisibleSegment())
}

// campStateDirSegment returns the camp_id used as a directory segment
// for internal state. Empty (legacy --peer mode) falls back to "_root"
// so we never write to the bare app-support root.
func (e *Engine) campStateDirSegment() string {
	if e.cfg.Camp == nil || e.cfg.Camp.ID == "" {
		return "_root"
	}
	return e.cfg.Camp.ID
}

// campUserVisibleSegment returns a human-friendly directory segment
// for user-visible paths (~/Downloads/f2f-drops/...). For new-format
// camp_ids "<64hex>_<label>" we return "<label>_<8hex>" — readable, but
// disambiguated when two camps share a label. Legacy ids are used
// as-is.
func (e *Engine) campUserVisibleSegment() string {
	if e.cfg.Camp == nil || e.cfg.Camp.ID == "" {
		return "_root"
	}
	id := e.cfg.Camp.ID
	label := identity.CampLabel(id)
	if label == id {
		return label // legacy free-form id
	}
	return label + "_" + id[:8]
}

// userHome returns the home of the invoking (non-root) user. Engine
// runs as root via sudo, but files should be owned/visible to the
// user. Resolves via SUDO_USER through the OS user database so the
// real path comes out (/Users/<name> on macOS, /home/<name> on
// linux).
func userHome() string {
	if su := os.Getenv("SUDO_USER"); su != "" {
		if u, err := user.Lookup(su); err == nil && u.HomeDir != "" {
			return u.HomeDir
		}
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
	// Bind the BT listener on the v4 overlay alias — that's the address
	// other peers in the camp reach us at through utun.
	host := e.cfg.LocalIP
	t0 := time.Now()
	opts := internaltorrent.Options{
		ListenAddr:   net.JoinHostPort(host, fmt.Sprint(internaltorrent.DefaultPort)),
		SharedDir:    e.torrentSharedDir(),
		DownloadsDir: e.torrentDownloadsDir(),
	}
	log.Printf("torrent: binding on %s …", opts.ListenAddr)
	c, err := internaltorrent.New(opts)
	if err != nil {
		return fmt.Errorf("torrent: bind %s: %w", opts.ListenAddr, err)
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
	saved := e.loadSavedDownloads()
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
		if err := e.saveDownloads(keep); err != nil {
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

func (e *Engine) downloadsStatePath() string {
	return filepath.Join(userHome(), "Library", "Application Support", "f2f", e.campStateDirSegment(), "downloads.json")
}

func (e *Engine) loadSavedDownloads() []savedDownload {
	path := e.downloadsStatePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []savedDownload
	if err := json.Unmarshal(data, &out); err != nil {
		log.Printf("downloads: parse %s: %v", path, err)
		return nil
	}
	return out
}

func (e *Engine) saveDownloads(list []savedDownload) error {
	path := e.downloadsStatePath()
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
	saved := e.loadSavedDownloads()
	for _, s := range saved {
		if s.InfoHash == d.InfoHash {
			return d, nil // already remembered
		}
	}
	saved = append(saved, savedDownload{
		Magnet: magnet, InfoHash: d.InfoHash, Peers: peers,
	})
	if err := e.saveDownloads(saved); err != nil {
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
	saved := e.loadSavedDownloads()
	kept := saved[:0]
	for _, s := range saved {
		if s.InfoHash == infoHash {
			continue
		}
		kept = append(kept, s)
	}
	if len(kept) != len(saved) {
		if err := e.saveDownloads(kept); err != nil {
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
	saved := e.loadSavedDownloads()
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

// trustedPeersRootDir is the parent under which per-camp peer-CA caches
// live. Each camp gets its own subdir keyed by camp_id so CAs from
// camp A don't leak into camp B's "trusted peer CAs" UI panel.
const trustedPeersRootDir = "/var/lib/f2f/trusted-peers"

// TrustedPeersDir returns the per-camp cache dir. Empty cfg.Camp falls
// back to the root path (static --peer legacy mode, no notion of camp).
// Exposed for the services/trust service which owns the on-disk cert
// management.
func (e *Engine) TrustedPeersDir() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cfg.Camp == nil || e.cfg.Camp.ID == "" {
		return trustedPeersRootDir
	}
	return filepath.Join(trustedPeersRootDir, e.cfg.Camp.ID)
}

// CampFirewall returns a copy of the camp config's user-configured
// firewall allow list. Returns nil when the engine isn't running
// (no camp loaded → no list to surface).
func (e *Engine) CampFirewall() []config.Firewall {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.camp == nil {
		return nil
	}
	out := make([]config.Firewall, len(e.camp.Firewall))
	copy(out, e.camp.Firewall)
	return out
}

// SetCampFirewall replaces the camp config's firewall list and
// persists the change to disk. Caller is expected to have already
// validated the list (see services/firewall.CleanList).
// Returns an error if the engine isn't running, since the camp
// config is keyed by camp_id.
func (e *Engine) SetCampFirewall(list []config.Firewall) error {
	e.mu.Lock()
	if !e.running || e.camp == nil {
		e.mu.Unlock()
		return errors.New("engine not running")
	}
	e.camp.Firewall = append([]config.Firewall(nil), list...)
	e.persistCampLocked()
	e.mu.Unlock()
	return nil
}

// TunnelHTTPPort is the port other peers expose their /api/* on over
// utun (same value we host the UI on). Empty when the engine wasn't
// started with a UI bind. Exposed for services/trust and any future
// peer-poll service.
func (e *Engine) TunnelHTTPPort() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tunnelHTTPPort
}

// OnlinePeerHTTPInfo is one peer reachable over utun for any
// service-level poll loop (CA poll, domain poll, etc.). The shape is
// intentionally minimal — just what a poller needs to dial the peer
// and key things back to the camp catalog.
type OnlinePeerHTTPInfo struct {
	Pub  string // Ed25519 hex pubkey, stable identity (used as map key)
	Name string
	Host string // overlay v4 string
}

// OnlinePeersForCAPoll returns the snapshot of currently-online peers
// in the shape services/trust needs. Filters out peers without a
// deriveable HTTP host (no Pub yet).
func (e *Engine) OnlinePeersForCAPoll() []OnlinePeerHTTPInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]OnlinePeerHTTPInfo, 0, len(e.peers))
	for _, p := range e.peers {
		if !p.IsOnline() {
			continue
		}
		host := e.peerHTTPHostLocked(p)
		if host == "" {
			continue
		}
		out = append(out, OnlinePeerHTTPInfo{Pub: p.Pub, Name: p.Name, Host: host})
	}
	return out
}

// UpsertTrustedPeerInCamp upserts (by fingerprint) the trusted-CA
// metadata into the active camp config and persists. PEM bytes stay
// under TrustedPeersDir — config carries only fingerprint + display
// fields so the UI can list and remove CAs without re-reading the
// trust store. No-op when the engine isn't running.
func (e *Engine) UpsertTrustedPeerInCamp(t config.TrustedPeer) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.camp == nil {
		return
	}
	for i, ex := range e.camp.TrustedPeers {
		if ex.Fingerprint == t.Fingerprint {
			e.camp.TrustedPeers[i] = t
			e.persistCampLocked()
			return
		}
	}
	e.camp.TrustedPeers = append(e.camp.TrustedPeers, t)
	e.persistCampLocked()
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
		if p.Name == ourName {
			continue
		}
		// Pub is the stable identity — peers without one are skipped
		// (transitional case: camp roster has the entry but the peer
		// hasn't yet announced an Ed25519 pub). They'll show up once
		// they do.
		if p.Pub == "" {
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
		existing, ok := e.peers[p.Pub]
		if !ok {
			st := &peerState{
				Name:        p.Name,
				Pub:         p.Pub,
				PublicIP:    p.PublicIP,
				UDPPort:     p.UDPPort,
				UDPEndpoint: p.UDPEndpoint,
				JoinedAt:    p.JoinedAt,
				InCamp:      p.Online,
				LastSeenAt:  p.LastSeenAt,
				UDPAddr:     addr,
			}
			e.peers[p.Pub] = st
			if p.Online {
				log.Printf("camp: peer %s @ %s entered roster (pub=%s)", p.Name, addr, p.Pub)
			} else {
				log.Printf("camp: peer %s in roster but stale (pub=%s)", p.Name, p.Pub)
			}
		} else {
			existing.Name = p.Name
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
					log.Printf("camp: peer %s back in roster (pub=%s)", p.Name, p.Pub)
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
					log.Printf("camp: peer %s left roster (pub=%s)", p.Name, p.Pub)
				}
			}
			existing.InCamp = p.Online
		}
		seen[p.Pub] = struct{}{}
	}
	// Peers not in the latest poll: camp dropped them from the roster
	// entirely (binding expired on their side). We KEEP them in e.peers
	// as offline ghosts so the UI shows historical nodes — same as if
	// camp still reported them with Online=false. holePunchLoop already
	// skips peers without UDPAddr, so this is safe.
	active := e.activePub.Load()
	for pub, st := range e.peers {
		if _, alive := seen[pub]; alive {
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
		if active != nil && *active == pub {
			e.activePub.Store(nil)
		}
	}
	// Merge the snapshot into the persistent catalog so the UI sees
	// known nodes (incl. currently-offline) on the next engine start.
	if e.camp != nil {
		e.mergePeerSnapshotLocked(peers)
		e.persistCampLocked()
	}
	// Camp poll may have refreshed peer endpoints (NAT rebind) or
	// added/removed peers entirely. Refresh the AWG peer list so
	// routing and allowed_ips stay current. Async to keep poller fast.
	if e.awgDevice != nil {
		go e.awgSyncPeers()
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
		host string
		pub  string
		name string
	}
	var targets []target
	e.mu.Lock()
	for pub, p := range e.peers {
		if !p.IsOnline() {
			continue
		}
		targets = append(targets, target{host: e.peerHTTPHostLocked(p), pub: pub, name: p.Name})
	}
	port := e.tunnelHTTPPort
	e.mu.Unlock()
	if port == "" {
		return
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for _, t := range targets {
		url := "http://" + net.JoinHostPort(t.host, port) + "/api/files"
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
		if p, ok := e.peers[t.pub]; ok {
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
		host string
		pub  string
		name string
	}
	var targets []target
	e.mu.Lock()
	for pub, p := range e.peers {
		if !p.IsOnline() {
			continue
		}
		targets = append(targets, target{host: e.peerHTTPHostLocked(p), pub: pub, name: p.Name})
	}
	port := e.tunnelHTTPPort
	e.mu.Unlock()
	if port == "" {
		return
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for _, t := range targets {
		url := "http://" + net.JoinHostPort(t.host, port) + "/api/firewall"
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
			User []config.Firewall `json:"user"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		e.mu.Lock()
		if p, ok := e.peers[t.pub]; ok {
			p.Firewall = body.User
		}
		e.persistPeerFirewallLocked(t.pub, body.User)
		e.mu.Unlock()
	}
}

// persistPeerFirewallLocked mirrors a peer's published firewall list
// into the camp catalog. Caller holds e.mu.
func (e *Engine) persistPeerFirewallLocked(pub string, fw []config.Firewall) {
	if e.camp == nil || pub == "" {
		return
	}
	out := append([]config.Firewall(nil), fw...)
	for i := range e.camp.PeerCatalog {
		if e.camp.PeerCatalog[i].Pub == pub {
			e.camp.PeerCatalog[i].Firewall = out
			e.persistCampLocked()
			return
		}
	}
}

// Domain catalog poll + service health-check moved to services/dns.

func (e *Engine) SetTunnelHTTPPort(port string) {
	e.tunnelHTTPPort = port
}

// SetDefaultListen wires the UDP listen address autostart should use
// when bringing the last camp up. Empty falls back to ":0" so the
// kernel picks a free port — peers learn our reflex after NAT, so
// the local port number does not need to be fixed or coordinated.
func (e *Engine) SetDefaultListen(addr string) {
	e.defaultListen = addr
}

// awgSyncPeers snapshots the current verified-peer set and pushes it
// into the AWG device via IpcSet. No-op when AWG isn't active (static
// mode or pre-Start). Called whenever the peer set or any peer's
// endpoint changes — typically after a successful pair-handshake or
// a camp poll. Runs the IpcSet outside e.mu to avoid blocking other
// engine paths.
func (e *Engine) awgSyncPeers() {
	e.mu.Lock()
	if e.awgDevice == nil {
		e.mu.Unlock()
		return
	}
	peers := make([]awg.PeerSyncInfo, 0, len(e.peers))
	for _, p := range e.peers {
		if p.WGPub == "" || p.UDPAddr == nil {
			continue
		}
		overlayAddr, err := overlay.PubToV4Addr(p.Pub)
		if err != nil {
			continue
		}
		// AllowedIPs = peer's overlay /32 + every intercept prefix bound to
		// this peer in e.intercepts. Without the intercepts here AWG drops
		// outgoing traffic to those destinations because no peer's trie
		// entry matches the dst, and we lose the egress-via-peer feature
		// that worked in the old plaintext routeFor() path.
		cidrs := make([]string, 0, 1+8)
		cidrs = append(cidrs, overlayAddr.String()+"/32")
		for _, info := range e.intercepts {
			if info.Peer != p.Name {
				continue
			}
			for _, pref := range info.Prefixes {
				// IPv6 entries get a " (reject)" annotation in the
				// engine's bookkeeping — strip it for UAPI.
				pref = strings.TrimSuffix(pref, " (reject)")
				if pref == "" {
					continue
				}
				cidrs = append(cidrs, pref)
			}
		}
		peers = append(peers, awg.PeerSyncInfo{
			WGPub:        p.WGPub,
			Endpoint:     p.UDPAddr.String(),
			AllowedCIDRs: cidrs,
		})
	}
	dev := e.awgDevice
	e.mu.Unlock()
	awgDebug("awg sync peers: %d peers", len(peers))
	for _, p := range peers {
		awgDebug("  peer wg_pub=%s endpoint=%s allowed=%v",
			p.WGPub[:16], p.Endpoint, p.AllowedCIDRs)
	}
	if err := dev.SyncPeers(peers); err != nil {
		log.Printf("awg sync peers: %v", err)
	}
}

// pairReqPacket builds a fresh pair_req sealed in a control-envelope.
// Unlike hello, the inner JSON is rebuilt and re-signed every call
// because sent_ms (and therefore the canonical signed bytes) changes.
// Cost: ~80μs Ed25519 sign per call — negligible at burst (1Hz/peer)
// and keepalive (25s/peer) cadences.
//
// Returns nil + error when we're not in camp mode (no obfenv, no
// identity) — caller falls back to skipping pair_req.
func (e *Engine) pairReqPacket(sentMs int64) ([]byte, error) {
	if e.cfg.Camp == nil || e.identity == nil || e.obfenv == nil {
		return nil, errors.New("pair_req: not in camp mode")
	}
	reqJSON, err := pair.BuildReq(e.identity, e.cfg.Camp.Name, sentMs)
	if err != nil {
		return nil, fmt.Errorf("pair_req build: %w", err)
	}
	return e.obfenv.Seal(obfenv.SlotHello, reqJSON)
}

// handlePairReq applies a verified pair_req to engine state, then
// immediately sends a pair_res back. The response is fire-on-receive,
// not scheduled — that's what gives the requester a clean RTT
// measurement (echo of their sent_ms with our process time as the only
// delay).
func (e *Engine) handlePairReq(req pair.Req, from *net.UDPAddr) {
	now := time.Now().UnixMilli()
	e.mu.Lock()
	p, ok := e.peers[req.Pub]
	if !ok {
		e.mu.Unlock()
		log.Printf("pair_req: from non-member pub=%s name=%q at %s — drop",
			identity.FingerprintHex(req.Pub), req.Name, from)
		return
	}
	switch {
	case p.WGPub == "":
		log.Printf("pair_req: peer %s (fp=%s) wg_pub=%s established",
			req.Name, identity.FingerprintHex(req.Pub), req.WGPub[:16])
	case p.WGPub != req.WGPub:
		log.Printf("pair_req: peer %s rotated wg_pub: %s → %s",
			req.Name, p.WGPub[:16], req.WGPub[:16])
	}
	p.WGPub = req.WGPub
	if !sameUDPAddr(p.UDPAddr, from) {
		log.Printf("pair_req: peer %s UDP %s → %s (NAT rebind?)", req.Name, p.UDPAddr, from)
		p.UDPAddr = from
	}
	e.mu.Unlock()
	p.LastSeenMs.Store(now)
	// First-time signal: this is the visible confirmation in logs that
	// the pair protocol is alive on this peer, independent of whatever
	// hello already established about WGPub.
	firstReq := p.LastValidReqMs.Load() == 0
	if firstReq {
		log.Printf("pair_req: first valid from %s (fp=%s)", req.Name, identity.FingerprintHex(req.Pub))
	}
	p.LastValidReqMs.Store(now)
	// Newly verified peer or first pair_req after restart — refresh
	// the AWG peer list so this peer becomes routable through the
	// encrypted tunnel. Runs asynchronously so we don't block the
	// recv path on IpcSet.
	if firstReq {
		go e.awgSyncPeers()
	}

	// Build and send the response. Our own sent_ms is the response's
	// sent_ms; echo_ms carries the requester's sent_ms verbatim so they
	// can compute RTT on receipt.
	if e.cfg.Camp == nil || e.identity == nil || e.obfenv == nil {
		return
	}
	resJSON, err := pair.BuildRes(e.identity, e.cfg.Camp.Name, now, req.SentMs)
	if err != nil {
		log.Printf("pair_res build: %v", err)
		return
	}
	sealed, err := e.obfenv.Seal(obfenv.SlotHello, resJSON)
	if err != nil {
		log.Printf("pair_res seal: %v", err)
		return
	}
	if _, err := e.udp.WriteToUDP(sealed, from); err != nil {
		log.Printf("pair_res send to %s: %v", from, err)
	}
}

// handlePairRes applies a verified pair_res to engine state. RTT is
// computed only when the response's echo_ms matches our LastSentReqMs
// — that guards against stale/duplicated responses (NAT rebinds,
// retransmits) inflating or deflating our timing.
func (e *Engine) handlePairRes(res pair.Res, from *net.UDPAddr) {
	now := time.Now().UnixMilli()
	e.mu.Lock()
	p, ok := e.peers[res.Pub]
	if !ok {
		e.mu.Unlock()
		log.Printf("pair_res: from non-member pub=%s name=%q at %s — drop",
			identity.FingerprintHex(res.Pub), res.Name, from)
		return
	}
	switch {
	case p.WGPub == "":
		log.Printf("pair_res: peer %s (fp=%s) wg_pub=%s established",
			res.Name, identity.FingerprintHex(res.Pub), res.WGPub[:16])
	case p.WGPub != res.WGPub:
		log.Printf("pair_res: peer %s rotated wg_pub: %s → %s",
			res.Name, p.WGPub[:16], res.WGPub[:16])
	}
	p.WGPub = res.WGPub
	if !sameUDPAddr(p.UDPAddr, from) {
		log.Printf("pair_res: peer %s UDP %s → %s (NAT rebind?)", res.Name, p.UDPAddr, from)
		p.UDPAddr = from
	}
	e.mu.Unlock()
	p.LastSeenMs.Store(now)
	firstRes := p.LastValidResMs.Load() == 0
	p.LastValidResMs.Store(now)

	// RTT only when this response echoes our most recent request. A
	// stale echo (e.g. our LastSentReqMs has already moved past) would
	// give a meaningless number — skip it.
	var rtt int64 = -1
	if res.EchoMs != 0 && res.EchoMs == p.LastSentReqMs.Load() {
		r := now - res.EchoMs
		if r >= 0 && r < 60_000 {
			p.LastRTTMs.Store(r)
			rtt = r
		}
	}
	if firstRes {
		if rtt >= 0 {
			log.Printf("pair_res: first valid from %s (fp=%s) rtt=%dms",
				res.Name, identity.FingerprintHex(res.Pub), rtt)
		} else {
			log.Printf("pair_res: first valid from %s (fp=%s) — echo didn't match LastSentReqMs (stale)",
				res.Name, identity.FingerprintHex(res.Pub))
		}
		// Same trigger as handlePairReq: first valid res confirms the
		// full round-trip; push the peer into the AWG device's routing
		// so traffic to its overlay IP gets encrypted.
		go e.awgSyncPeers()
	}
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
				// Healthy = peer is sending us packets AND our last ping
				// got a pong recently. Either signal stale → burst. This
				// covers the asymmetric case: peer's keepalive reaches us
				// fine but our pings get lost (NAT binding lapsed our
				// way), so receive-only freshness isn't enough.
				if seen != 0 && now-seen < freshMs {
					// pair_res from our own pair_req confirms the path is
					// alive in BOTH directions (our send reaches them, their
					// reply reaches us). Without that, "seen" alone could
					// be a one-way path — keep punching at burst cadence.
					if rs := p.LastValidResMs.Load(); rs != 0 && now-rs < freshMs {
						cadence = keepaliveMs
					}
				}
				if now-lastSent < cadence {
					continue
				}
				// Camp peers get a signed pair_req (sealed in control-envelope).
				// Static --peer mode (no obfenv) is handled below.
				if e.obfenv == nil {
					continue
				}
				reqPkt, perr := e.pairReqPacket(now)
				if perr != nil {
					if ctx.Err() == nil {
						log.Printf("WARN: build pair_req for %s: %v", p.Name, perr)
					}
					continue
				}
				if _, err := e.udp.WriteToUDP(reqPkt, p.UDPAddr); err != nil {
					if ctx.Err() == nil {
						log.Printf("WARN: pair_req %s: %v", p.Name, err)
					}
					continue
				}
				p.LastPingMs.Store(now)
				p.LastSentReqMs.Store(now)
			}
			// Static --peer mode (legacy): single keepalive every 25s
			// to the configured static endpoint, no peer-state tracking.
			// No obfenv in static mode — fall back to the legacy 1-byte
			// punch packet.
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
	// DNS server stats now live in services/dns; UI reads them via
	// a separate endpoint backed by dns.Service.Stats().
	if e.cfg.Camp != nil {
		d.DNSResolverOK = platform.ZoneResolverInstalled(identity.CampLabel(e.cfg.Camp.ID))
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
	awgDev := e.awgDevice
	e.mu.Unlock()

	// DNS server + zone-resolver teardown lives in services/dns now;
	// main.go drives it from eng.OnStopped.

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

	// Now tear utun down. When AWG was active, Device owns the TUN fd —
	// Device.Close shuts down its goroutines and closes the fd. When
	// AWG wasn't active (static mode), engine's tunToPeerLoop is the
	// owner — close tun directly so the loop sees Read fail and exits.
	if awgDev != nil {
		_ = awgDev.Close()
	} else if tun != nil {
		_ = tun.Close()
	}
	e.workers.Wait()

	if egr != nil {
		if err := egr.Close(); err != nil {
			errs = append(errs, fmt.Errorf("egress: %w", err))
		}
	}

	e.mu.Lock()
	if e.torrent != nil {
		_ = e.torrent.Close()
	}
	if e.awgDevice != nil {
		// Device.Close stops its goroutines AND closes the underlying
		// TUN fd (since we passed tun.Device() to it, ownership transferred).
		_ = e.awgDevice.Close()
	}
	if e.awgBind != nil {
		_ = e.awgBind.Close()
	}
	e.running = false
	e.tun = nil
	e.udp = nil
	e.awgBind = nil
	e.awgDevice = nil
	e.obfenv = nil
	e.routes = nil
	e.egr = nil
	e.ca = nil
	e.torrent = nil
	e.announce = nil
	e.poller = nil
	e.campAddr.Store(nil)
	e.campPeers.Store(nil)
	e.campReflex.Store(nil)
	e.peers = map[string]*peerState{}
	e.activePub.Store(nil)
	e.staticPeer.Store(nil)
	e.lastStaticPingMs.Store(0)
	e.intercepts = map[string]*InterceptInfo{}
	e.camp = nil
	e.identity = nil
	if cc := e.loadCall(); cc != nil {
		cc.sfu.Close()
		e.clearCall()
	}
	e.ClearJoinedSFUHost()
	e.storeRemoteCalls(nil)
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
		}
		if e.cfg.Camp != nil {
			st.CampActive = e.announce != nil
			st.CampURL = e.cfg.Camp.URL
			st.CampName = e.cfg.Camp.Name
			st.CampID = e.cfg.Camp.ID
			st.CampLabel = identity.CampLabel(e.cfg.Camp.ID)
			st.CampReflex = e.currentReflex()
			if e.identity != nil {
				st.IdentityPub = e.identity.PubHex()
				st.IdentityFP = e.identity.Fingerprint()
			}
			if active := e.activePub.Load(); active != nil {
				st.ActivePeerPub = *active
				if p, ok := e.peers[*active]; ok {
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
	if a := e.activePub.Load(); a != nil {
		active = *a
	}
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
			UDPEndpoint: selfEndpoint,
			JoinedAt:    e.started.UnixMilli(),
			InCamp:      true,
			Online:      true,
			Reachable:   true,
			Verified:    true,
			Paired:      true,
			Self:        true,
			OverlayV4:   e.cfg.LocalIP,
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
		// Pair-handshake state — single source of truth for UI color logic.
		paired := p.IsPaired()
		halfPaired := p.IsHalfPaired()
		// LastPongMs / Verified are legacy fields kept for UI backward
		// compat. Semantically they now reflect the pair_res signal and
		// "fully paired" respectively. New UI code should use Paired /
		// HalfPaired directly.
		lastPong := p.LastValidResMs.Load()
		rtt := p.LastRTTMs.Load()
		out = append(out, PeerStatusInfo{
			Name:        p.Name,
			Pub:         p.Pub,
			Fp:          identity.FingerprintHex(p.Pub),
			PublicIP:    p.PublicIP,
			UDPPort:     p.UDPPort,
			UDPEndpoint: p.UDPEndpoint,
			JoinedAt:    p.JoinedAt,
			LastSeenMs:  seen,
			Online:      online,
			Reachable:   online, // kept as alias for backward compat; same semantic now
			InCamp:      p.InCamp,
			Active:      p.Pub == active,
			Files:       sortedFiles(p.Files),
			Firewall:    append([]config.Firewall(nil), p.Firewall...),
			LastPongMs:  lastPong,
			RTTMs:       rtt,
			Verified:    paired,
			Paired:      paired,
			HalfPaired:  halfPaired,
			OverlayV4:   overlayV4OrEmpty(p.Pub),
		})
	}
	return out
}

// validCampLabel returns true iff label only uses chars the camp
// server's NAME_RE accepts. The same character set we constrain camp
// labels to so the derived camp_id = <pub>_<label> stays acceptable.
func validCampLabel(label string) bool {
	if label == "" {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// peerHTTPHostLocked returns the pub-derived v4 address for peer p,
// used as the host in tunnel-side HTTP URLs.
func (e *Engine) peerHTTPHostLocked(p *peerState) string {
	if p != nil && p.Pub != "" {
		if a, err := overlay.PubToV4Addr(p.Pub); err == nil {
			return a.String()
		}
	}
	return ""
}

func overlayV4OrEmpty(pubHex string) string {
	if pubHex == "" {
		return ""
	}
	addr, err := overlay.PubToV4Addr(pubHex)
	if err != nil {
		return ""
	}
	return addr.String()
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
// catch-all traffic and the meet signalling go to. pub must match a
// peer currently in the peers map; empty string clears the selection.
func (e *Engine) SetActivePeer(pub string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if pub == "" {
		e.activePub.Store(nil)
		log.Printf("camp: active peer cleared")
		return nil
	}
	p, ok := e.peers[pub]
	if !ok {
		return fmt.Errorf("no peer with pub %s", pub)
	}
	e.activePub.Store(&pub)
	log.Printf("camp: active peer = %s (pub=%s)", p.Name, pub)
	return nil
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
	// Push the new prefixes into AWG's allowed_ips trie so outbound
	// packets to intercept destinations route through this peer.
	if e.awgDevice != nil {
		go e.awgSyncPeers()
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
	// Drop the removed prefixes from AWG's allowed_ips trie so outbound
	// packets to those destinations no longer route through this peer.
	if e.awgDevice != nil {
		go e.awgSyncPeers()
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
			packetLog("[%s] %s [%s]", e.tun.Name(), summary, action)
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
			packetLog("[%s] %s [%s]", e.tun.Name(), summary, action)
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
		packetLog("[%s] %s [%s]", e.tun.Name(), summary, action)
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
	// Per-peer lookup: every peer has a deterministic v4 alias
	// (100.64.X.Y from sha256(pub)). Walk peers, compute the matching
	// address, compare.
	for _, p := range e.peers {
		if p.Pub == "" || p.UDPAddr == nil {
			continue
		}
		if a, err := overlay.PubToV4Addr(p.Pub); err == nil && a == dst {
			return p.UDPAddr
		}
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
		// Control-envelope multiplex (hello + future control types + AWG).
		// First uint32 of every packet is the magic header — if it lands
		// in one of our slot ranges this is either:
		//   - a hello (decrypt + verify + store WGPub)
		//   - an AWG packet (currently dropped; will dispatch to AWG bind once wired)
		//   - a reserved control type (dropped)
		// Anything else falls through to the legacy plaintext path.
		if e.obfenv != nil && n >= 4 {
			firstU32 := binary.LittleEndian.Uint32(pkt[:4])
			slot := e.obfenv.SlotFor(firstU32)
			awgDebug("rx udp %d bytes from %s magic=0x%08x slot=%d", n, from, firstU32, slot)
			switch slot {
			case obfenv.SlotHello:
				if plain, _, ok := e.obfenv.Open(pkt); ok {
					// Slot is shared by hello and pair during transition.
					// Discriminate by the JSON "t" field.
					switch pair.Type(plain) {
					case pair.TypeReq:
						if req, rok := pair.ParseReq(plain); rok {
							e.handlePairReq(req, from)
						} else {
							log.Printf("pair: req parse/verify failed from %s", from)
						}
					case pair.TypeRes:
						if res, rok := pair.ParseRes(plain); rok {
							e.handlePairRes(res, from)
						} else {
							log.Printf("pair: res parse/verify failed from %s", from)
						}
					default:
						log.Printf("pair: unknown control packet type %q from %s", pair.Type(plain), from)
					}
				}
				continue
			case obfenv.SlotAWGInit, obfenv.SlotAWGResponse,
				obfenv.SlotAWGCookie, obfenv.SlotAWGTransport:
				// AWG slot — forward to Bind. Once Device is wired (next
				// step) it will drain the Bind's inbox via the
				// ReceiveFunc returned from Open. Until then the inbox
				// fills to 64 and overflows silently — fine, AWG keepalive
				// will retransmit.
				if e.awgBind != nil {
					e.awgBind.Deliver(pkt, from.AddrPort())
				}
				continue
			case obfenv.SlotReserved6, obfenv.SlotReserved7, obfenv.SlotReserved8:
				// Reserved control slots — no handler yet.
				continue
			}
			// SlotFor == -1: not our envelope, fall through.
		}
		// Identify which peer sent this and refresh LastSeen *before*
		// any IP-shape filter — that way 1-byte hole-punch and
		// keepalive packets also count as "peer is alive" signals, not
		// just real IP traffic. Peers are identified by UDP source —
		// NAT rebinds invalidate this until the next camp poll, at
		// which point applyPeerList re-resolves the UDPEndpoint and
		// the next packet from them re-matches.
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
			packetLog("[udp %s] drop non-IP byte=0x%02x (%d bytes)", from, pkt[0], n)
			continue
		}
		summary := packet.Summary(pkt)

		// When AWG owns utun, engine must not write to the same fd
		// concurrently — Device's write goroutine and ours would race.
		// Plaintext IP arriving on the UDP socket here is either:
		//   - a packet from an old peer (no pair → no AWG session) →
		//     legitimate to drop because we can't authenticate it anyway
		//   - junk from the network → drop
		// Either way: drop silently when Device is active.
		if e.awgDevice != nil {
			packetLog("[udp %s] %s [drop-no-awg-session]", from, summary)
			continue
		}
		if werr := e.tun.Write(pkt); werr != nil {
			if ctx.Err() == nil {
				log.Printf("WARN: utun write from %s: %v", from, werr)
			}
			packetLog("[udp %s] %s [→utun-failed]", from, summary)
		} else {
			e.rxBytes.Add(uint64(n))
			e.rxPackets.Add(1)
			packetLog("[udp %s] %s [→utun]", from, summary)
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
	// awgDevice.Close also closes the underlying TUN fd (since we
	// passed tun.Device() to it). So if device was active, do NOT
	// call e.tun.Close — that would be a double-close.
	devActive := e.awgDevice != nil
	if e.awgDevice != nil {
		_ = e.awgDevice.Close()
		e.awgDevice = nil
	}
	if e.awgBind != nil {
		_ = e.awgBind.Close()
		e.awgBind = nil
	}
	if e.tun != nil {
		if !devActive {
			_ = e.tun.Close()
		}
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
	e.poller = nil
	e.obfenv = nil
}

func sameUDPAddr(a, b *net.UDPAddr) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Port == b.Port && a.IP.Equal(b.IP)
}
