//go:build darwin

// Package engine owns the tunnel runtime: utun, UDP, routes, and (optionally)
// egress NAT setup. It exposes a Start/Stop lifecycle plus methods to mutate
// the intercept list while running, so that both the CLI and the web UI can
// drive the same core.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vseplet/f2f/source/mac/internal/egress"
	"github.com/vseplet/f2f/source/mac/internal/packet"
	"github.com/vseplet/f2f/source/mac/internal/rendezvous"
	"github.com/vseplet/f2f/source/mac/internal/route"
	"github.com/vseplet/f2f/source/mac/internal/tunnel"
)

// CampConfig points the engine at a rendezvous (camp) server: instead of
// the user supplying the peer's UDP endpoint via --peer, we discover our
// own external endpoint via STUN, register with camp under (Name, Room),
// and adopt the other peer in the same room when it announces an endpoint.
type CampConfig struct {
	URL      string // wss://f2f-camp.fly.dev/ws
	Name     string // our identity within Room
	Room     string // shared room name
	StunAddr string // host:port for the UDP STUN probe (e.g. f2f-camp.fly.dev:3478)
}

// Config is the input to Start.
type Config struct {
	LocalIP       string   // utun local point-to-point address
	PeerIP        string   // utun remote point-to-point address
	Listen        string   // UDP listen address (":9000"), empty = no peer mode
	Peer          string   // UDP peer address ("host:9000"); ignored when Camp is set
	Intercepts    []string // user-provided IPs/CIDRs/domains, resolved at Start
	InboundAllow  []string // whitelist of destinations the peer is allowed to reach via us
	EgressIface   string   // physical interface for NAT (empty = no egress)
	EgressSubnet  string   // CIDR to NAT (default "10.99.0.0/24")
	Camp          *CampConfig // optional: use a rendezvous server instead of static Peer
}

// Status is a point-in-time snapshot. It is computed; the underlying state
// changes between calls.
type Status struct {
	Running        bool            `json:"running"`
	UtunName       string          `json:"utun_name,omitempty"`
	LocalIP        string          `json:"local_ip,omitempty"`
	PeerIP         string          `json:"peer_ip,omitempty"`
	ListenAddr     string          `json:"listen_addr,omitempty"`
	PeerAddr       string          `json:"peer_addr,omitempty"`
	EgressActive   bool            `json:"egress_active"`
	EgressIface    string          `json:"egress_iface,omitempty"`
	EgressAnchor   string          `json:"egress_anchor,omitempty"`
	EgressSubnet   string          `json:"egress_subnet,omitempty"`
	CampActive     bool            `json:"camp_active"`
	CampURL        string          `json:"camp_url,omitempty"`
	CampName       string          `json:"camp_name,omitempty"`
	CampRoom       string          `json:"camp_room,omitempty"`
	CampPeerName   string          `json:"camp_peer_name,omitempty"`     // adopted peer's name in the room
	CampReflex     string          `json:"camp_reflex,omitempty"`        // our own external endpoint per STUN
	Intercepts     []InterceptInfo `json:"intercepts"`
	InboundAllow   []InterceptInfo `json:"inbound_allow"`
	StartedAt      time.Time       `json:"started_at,omitempty"`
	TxBytes        uint64          `json:"tx_bytes"`
	RxBytes        uint64          `json:"rx_bytes"`
	TxPackets      uint64          `json:"tx_packets"`
	RxPackets      uint64          `json:"rx_packets"`
	DroppedInbound uint64          `json:"dropped_inbound"`
}

// InterceptInfo describes one intercept entry and what host routes it owns.
type InterceptInfo struct {
	ID       string   `json:"id"`
	Spec     string   `json:"spec"`
	Prefixes []string `json:"prefixes"`
}

