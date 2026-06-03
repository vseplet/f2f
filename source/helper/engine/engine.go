// Package engine owns the tunnel runtime: utun, UDP, routes, and (optionally)
// egress NAT setup. It exposes a Start/Stop lifecycle plus methods to mutate
// the intercept list while running, so that both the CLI and the web UI can
// drive the same core.
package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/engine/awg"
	"github.com/vseplet/f2f/source/helper/engine/obfenv"
	
	
	"github.com/vseplet/f2f/source/helper/engine/pair"
	"github.com/vseplet/f2f/source/helper/services/camp/rendezvous"
	"github.com/vseplet/f2f/source/helper/engine/route"
	"github.com/vseplet/f2f/source/helper/engine/utun"
	"github.com/vseplet/f2f/source/helper/identity"
	"github.com/vseplet/f2f/source/helper/platform"
)

// tunnelSubnetCIDR is the CGNAT /10 the overlay carves per-peer
// landing pads out of. Each mac picks its own alias in here
// deterministically from its pub (see PubToV4Addr). Hardcoded
// — camp's
// hub uses the same prefix when allocating tunnel_ips.
const tunnelSubnetCIDR = V4Subnet

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

// Config is the input to Start.
type Config struct {
	LocalIP string // utun local point-to-point address
	PeerIP  string // utun remote point-to-point address (static mode only)
	Listen  string // UDP listen address (":9000"), empty = no peer mode
	Peer    string // UDP peer address ("host:9000"); ignored when CampID is set
	// Camp mode — engine wants the minimum it needs for identity /
	// obfenv / store key. Server endpoints (URL/StunAddr) live in
	// the per-camp config.Camp and are read by services/camp.
	CampID    string // shared camp id; empty triggers create-new path (CampLabel required)
	CampName  string // our display alias in the camp
	CampLabel string // human label for create-new (derives ID = <pub>_<label>)
}

// Status is a point-in-time snapshot. It is computed; the underlying state
// changes between calls.
type Status struct {
	Running      bool   `json:"running"`
	UtunName     string `json:"utun_name,omitempty"`
	LocalIP      string `json:"local_ip,omitempty"`
	PeerIP       string `json:"peer_ip,omitempty"` // active peer's tunnel_ip (camp mode) or static peer (legacy)
	ListenAddr   string `json:"listen_addr,omitempty"`
	PeerAddr     string `json:"peer_addr,omitempty"` // active peer's UDP endpoint
	// CampID is the only camp metadata engine carries — it's the
	// active session key (config store / identity / obfenv all keyed
	// by it). URL/StunAddr/Name/Label and connection signals
	// (Active/Reflex/Health) live in services/camp + web statusView.
	CampID string `json:"camp_id,omitempty"`
	// Identity (Ed25519) for the running camp. Pub is the full 32-byte
	// public key in hex; Fingerprint is the short SHA-256 prefix the
	// UI shows. Empty in static --peer mode.
	IdentityPub string `json:"identity_pub,omitempty"`
	IdentityFP  string `json:"identity_fp,omitempty"`
	// ActivePeerPub is the user-selected peer the tunnel routes
	// catch-all traffic through. Empty when no one has been selected.
	ActivePeerPub string           `json:"active_peer_pub,omitempty"`
	Peers         []PeerStatusInfo `json:"peers"`
	StartedAt     time.Time        `json:"started_at,omitempty"`
	TxBytes       uint64           `json:"tx_bytes"`
	RxBytes       uint64           `json:"rx_bytes"`
	TxPackets     uint64           `json:"tx_packets"`
	RxPackets     uint64           `json:"rx_packets"`
	// Diagnostics is the runtime info dump for the diagnostics tab —
	// DNS counters, goroutines, etc. Always populated when Running.
	Diagnostics *Diagnostics `json:"diagnostics,omitempty"`
}

// Camp connection health (UDP-announce + HTTP-poll counters) lives
// in services/camp now; web statusView merges it into /api/status.

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
	Pub string `json:"pub,omitempty"`
	// Fp is the short SHA-256 fingerprint (16 hex chars) of Pub —
	// what the UI shows. Computed server-side so the browser doesn't
	// have to do crypto. Empty when Pub is empty.
	Fp          string `json:"fp,omitempty"`
	PublicIP    string `json:"public_ip,omitempty"`
	UDPPort     int    `json:"udp_port,omitempty"`
	UDPEndpoint string `json:"udp_endpoint,omitempty"`
	JoinedAt    int64  `json:"joined_at,omitempty"`
	LastSeenMs  int64  `json:"last_seen_ms,omitempty"` // ms since last packet; 0 = never
	Online      bool   `json:"online"`                 // camp-side: announced recently
	Reachable   bool   `json:"reachable"`              // local: receiving UDP from this peer
	Active      bool   `json:"active"`
	Self        bool   `json:"self,omitempty"`
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

	UDPAddr    *net.UDPAddr // current best-known UDP target (port can shift on NAT rebind)
	LastSeenMs atomic.Int64 // epoch ms of last received packet from this peer; 0 = never
	LastPingMs atomic.Int64 // epoch ms of last punch/keepalive we sent

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

	tun      *utun.Tunnel
	udp    *net.UDPConn
	routes *route.Manager

	// udpHandlers are external claimants of UDP packets from
	// peerToTunLoop. services/camp registers one to catch camp-source
	// packets (announce reply). Empty slot left by unregister() is nil.
	udpHandlersMu sync.Mutex
	udpHandlers   []UDPHandler

	// obfenv carries the camp-wide obfuscation parameters (camp_key, magic
	// header ranges H1..H8) derived deterministically from cfg.CampID.
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

	// awgAllowedHook lets services/tunnel inject extra allowed_ips
	// (intercept prefixes) into AWG peer sync without engine owning
	// the intercept catalog. nil disables the feature.
	awgAllowedHook func(peerName string) []string

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