// Engine is the long-lived tunnel runtime.
type Engine struct {
	mu sync.Mutex

	running bool
	cfg     Config

	tun     *tunnel.Tunnel
	udp     *net.UDPConn
	routes  *route.Manager
	egr     *egress.Egress
	camp    *rendezvous.Client
	// campReflex / campPeerName / campPeerTunnelIP are display-only and
	// updated from adoptCampPeer, which can run while Start already holds
	// e.mu. Keep them lock-free via atomics so they can't deadlock.
	campReflex       atomic.Pointer[string]
	campPeerName     atomic.Pointer[string]
	campPeerTunnelIP atomic.Pointer[string]
	peerPtr          atomic.Pointer[net.UDPAddr]

	intercepts   map[string]*InterceptInfo
	inboundAllow map[string]*InterceptInfo
	nextItemID   uint64

	cancel  context.CancelFunc
	workers sync.WaitGroup
	started time.Time

	txBytes, rxBytes     atomic.Uint64
	txPackets, rxPackets atomic.Uint64
	droppedInbound       atomic.Uint64

	tap *logTap
}

// New returns a fresh Engine. Start it to bring it up.
func New() *Engine {
	return &Engine{
		intercepts:   map[string]*InterceptInfo{},
		inboundAllow: map[string]*InterceptInfo{},
		tap:          newLogTap(),
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
		if cfg.Camp.URL == "" || cfg.Camp.Name == "" || cfg.Camp.Room == "" || cfg.Camp.StunAddr == "" {
			return errors.New("Camp.{URL,Name,Room,StunAddr} all required")
		}
	} else if (cfg.Listen == "") != (cfg.Peer == "") {
		return errors.New("Listen and Peer must both be set or both be empty")
	}

	// Egress goes first so its rollback runs last on the way down.
	if cfg.EgressIface != "" {
		subnetStr := cfg.EgressSubnet
		if subnetStr == "" {
			subnetStr = "10.99.0.0/24"
		}
		subnet, err := netip.ParsePrefix(subnetStr)
		if err != nil {
			return fmt.Errorf("egress subnet %q: %w", subnetStr, err)
		}
		egr, err := egress.Open(cfg.EgressIface, subnet)
		if err != nil {
			return fmt.Errorf("egress setup: %w", err)
		}
		e.egr = egr
		log.Printf("egress: NAT %s → %s via pf anchor %q, ip.forwarding=1",
			subnet, cfg.EgressIface, egr.Anchor())
		cfg.EgressSubnet = subnetStr
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
			e.peerPtr.Store(initialPeer)
		}
	}

	// Camp: discover external endpoint via STUN, then connect to the
	// rendezvous WebSocket. Welcome carries our camp-assigned tunnel IP,
	// which utun will be opened with below.
	var (
		welcome  *rendezvous.Welcome
		localIP  = cfg.LocalIP
		peerIP   = cfg.PeerIP
	)
	if cfg.Camp != nil {
		reflex, err := rendezvous.Probe(e.udp, cfg.Camp.StunAddr, 5*time.Second)
		if err != nil {
			e.rollbackPartial()
			return fmt.Errorf("camp stun: %w", err)
		}
		log.Printf("camp: STUN reflex %s (advertised port %d)", reflex, reflex.Port)

		dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
		client, w, err := rendezvous.Dial(dialCtx, cfg.Camp.URL, cfg.Camp.Name, cfg.Camp.Room, reflex.Port)
		dialCancel()
		if err != nil {
			e.rollbackPartial()
			return fmt.Errorf("camp dial: %w", err)
		}
		if w.You.TunnelIP == "" {
			_ = client.Close()
			e.rollbackPartial()
			return fmt.Errorf("camp welcome did not assign a tunnel_ip")
		}
		e.camp = client
		rs := reflex.String()
		e.campReflex.Store(&rs)
		welcome = w
		localIP = w.You.TunnelIP
		// peerIP stays as the user-provided value; it's only used for
		// utun's point-to-point pair in static mode. In Camp mode utun
		// is opened as a subnet so peerIP doesn't matter.
		log.Printf("camp: registered as %s in room %s, tunnel_ip=%s", cfg.Camp.Name, cfg.Camp.Room, localIP)
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
		log.Printf("UDP listening on %s, forwarding to peer %s", e.udp.LocalAddr(), e.peerPtr.Load())
	}

	// Routes for the initial intercept list.
	e.routes = route.New(tun.Name())
	for _, spec := range cfg.Intercepts {
		if _, err := e.addInterceptLocked(spec); err != nil {
			log.Printf("WARN: intercept %q: %v", spec, err)
		}
	}

	// Seed inbound whitelist (no system side effects — just resolve prefixes).
	for _, spec := range cfg.InboundAllow {
		if _, err := e.addInboundAllowLocked(spec); err != nil {
			log.Printf("WARN: inbound-allow %q: %v", spec, err)
		}
	}

	// If camp's welcome told us about peers already in the room, adopt
	// the first one with a known UDP endpoint — two-party tunnel for now.
	if welcome != nil {
		log.Printf("camp: %d existing peer(s) in room", len(welcome.Peers))
		for _, p := range welcome.Peers {
			if p.UDPEndpoint != "" {
				e.adoptCampPeer(p)
				break
			}
		}
	}

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
	if e.camp != nil {
		e.workers.Add(1)
		go e.campLoop(ctx, e.camp)
	}
	if e.udp != nil {
		e.workers.Add(1)
		go e.keepaliveLoop(ctx)
	}
	return nil
}