// New returns a fresh Engine with the given config Store. The Store
// is shared with services that need to persist their own slices of
// camp config (firewall rules, trusted peers, MyDomains, ...).
// Passing the Store explicitly lets main.go own the lifecycle and
// keeps the engine from being the sole gatekeeper of disk state.
func New(store *config.Store) *Engine {
	return &Engine{
		store: store,
		peers: map[string]*peerState{},
		tap:   newLogTap(),
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
	if cfg.CampID != "" {
		// With Camp, peer is auto-discovered; we still need a UDP socket
		// to receive on.
		if cfg.Listen == "" {
			return errors.New("Camp mode requires Listen")
		}
		if cfg.CampName == "" {
			return errors.New("Camp mode requires CampName")
		}
		// camp_id is optional iff we have a label — empty id means
		// "create a new camp": generate identity first, derive id from
		// the pub. ID populated here so the rest of Start sees a normal
		// fully-formed CampConfig.
		if cfg.CampID == "" {
			label := strings.TrimSpace(cfg.CampLabel)
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
			cfg.CampID = id.PubHex() + "_" + label
			idDir := filepath.Join("/var/lib/f2f/identity", cfg.CampID)
			if err := id.Save(idDir); err != nil {
				return fmt.Errorf("camp create: save identity: %w", err)
			}
			e.identity = id
			log.Printf("camp create: new camp id=%s pub=%s", cfg.CampID, id.PubHex())
		}
	} else if (cfg.Listen == "") != (cfg.Peer == "") {
		return errors.New("Listen and Peer must both be set or both be empty")
	}

	// Open $HOME/.f2f/ + load (or create) the per-camp config. Camp
	// mode only — static --peer mode has no per-camp identity.
	if cfg.CampID != "" {
		if err := e.ensureStore(); err != nil {
			return fmt.Errorf("config store: %w", err)
		}
		c, err := e.loadOrCreateCamp(cfg.CampID, cfg.CampName)
		if err != nil {
			return fmt.Errorf("config load %s: %w", cfg.CampID, err)
		}
		e.camp = c
		// Per-camp Ed25519 keypair. Lives under /var/lib/f2f/ (root,
		// 0700) so different camps can't correlate and "leaving" a
		// camp is rm -rf of that one subdir. Failures here are fatal
		// — without an identity we can't prove tunnel_ip ownership
		// to the camp server once that path is wired through.
		// If we already created+saved one above (camp-create path),
		// LoadOrGenerate finds it on disk and returns it.
		idDir := filepath.Join("/var/lib/f2f/identity", cfg.CampID)
		id, err := identity.LoadOrGenerate(idDir)
		if err != nil {
			return fmt.Errorf("identity %s: %w", cfg.CampID, err)
		}
		e.identity = id
		log.Printf("identity: camp %s pub=%s fp=%s", cfg.CampID, id.PubHex(), id.Fingerprint())
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
		e.obfenv = obfenv.NewCamp(cfg.CampID)
		log.Printf("pair: control envelope ready for camp %s", cfg.CampID)
	}

	// Egress NAT (route the overlay subnet out through the host's
	// default route) lives in services/tunnel now — main.go drives
	// it off eng.OnStarted.

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
		if cfg.CampID == "" && cfg.Peer != "" {
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

	// Local IP for utun is derived from our Ed25519 identity (no camp
	// involvement) — unique per-peer so intercept replies routed back
	// through this overlay don't collide on the egress peer's utun.
	// The camp roster (UDP announce + HTTP peer-list poll) is owned
	// by services/camp now and runs after eng.OnStarted.
	var (
		localIP = cfg.LocalIP
		peerIP  = cfg.PeerIP
	)
	if cfg.CampID != "" && e.identity != nil {
		a, derr := PubToV4Addr(e.identity.PubHex())
		if derr != nil {
			e.rollbackPartial()
			return fmt.Errorf("derive v4 alias: %w", derr)
		}
		localIP = a.String()
	}

	// utun. In Camp mode the interface owns the whole 10.99.0.0/24
	// overlay; static mode keeps the legacy point-to-point form.
	var tun *utun.Tunnel
	if cfg.CampID != "" {
		t, err := utun.OpenSubnet(localIP, 10)
		if err != nil {
			e.rollbackPartial()
			return fmt.Errorf("open tunnel: %w", err)
		}
		tun = t
		log.Printf("opened %s (subnet=%s/24 mtu=%d)", tun.Name(), localIP, utun.MTU)
	} else {
		t, err := utun.Open(localIP, peerIP)
		if err != nil {
			e.rollbackPartial()
			return fmt.Errorf("open tunnel: %w", err)
		}
		tun = t
		log.Printf("opened %s (local=%s peer=%s mtu=%d)", tun.Name(), localIP, peerIP, utun.MTU)
	}
	e.tun = tun
	// Reflect the actual addresses we ended up using back into the
	// stored config so Status() shows ground truth, not user intent.
	cfg.LocalIP = localIP
	if cfg.CampID == "" {
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
	if cfg.CampID != "" && e.awgBind != nil && e.identity != nil && e.obfenv != nil {
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
	// read e.cfg.CampName to filter our own entry out of the peer
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

	// Intercepts (routing user-specified prefixes through specific
	// peers) and their periodic DNS-spec refresh live in
	// services/tunnel — main.go starts it off eng.OnStarted.
	if e.camp != nil {
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
	// Camp announce + HTTP poller + UDP dispatch handler all live in
	// services/camp; main.go starts the service off eng.OnStarted.
	if e.udp != nil {
		e.workers.Add(1)
		go e.holePunchLoop(ctx)
	}
	// Peer firewall poll lives in services/firewall.
	// Domain catalog + DNS server live in services/dns.
	// Local CA + peer-CA discovery / install live in services/pki.
	// BitTorrent client + peer-file poll live in services/drop.
	// Group calls + SFU + remote-call poll live in services/calls.
	// main.go drives their lifecycles off eng.OnStarted/OnStopped.

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

// Routes returns the live route manager — the OS-level primitive
// that installs/removes prefixes against the active utun. Exposed
// for services/tunnel which owns the application-level intercept
// catalog. Returns nil before Start.
func (e *Engine) Routes() *route.Manager {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.routes
}

// UtunName returns the live utun interface name (e.g. "utun6") or
// "" before Start. Cheap; safe to call from anywhere.
func (e *Engine) UtunName() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tun == nil {
		return ""
	}
	return e.tun.Name()
}

// HasPeerName reports whether a peer with this name is currently in
// the engine's peer map (camp catalog merged with live state).
// services/tunnel uses this when validating intercept (spec, peer)
// pairs at Add time.
func (e *Engine) HasPeerName(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.peers {
		if p.Name == name {
			return true
		}
	}
	return false
}

// SyncAWG triggers an async AWG peer-list reconciliation. Services
// that change inputs visible to allowed_ips (intercept add/remove)
// call this so AWG re-pushes the updated rule set.
func (e *Engine) SyncAWG() {
	if e.awgDevice == nil {
		return
	}
	go e.awgSyncPeers()
}

// SetAWGAllowedCIDRsHook installs a callback the engine consults
// inside awgSyncPeers to gather extra allowed_ips for one peer (the
// peer's overlay /32 is always included automatically). Pass nil to
// detach. Called by services/tunnel at Start/Stop.
func (e *Engine) SetAWGAllowedCIDRsHook(fn func(peerName string) []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.awgAllowedHook = fn
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

// ApplyCampRoster is called by services/camp every poll cycle (and
// any other future producer of peer rosters) to push the latest list
// into engine state. Updates e.peers (live state used by pair-
// handshake + hole-punch + AWG sync) and the persisted catalog.
//
// Internally delegates to applyPeerList — public surface kept thin
// so the engine import list in services/camp stays small.
func (e *Engine) ApplyCampRoster(peers []rendezvous.PeerInfo) {
	e.applyPeerList(peers)
}

// UDPConn returns the shared UDP socket the transport runs on. Nil
// before Start / after Stop. services/camp borrows it to construct
// its AnnounceClient (the announce protocol piggybacks on the same
// socket so camp can observe our external endpoint via the packet
// source — replaces STUN).
func (e *Engine) UDPConn() *net.UDPConn {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.udp
}

// IdentityPub returns the local Ed25519 public key in hex, or "" if
// the engine isn't running in camp mode. services/camp tags the
// announce packet with this so peers in the camp can route us by
// identity.
func (e *Engine) IdentityPub() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.identity == nil {
		return ""
	}
	return e.identity.PubHex()
}

// UDPHandler claims a UDP packet by returning true. peerToTunLoop
// walks registered handlers in order before falling through to the
// engine's own dispatch (obfenv multiplex → tun.Write).
type UDPHandler func(pkt []byte, from *net.UDPAddr) bool

// RegisterUDPHandler adds h to the dispatch chain. Returns an
// unregister func that removes it again. services/camp registers
// one in Start to claim packets whose source is the camp server.
func (e *Engine) RegisterUDPHandler(h UDPHandler) (unregister func()) {
	e.udpHandlersMu.Lock()
	defer e.udpHandlersMu.Unlock()
	e.udpHandlers = append(e.udpHandlers, h)
	idx := len(e.udpHandlers) - 1
	return func() {
		e.udpHandlersMu.Lock()
		defer e.udpHandlersMu.Unlock()
		if idx < len(e.udpHandlers) {
			e.udpHandlers[idx] = nil
		}
	}
}

// dispatchUDP walks the registered handlers and returns true as soon
// as one claims the packet. Called from peerToTunLoop hot path; lock
// is held only long enough to snapshot the slice.
func (e *Engine) dispatchUDP(pkt []byte, from *net.UDPAddr) bool {
	e.udpHandlersMu.Lock()
	hs := append([]UDPHandler(nil), e.udpHandlers...)
	e.udpHandlersMu.Unlock()
	for _, h := range hs {
		if h == nil {
			continue
		}
		if h(pkt, from) {
			return true
		}
	}
	return false
}

// applyPeerList reconciles our peers map with the camp's current view
// and caches the snapshot for the UI. Every peer (except ourselves) is
// tracked so the holePunchLoop can keep NAT mappings open with all of
// them. Active selection is independent and driven by the UI.
func (e *Engine) applyPeerList(peers []rendezvous.PeerInfo) {
	// Snapshot of the polled roster lives in services/camp now (its
	// Snapshot() exposes it to the UI). Engine just merges the diff
	// into peers + catalog.

	ourName := e.cfg.CampName

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

// Camp roster snapshot lives in services/camp now — UI hits
// campSvc.Snapshot() directly.

// 1-byte UDP punch/keepalive packets are below our 20-byte IP minimum,
// so the receiving peer's peerToTunLoop drops them without touching
// utun. They exist purely to keep NAT mappings open.
// holePunchLoop sends 1-byte UDP packets to every known peer at an
// adaptive cadence: 1 Hz while the peer is unconfirmed (LastSeenMs ==
// 0 or stale by >25s), then once per ~25s as keepalive once we've
// seen a packet from them. The single tick drives both modes, so a
// peer that goes silent automatically reverts to burst mode.


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
		overlayAddr, err := PubToV4Addr(p.Pub)
		if err != nil {
			continue
		}
		// AllowedIPs = peer's overlay /32 + every intercept prefix the
		// tunnel service has bound to this peer (via awgAllowedHook).
		// Without those extra CIDRs AWG drops outgoing packets to
		// intercept destinations.
		cidrs := make([]string, 0, 8)
		cidrs = append(cidrs, overlayAddr.String()+"/32")
		if e.awgAllowedHook != nil {
			cidrs = append(cidrs, e.awgAllowedHook(p.Name)...)
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
	if e.cfg.CampID == "" || e.identity == nil || e.obfenv == nil {
		return nil, errors.New("pair_req: not in camp mode")
	}
	reqJSON, err := pair.BuildReq(e.identity, e.cfg.CampName, sentMs)
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
	endpointChanged := !sameUDPAddr(p.UDPAddr, from)
	if endpointChanged {
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
	// Refresh the AWG peer list:
	//   - firstReq → peer just became routable through AWG, register it.
	//   - endpointChanged → NAT rebind, AWG must push the new endpoint
	//     or it'll keep handshaking against the stale tuple silently.
	// Runs asynchronously so the recv path doesn't block on IpcSet.
	if firstReq || endpointChanged {
		go e.awgSyncPeers()
	}

	// Build and send the response. Our own sent_ms is the response's
	// sent_ms; echo_ms carries the requester's sent_ms verbatim so they
	// can compute RTT on receipt.
	if e.cfg.CampID == "" || e.identity == nil || e.obfenv == nil {
		return
	}
	resJSON, err := pair.BuildRes(e.identity, e.cfg.CampName, now, req.SentMs)
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
	endpointChanged := !sameUDPAddr(p.UDPAddr, from)
	if endpointChanged {
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
	} else if endpointChanged {
		// NAT rebind after the initial pair — AWG was still gunning at
		// the stale tuple. Push the new endpoint so handshakes land.
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
	if e.cfg.CampID != "" {
		d.DNSResolverOK = platform.ZoneResolverInstalled(identity.CampLabel(e.cfg.CampID))
	}
	return d
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


	e.mu.Lock()
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
	e.peers = map[string]*peerState{}
	e.activePub.Store(nil)
	e.staticPeer.Store(nil)
	e.lastStaticPingMs.Store(0)
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
		Running:   e.running,
		StartedAt: e.started,
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
		if e.cfg.CampID == "" {
			// Static --peer mode — legacy, single peer.
			st.PeerIP = e.cfg.PeerIP
			if p := e.staticPeer.Load(); p != nil {
				st.PeerAddr = p.String()
			}
		}
		if e.cfg.CampID != "" {
			st.CampID = e.cfg.CampID
			if e.identity != nil {
				st.IdentityPub = e.identity.PubHex()
				st.IdentityFP = e.identity.Fingerprint()
			}
			if active := e.activePub.Load(); active != nil {
				st.ActivePeerPub = *active
				if p, ok := e.peers[*active]; ok && p.UDPAddr != nil {
					st.PeerAddr = p.UDPAddr.String()
				}
			}
			st.Peers = e.peersStatusLocked()
		}
		st.Diagnostics = e.diagnosticsLocked()
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
	if e.cfg.CampID != "" {
		selfPub, selfFp := "", ""
		if e.identity != nil {
			selfPub = e.identity.PubHex()
			selfFp = e.identity.Fingerprint()
		}
		// UDPEndpoint for self comes from services/camp via web
		// statusView (engine no longer owns the announce reply).
		out = append(out, PeerStatusInfo{
			Name:       e.cfg.CampName,
			Pub:        selfPub,
			Fp:         selfFp,
			JoinedAt:   e.started.UnixMilli(),
			InCamp:     true,
			Online:     true,
			Reachable:  true,
			Verified:   true,
			Paired:     true,
			Self:       true,
			OverlayV4:  e.cfg.LocalIP,
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
		if a, err := PubToV4Addr(p.Pub); err == nil {
			return a.String()
		}
	}
	return ""
}

func overlayV4OrEmpty(pubHex string) string {
	if pubHex == "" {
		return ""
	}
	addr, err := PubToV4Addr(pubHex)
	if err != nil {
		return ""
	}
	return addr.String()
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
		summary := packetSummary(pkt)
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
			if e.cfg.CampID != "" {
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
//  1. dst is a known peer's tunnel_ip → that peer (meet, direct).
//  2. dst is covered by an intercept → that intercept's bound peer.
//  3. otherwise → drop (no implicit catch-all peer).
//
// Static mode: always to the configured static peer.
func (e *Engine) routeFor(pkt []byte) *net.UDPAddr {
	if e.cfg.CampID == "" {
		return e.staticPeer.Load()
	}
	dst := extractDst(pkt)
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
		if a, err := PubToV4Addr(p.Pub); err == nil && a == dst {
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

// interceptPeerForLocked returned the bound peer name for an
// intercept whose prefix contained dst. Used only by routeFor in
// the legacy plaintext (--peer) data path, which is now phased out
// in favour of AWG (the encrypted Device owns utun forwarding). The
// hook is dead in AWG mode and routeFor handles a nil result fine.
func (e *Engine) interceptPeerForLocked(_ netip.Addr) string {
	return ""
}

func (e *Engine) peerToTunLoop(ctx context.Context) {
	defer e.workers.Done()
	buf := make([]byte, utun.MTU)
	for {
		n, from, err := e.udp.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("udp read stopped: %v", err)
			}
			return
		}
		pkt := buf[:n]
		// External claimants first: services/camp registers a handler
		// that catches announce-reply packets from the camp server
		// (and could be extended by future control-plane services
		// piggybacking on the same socket).
		if e.dispatchUDP(pkt, from) {
			continue
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
		if e.cfg.CampID != "" {
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
		summary := packetSummary(pkt)

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
	e.obfenv = nil
}

func sameUDPAddr(a, b *net.UDPAddr) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Port == b.Port && a.IP.Equal(b.IP)
}