// keepaliveLoop fires a one-byte UDP packet at the current peer every
// 25 seconds. Most consumer NATs drop idle UDP mappings after 30–120s;
// the keepalive is what holds the hole open between bursts of real
// tunnel traffic. The single byte is below our 20-byte IP minimum, so
// the peer's peerToTunLoop drops it without touching utun.
func (e *Engine) keepaliveLoop(ctx context.Context) {
	defer e.workers.Done()
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			addr := e.peerPtr.Load()
			if addr == nil {
				continue
			}
			if _, err := e.udp.WriteToUDP([]byte{0}, addr); err != nil {
				if ctx.Err() == nil {
					log.Printf("WARN: keepalive: %v", err)
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
	camp := e.camp
	e.mu.Unlock()

	cancel()
	// Close the camp WebSocket early so its read loop unblocks before we
	// wait on workers below.
	if camp != nil {
		_ = camp.Close()
	}
	// Close UDP first; this aborts the peerToTun worker. It is independent
	// of utun and routes, so it's safe to do early.
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
	e.camp = nil
	e.campReflex.Store(nil)
	e.campPeerName.Store(nil)
	e.campPeerTunnelIP.Store(nil)
	e.peerPtr.Store(nil)
	e.intercepts = map[string]*InterceptInfo{}
	e.inboundAllow = map[string]*InterceptInfo{}
	e.txBytes.Store(0)
	e.rxBytes.Store(0)
	e.txPackets.Store(0)
	e.rxPackets.Store(0)
	e.droppedInbound.Store(0)
	e.mu.Unlock()
	return errors.Join(errs...)
}

// Status returns a snapshot of the current state.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()

	st := Status{
		Running:        e.running,
		EgressActive:   e.egr != nil,
		StartedAt:      e.started,
		TxBytes:        e.txBytes.Load(),
		RxBytes:        e.rxBytes.Load(),
		TxPackets:      e.txPackets.Load(),
		RxPackets:      e.rxPackets.Load(),
		DroppedInbound: e.droppedInbound.Load(),
	}
	if e.tun != nil {
		st.UtunName = e.tun.Name()
	}
	if e.running {
		st.LocalIP = e.cfg.LocalIP
		st.PeerIP = e.cfg.PeerIP
		// In Camp mode the user-supplied PeerIP is meaningless — utun is a
		// subnet, the peer's tunnel IP comes from camp on adoption.
		if e.cfg.Camp != nil {
			if p := e.campPeerTunnelIP.Load(); p != nil {
				st.PeerIP = *p
			} else {
				st.PeerIP = "" // not adopted yet
			}
		}
		st.ListenAddr = e.cfg.Listen
		if p := e.peerPtr.Load(); p != nil {
			st.PeerAddr = p.String()
		}
		if e.egr != nil {
			st.EgressIface = e.cfg.EgressIface
			st.EgressAnchor = e.egr.Anchor()
			st.EgressSubnet = e.cfg.EgressSubnet
		}
		if e.cfg.Camp != nil {
			st.CampActive = e.camp != nil
			st.CampURL = e.cfg.Camp.URL
			st.CampName = e.cfg.Camp.Name
			st.CampRoom = e.cfg.Camp.Room
			if r := e.campReflex.Load(); r != nil {
				st.CampReflex = *r
			}
			if n := e.campPeerName.Load(); n != nil {
				st.CampPeerName = *n
			}
		}
	}
	st.Intercepts = make([]InterceptInfo, 0, len(e.intercepts))
	for _, info := range e.intercepts {
		st.Intercepts = append(st.Intercepts, *info)
	}
	st.InboundAllow = make([]InterceptInfo, 0, len(e.inboundAllow))
	for _, info := range e.inboundAllow {
		st.InboundAllow = append(st.InboundAllow, *info)
	}
	return st
}

// AddIntercept resolves spec and installs its host routes via utun. Returns
// the new entry's info. Requires Running.
func (e *Engine) AddIntercept(spec string) (InterceptInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running {
		return InterceptInfo{}, errors.New("engine not running")
	}
	return e.addInterceptLocked(spec)
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

// AddInboundAllow adds an entry to the inbound whitelist. When the
// whitelist is non-empty, packets arriving from the peer whose destination
// is NOT in any whitelist prefix are dropped (and counted in DroppedInbound).
// Requires Running.
func (e *Engine) AddInboundAllow(spec string) (InterceptInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running {
		return InterceptInfo{}, errors.New("engine not running")
	}
	return e.addInboundAllowLocked(spec)
}

// RemoveInboundAllow removes an entry from the inbound whitelist.
func (e *Engine) RemoveInboundAllow(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running {
		return errors.New("engine not running")
	}
	info, ok := e.inboundAllow[id]
	if !ok {
		return fmt.Errorf("inbound-allow %q not found", id)
	}
	delete(e.inboundAllow, id)
	log.Printf("removed inbound-allow %s (%s)", id, info.Spec)
	return nil
}

// addInboundAllowLocked resolves spec and adds the resulting prefixes to
// the whitelist. Caller holds e.mu.
func (e *Engine) addInboundAllowLocked(spec string) (InterceptInfo, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return InterceptInfo{}, errors.New("empty inbound-allow spec")
	}
	prefixes, err := resolveSpec(spec)
	if err != nil {
		return InterceptInfo{}, err
	}
	if len(prefixes) == 0 {
		return InterceptInfo{}, fmt.Errorf("%q: no addresses", spec)
	}
	e.nextItemID++
	id := "a" + strconv.FormatUint(e.nextItemID, 10)
	info := &InterceptInfo{ID: id, Spec: spec}
	for _, p := range prefixes {
		info.Prefixes = append(info.Prefixes, p.String())
	}
	e.inboundAllow[id] = info
	log.Printf("inbound-allow %s → %s", spec, strings.Join(info.Prefixes, ", "))
	return *info, nil
}

// addInterceptLocked must be called with e.mu held and e.running == true.
func (e *Engine) addInterceptLocked(spec string) (InterceptInfo, error) {
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
	info := &InterceptInfo{ID: id, Spec: spec}
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
		peerAddr := e.peerPtr.Load()
		if hasPeer && peerAddr != nil {
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
		} else if hasPeer {
			action = "drop-no-peer"
		}
		log.Printf("[%s] %s [%s]", e.tun.Name(), summary, action)
	}
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
		// Anything not shaped like an IPv4/IPv6 packet — hole-punch
		// markers, stale STUN reflexes that raced past Probe(), random
		// scans — gets dropped here before it can pollute peer tracking
		// or fail utun.Write. Only after we've decided this is a real
		// tunnel packet do we let it update peerPtr (so peer-NAT rebinds
		// can still be followed) and reach utun.
		if n < 20 {
			continue
		}
		version := pkt[0] >> 4
		if version != 4 && version != 6 {
			log.Printf("[udp %s] drop non-IP byte=0x%02x (%d bytes)", from, pkt[0], n)
			continue
		}
		if cur := e.peerPtr.Load(); !sameUDPAddr(cur, from) {
			log.Printf("peer address updated: %s → %s", cur, from)
			e.peerPtr.Store(from)
		}
		summary := packet.Summary(pkt)

		// Inbound whitelist: when non-empty, only packets whose destination
		// matches some whitelist prefix are passed to the OS.
		if !e.inboundAllowed(pkt) {
			e.droppedInbound.Add(1)
			log.Printf("[udp %s] %s [drop-filter]", from, summary)
			continue
		}

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

// inboundAllowed reports whether the packet's destination matches the
// whitelist. An empty whitelist means "no filter" — every packet is allowed.
// Packets whose destination address we cannot parse are also allowed (we
// don't want to silently break unknown-format traffic).
//
// Packets destined to OUR OWN tunnel IP terminate locally — they're not
// "passing through" us into the wider internet, so the whitelist (which
// is about restricting peer-to-internet egress) doesn't apply to them.
func (e *Engine) inboundAllowed(pkt []byte) bool {
	e.mu.Lock()
	if len(e.inboundAllow) == 0 {
		e.mu.Unlock()
		return true
	}
	localIPStr := e.cfg.LocalIP
	// Snapshot prefixes so we can drop the lock before the match loop.
	prefixes := make([]netip.Prefix, 0, len(e.inboundAllow)*2)
	for _, info := range e.inboundAllow {
		for _, s := range info.Prefixes {
			if p, err := netip.ParsePrefix(s); err == nil {
				prefixes = append(prefixes, p)
			}
		}
	}
	e.mu.Unlock()

	dst := packet.ExtractDst(pkt)
	if !dst.IsValid() {
		return true
	}
	if local, err := netip.ParseAddr(localIPStr); err == nil && dst == local {
		return true
	}
	for _, p := range prefixes {
		if p.Contains(dst) {
			return true
		}
	}
	return false
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
	if e.camp != nil {
		_ = e.camp.Close()
		e.camp = nil
	}
}

// campLoop runs the WebSocket reader and dispatches peer events from the
// rendezvous server. Cancelled via ctx; Run also returns when the socket
// is closed by Stop.
func (e *Engine) campLoop(ctx context.Context, c *rendezvous.Client) {
	defer e.workers.Done()

	// Run blocks until ctx is cancelled or the socket closes; surface
	// non-cancellation errors but don't crash — Stop will tidy up.
	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()

	events := c.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-runErr:
			if err != nil && !rendezvous.IsCancelled(err) {
				log.Printf("camp: connection closed: %v", err)
			}
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			e.handleCampEvent(ev)
		}
	}
}

func (e *Engine) handleCampEvent(ev rendezvous.Event) {
	switch ev.Kind {
	case rendezvous.EventJoined:
		log.Printf("camp: peer %s joined (%s)", ev.Peer.Name, ev.Peer.UDPEndpoint)
		if ev.Peer.UDPEndpoint != "" {
			e.adoptCampPeer(ev.Peer)
		}
	case rendezvous.EventUpdated:
		log.Printf("camp: peer %s updated (%s)", ev.Peer.Name, ev.Peer.UDPEndpoint)
		if ev.Peer.UDPEndpoint != "" {
			e.adoptCampPeer(ev.Peer)
		}
	case rendezvous.EventLeft:
		log.Printf("camp: peer %s left", ev.Name)
	case rendezvous.EventSignal:
		log.Printf("camp: signal from %s (ignored)", ev.From)
	}
}

// adoptCampPeer sets the engine's peer address from a camp announcement
// and fires a short burst of single-byte hole-punching packets to open
// the NAT mapping. Real tunnel traffic from utun is what keeps it open
// after that.
func (e *Engine) adoptCampPeer(p rendezvous.PeerInfo) {
	addr, err := net.ResolveUDPAddr("udp", p.UDPEndpoint)
	if err != nil {
		log.Printf("WARN: camp peer %s has invalid endpoint %q: %v", p.Name, p.UDPEndpoint, err)
		return
	}
	if cur := e.peerPtr.Load(); sameUDPAddr(cur, addr) {
		return
	}
	e.peerPtr.Store(addr)
	name := p.Name
	e.campPeerName.Store(&name)
	if p.TunnelIP != "" {
		tip := p.TunnelIP
		e.campPeerTunnelIP.Store(&tip)
	}
	log.Printf("camp: adopted peer %s @ %s (tunnel_ip=%s)", p.Name, addr, p.TunnelIP)

	if e.udp == nil {
		return
	}
	go func() {
		for i := 0; i < 5; i++ {
			if _, err := e.udp.WriteToUDP([]byte{0}, addr); err != nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
}

func sameUDPAddr(a, b *net.UDPAddr) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Port == b.Port && a.IP.Equal(b.IP)
}
