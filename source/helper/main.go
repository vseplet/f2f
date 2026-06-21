// f2f-mac is the macOS-side CLI for the f2f UDP tunnel. Launches the
// web UI on 127.0.0.1:2202 (overridable via --bind). Needs sudo for
// utun + routing + pf.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/vseplet/f2f/source/helper/db/blocks"
	"github.com/vseplet/f2f/source/helper/db/blocks/channels"
	"github.com/vseplet/f2f/source/helper/db/blocks/message"
	"github.com/vseplet/f2f/source/helper/cli"
	"github.com/vseplet/f2f/source/helper/clog"
	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/db"
	"github.com/vseplet/f2f/source/helper/identity"
	"github.com/vseplet/f2f/source/helper/mesh/bus"
	"github.com/vseplet/f2f/source/helper/mesh/camp"
	"github.com/vseplet/f2f/source/helper/mesh/engine"
	"github.com/vseplet/f2f/source/helper/mesh/gossip"
	"github.com/vseplet/f2f/source/helper/services/calls"
	"github.com/vseplet/f2f/source/helper/services/dns"
	"github.com/vseplet/f2f/source/helper/services/drop"
	"github.com/vseplet/f2f/source/helper/services/firewall"
	"github.com/vseplet/f2f/source/helper/services/notify"
	"github.com/vseplet/f2f/source/helper/services/oidc"
	"github.com/vseplet/f2f/source/helper/services/pki"
	"github.com/vseplet/f2f/source/helper/services/proxy"
	"github.com/vseplet/f2f/source/helper/services/secrets"
	"github.com/vseplet/f2f/source/helper/services/shell"
	"github.com/vseplet/f2f/source/helper/services/tunnel"
	"github.com/vseplet/f2f/source/helper/services/vnc"
	"github.com/vseplet/f2f/source/helper/ui/web"
)

const defaultBind = "127.0.0.1:2202"

// service is the uniform shape every f2f service is wrapped in inside
// main.go — start on engine ready, stop on engine teardown, and
// optionally one long-lived worker tied to the process root ctx.
// Closures avoid touching individual service packages (their public
// APIs are intentionally varied — drop wants a goroutine, calls has
// no Start, etc.); main just dispatches.
type service struct {
	name  string
	start func(localIP string, st engine.Status) error
	stop  func() error
	run   func(ctx context.Context) // nil = no worker
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	args := os.Args[1:]

	// `f2f camp …` — camp management (create/list/use/join/rm/invite).
	// Runs and exits: no engine, no UI. Needs only the config store.
	if len(args) > 0 && args[0] == "camp" {
		store, err := config.NewStore()
		if err != nil {
			fmt.Fprintln(os.Stderr, "config store:", err)
			os.Exit(1)
		}
		if err := cli.RunCamp(store, args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	// `f2f remote …` — interactive TUI to expose this node's terminal/desktop
	// to channels. Talks to the running helper's loopback API; no engine here.
	if len(args) > 0 && args[0] == "remote" {
		if err := cli.RunRemote(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	// `f2f up [flags]` — non-interactive: bring up the last-used camp
	// (login items / headless). Bare `f2f` shows the interactive picker.
	autostart := false
	if len(args) > 0 && args[0] == "up" {
		autostart = true
		args = args[1:]
	}

	fs := flag.NewFlagSet("f2f", flag.ExitOnError)
	bind := fs.String("bind", defaultBind, "HTTP bind address for the loopback UI")
	console := fs.Bool("console", false, "also mirror logs to the console; by default logs go to the file only")
	_ = fs.Parse(args)

	if err := run(*bind, *console, autostart); err != nil {
		clog.Fatal("%v", err)
	}
}

func run(bind string, console bool, autostart bool) error {
	store, err := config.NewStore()
	if err != nil {
		return fmt.Errorf("config store: %w", err)
	}

	eng := engine.New()
	eng.SetDefaultListen(":0") // ephemeral; camp learns reflex after NAT

	// Centralised logging: log.* → file (+ UI tap), console only with
	// --console. clog.Console() is the always-visible channel.
	logCloser, err := clog.Init(filepath.Join(store.Dir(), "f2f.log"), console)
	if err != nil {
		return err
	}
	defer logCloser.Close()

	// Peer-to-peer QUIC data bus over the overlay. Started when the overlay
	// comes up (OnStarted); it auto-meshes with every reachable peer. All
	// peer↔peer service traffic rides it.
	busSvc, err := bus.New(busResolver{eng: eng})
	if err != nil {
		return fmt.Errorf("bus: %w", err)
	}
	defer busSvc.Stop()

	fwSvc := firewall.New(store, eng, busSvc)
	fwSvc.Register()
	pkiSvc := pki.New(store, eng, busSvc)
	pkiSvc.Register()
	dnsSvc := dns.New(store, eng, busSvc)
	dnsSvc.Register()
	callsSvc := calls.New(store, eng, busSvc)
	callsSvc.Register()
	tunnelSvc := tunnel.New(store, eng, busSvc)
	tunnelSvc.Register()
	// Domain intercepts resolve on the exit peer; mirror the answers
	// into the local DNS so apps here resolve those names to the same
	// IPs the intercept routes cover.
	tunnelSvc.OnDomainPinned = dnsSvc.SetPinned
	tunnelSvc.OnDomainUnpinned = dnsSvc.RemovePinned
	// On-demand subdomains: a query under an intercept zone with no exact
	// pin (e.g. www.myip.com under a myip.com intercept) gets resolved on
	// the exit peer and routed on the fly, so the user needn't add every
	// subdomain by hand.
	dnsSvc.OnPinnedMiss = tunnelSvc.ResolveSubdomain
	campSvc := camp.New(eng, store)
	// Built-in OIDC provider: turns overlay identity into standard tokens
	// for co-located apps. Served on a loopback port; the proxy exposes it
	// at id.<zone>.f2f and injects the attested caller pub.
	const oidcPort = 2203
	// camp dir holds shared/data state (db.sqlite, content); the OIDC provider's
	// own files (oidc_rsa.pem signing key + clients.json registry) live grouped
	// under ~/.f2f/<camp>/oidc/.
	campDir := func() string {
		id := eng.Status().CampID
		if id == "" {
			return ""
		}
		return store.CampDir(id)
	}
	oidcDir := func() string {
		id := eng.Status().CampID
		if id == "" {
			return ""
		}
		return store.OIDCDir(id)
	}
	oidcClients := oidc.NewClientStore(oidcDir)
	oidcKeys := oidc.NewSignKeys(oidcDir)

	// Distributed DB substrate: one append-only signed log per camp
	// (db.sqlite), replicated by anti-entropy over the bus. Block apps
	// (notes now; docs/tasks/chat later) build on it. Push on every local
	// commit; PullAll periodically (below) catches up offline gaps. Built
	// before OIDC so the provider can read passkeys/profile from block.profile.
	dbSvc := db.New(db.NewSQLiteStore(campDir))
	blocksMgr := blocks.New(dbSvc)

	// File sharing: drop reads the blocks itself to learn which torrent files
	// are referenced and in which channel scope (no file-scopes.json).
	dropSvc := drop.New(eng, store.CampDir, busSvc, blocksMgr)
	dropSvc.Register()

	oidcSvc := oidc.New(oidcBackend{eng: eng}, oidcClients, oidcKeys)
	// Login creds + display name come solely from the synced block.profile
	// (scope "profiles", keyed by peer pub) — there is no local passkeys.json.
	oidcSvc.SetProfileSource(oidcProfiles{blocks: blocksMgr})
	proxySvc := proxy.New(dnsSvc, pkiSvc, oidcPort, busResolver{eng: eng}.PubForIP)

	channelsMgr := channels.New(blocksMgr) // channels are blocks in the "channels" scope
	msgMgr := message.New(blocksMgr)       // messages are blocks in "message:<channelBid>"
	dbSync := db.NewSync(dbSvc, dbBus{busSvc})
	dbSync.Register()
	dbSvc.OnCommit(dbSync.Push)
	// Membership-gating: serve a scope to a peer only if it belongs to the
	// channel. Channel meta ("channel:<bid>"), messages ("message:<bid>") and
	// notes ("note:<bid>") all key off the channel bid; other scopes are open.
	dbSync.SetMemberCheck(func(scope, pub string) bool {
		var bid string
		switch {
		case strings.HasPrefix(scope, channels.ScopePrefix):
			bid = strings.TrimPrefix(scope, channels.ScopePrefix)
		case strings.HasPrefix(scope, message.ScopePrefix):
			bid = strings.TrimPrefix(scope, message.ScopePrefix)
		case strings.HasPrefix(scope, "note:"):
			bid = strings.TrimPrefix(scope, "note:")
		default:
			return true // non-channel scope → open
		}
		return channelsMgr.IsMember(bid, pub)
	})

	// Messaging is now blocks (db/blocks/message + channels) — see channelsMgr/
	// msgMgr above. Scoped (channel/DM) files are served over torrent only to
	// members of the channel; the drop catalog asks the channel registry.
	dropSvc.SetMembershipCheck(channelsMgr.IsMember)

	// Secrets vault store. Not on the block log (secrets must be mutable +
	// truly deletable); a separate per-camp sqlite. Channel-scoped vaults are
	// served to fellow members on demand over the bus, gated by IsMember.
	secretsSvc := secrets.New(eng, store.CampDir, busSvc)
	secretsSvc.SetMembershipCheck(channelsMgr.IsMember)
	// Gate app login by channel membership: only members of an app's listed
	// channels may authorize (same predicate as messages/secrets/drop).
	oidcSvc.SetMembershipCheck(channelsMgr.IsMember)
	// Gate domains: a remote peer may reach a domain only if it's a member of
	// one of the domain's channels (loopback/owner bypasses). The dns service
	// applies the same gate to discovery (a non-member never learns the name).
	proxySvc.SetMembershipCheck(channelsMgr.IsMember)
	dnsSvc.SetMembershipCheck(channelsMgr.IsMember)
	secretsSvc.Register()

	// Notification hub — fans UI notifications out over SSE. Peers can push
	// notifications to us over the bus ("notify" type); we also surface peer
	// presence (bus link up/down) and inbound chat/call activity below.
	notifySvc := notify.New(store.CampDir, func() string { return eng.Status().CampID })
	defer notifySvc.Close()
	busSvc.Handle("notify", notifySvc.FromBus)
	busSvc.Handle("notify.push", notifySvc.FromBus) // new name, accepted during the wire rollout

	// peerName resolves a peer pub to its display name (falls back to a short
	// fingerprint) from the live roster — used to title presence/chat alerts.
	peerName := func(pub string) string {
		for _, p := range eng.Status().Peers {
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

	// Peer presence: the bus reports a reachability change as "up"/"down".
	// Surface it as a peer notification routed to that peer's DM.
	busSvc.Events = func(_, peerPub, text string) {
		up := text == "up"
		state := "offline"
		if up {
			state = "online"
		}
		notifySvc.Push(notify.Notification{
			Kind:  "peer",
			Title: peerName(peerPub) + " " + state,
			From:  peerPub,
			Route: "channel:" + peerPub, // a DM is the degenerate channel
		})
	}

	// Inbound-message notifications are raised by the web layer when a remote
	// message frame syncs in (OnFrameApplied), so no messenger bridge here.

	// gossip: replicate our fabric-level NodeState (platform + peer-view)
	// across the mesh. Source assembles it from engine.Status() + runtime.
	gossipSvc := gossip.New(busSvc, func() gossip.NodeState {
		st := eng.Status()
		ns := gossip.NodeState{
			Pub: st.IdentityPub,
			Platform: gossip.Platform{
				OS:     runtime.GOOS,
				Arch:   runtime.GOARCH,
				NumCPU: runtime.NumCPU(),
				Go:     runtime.Version(),
			},
		}
		if h, err := os.Hostname(); err == nil {
			ns.Platform.Hostname = h
		}
		for _, p := range st.Peers {
			if p.Self {
				ns.Name = p.Name // our display name lives on the self entry
				continue
			}
			if p.Pub == "" {
				continue
			}
			ns.Sees = append(ns.Sees, gossip.PeerLink{
				Pub: p.Pub, Name: p.Name, Paired: p.Paired, Reachable: p.Reachable, RTTMs: p.RTTMs,
			})
		}
		return ns
	})

	// Remote-terminal service over the bus (mosh-like PTY, survives sleep).
	// Registers its bus handlers now; the web layer bridges a browser
	// xterm.js WebSocket to a bus stream opened here.
	shellSvc := shell.New(busSvc)
	shellSvc.SetMembershipCheck(channelsMgr.IsMember)
	shellSvc.Register()

	// Remote-desktop bridge over the bus — proxies to the host's local VNC
	// server (macOS Screen Sharing :5900 / x11vnc / …). noVNC in the UI.
	vncSvc := vnc.New(busSvc)
	vncSvc.SetMembershipCheck(channelsMgr.IsMember)
	vncSvc.Register()

	srv := web.New(eng, store, fwSvc, pkiSvc, dnsSvc, dropSvc, callsSvc, tunnelSvc, campSvc, dbSvc, notifySvc, gossipSvc, shellSvc, vncSvc, oidcSvc, secretsSvc, blocksMgr, channelsMgr, msgMgr, bind)
	srv.RegisterBus(busSvc) // inbound meet signalling + bus-first outbound
	// Remote block entries (sync) → live-refresh any open editor in the browser.
	dbSvc.OnApply(srv.OnFrameApplied)

	// Service registry. Start order top-to-bottom, Stop reverse.
	// Workers are spawned once and live for the whole process.
	services := []service{
		{
			name:  "firewall",
			start: func(localIP string, st engine.Status) error { return fwSvc.Start(st.UtunName, localIP, st.CampID) },
			stop:  fwSvc.Stop,
			run:   fwSvc.PollPeers,
		},
		{
			name:  "dns",
			start: func(_ string, st engine.Status) error { return dnsSvc.Start(st.CampID, identity.CampLabel(st.CampID)) },
			stop:  dnsSvc.Stop,
			run:   dnsSvc.PollPeers,
		},
		{
			name: "dns-health",
			run:  dnsSvc.HealthCheck,
		},
		{
			name:  "pki",
			start: func(_ string, st engine.Status) error { return pkiSvc.Start(st.CampID) },
			stop:  pkiSvc.Stop,
			run:   pkiSvc.PollPeers,
		},
		{
			name:  "tunnel",
			start: func(_ string, st engine.Status) error { tunnelSvc.Start(st.CampID); return nil },
			stop:  func() error { tunnelSvc.Stop(); return nil },
			run:   tunnelSvc.RefreshDomainRoutes,
		},
		{
			name: "camp",
			start: func(_ string, st engine.Status) error {
				if st.CampID == "" {
					return nil
				}
				c, err := store.SnapshotCamp(st.CampID)
				if err != nil || c == nil {
					return err
				}
				return campSvc.Start(c)
			},
			stop: campSvc.Stop,
		},
		{
			name: "drop",
			start: func(localIP string, st engine.Status) error {
				// anacrolix can take a moment to bind and has been
				// known to panic during init — isolate.
				go func() {
					defer func() {
						if r := recover(); r != nil {
							clog.Error("drop", "PANIC during startup: %v", r)
						}
					}()
					clog.Info("drop", "initialising torrent client …")
					if err := dropSvc.Start(st.CampID, localIP); err != nil {
						clog.Warn("drop", "%v (file sharing disabled)", err)
					}
				}()
				return nil
			},
			stop: dropSvc.Stop,
			run:  dropSvc.PollPeers,
		},
		{
			name:  "secrets",
			start: func(_ string, st engine.Status) error { secretsSvc.Start(st.CampID); return nil },
		},
		{
			// Apply the persisted remote-access exposure (which channels may open
			// our shell / desktop) from the camp config.
			name: "remote",
			start: func(_ string, st engine.Status) error {
				if st.CampID == "" {
					return nil
				}
				c, err := store.SnapshotCamp(st.CampID)
				if err != nil || c == nil {
					return err
				}
				shellSvc.SetChannels(c.Shell.Channels, c.Shell.Command)
				vncSvc.SetChannels(c.Vnc.Channels, c.Vnc.Addr)
				return nil
			},
		},
		{
			name: "calls",
			stop: func() error { callsSvc.Reset(); return nil },
			run:  callsSvc.PollPeers,
		},
	}

	// portal banner is printed once per camp, not on every (re)start —
	// the wake-from-sleep detector can restart the engine repeatedly.
	var lastPortalCamp string
	eng.OnStarted = func(localIP string) {
		st := eng.Status()
		// Route logs into the per-camp dir for the lifetime of this camp.
		if st.CampID != "" {
			if err := clog.SwitchTo(filepath.Join(store.CampDir(st.CampID), "f2f.log")); err != nil {
				clog.Warn("main", "switch camp log: %v", err)
			}
		}
		for _, s := range services {
			if s.start == nil {
				continue
			}
			if err := s.start(localIP, st); err != nil {
				clog.Error("main", "%s start: %v", s.name, err)
			}
		}
		// After services: pki has loaded the CA, so the proxy can bind
		// :443 with on-demand leaf certs (not just :80).
		if err := proxySvc.Start(localIP, st.CampID); err != nil {
			clog.Warn("main", "bind http proxies: %v", err)
		}
		// QUIC data bus on the overlay IP — auto-meshes with peers.
		if err := busSvc.Start(localIP); err != nil {
			clog.Warn("main", "start bus: %v", err)
		}
		gossipSvc.Start() // replicate NodeState across the mesh
		// Ensure the camp-wide general channel exists (everyone has it). No-op
		// if already present locally or pulled from a peer.
		if st.CampID != "" {
			if id := eng.Identity(); id != nil {
				if _, err := channelsMgr.EnsureGeneral(id); err != nil {
					clog.Warn("main", "ensure general channel: %v", err)
				}
			}
		}
		if st.CampID != "" && st.CampID != lastPortalCamp {
			clog.Console("portal: https://portal.%s.f2f", identity.CampLabel(st.CampID))
			lastPortalCamp = st.CampID
		}
	}
	eng.OnStopped = func() {
		gossipSvc.Stop()
		_ = busSvc.Stop()
		_ = proxySvc.Stop()
		for i := len(services) - 1; i >= 0; i-- {
			s := services[i]
			if s.stop == nil {
				continue
			}
			if err := s.stop(); err != nil {
				clog.Warn("main", "%s stop: %v", s.name, err)
			}
		}
		// Camp-less again — route logs back to the bootstrap file.
		_ = clog.SwitchTo(filepath.Join(store.Dir(), "f2f.log"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Choose the camp to bring up BEFORE starting the workers and the UI
	// server: the interactive huh picker must own a clean terminal, with
	// no concurrent log lines or the UI banner corrupting its redraws.
	// Non-interactive (`f2f up` / no TTY) auto-selects the last-used camp
	// and returns immediately. Camp provisioning + selection live in
	// package cli now (the engine no longer owns any of this).
	mgr := cli.NewManager(store)
	interactive := !autostart && cli.Interactive()
	selCamp, selIdt, selErr := mgr.SelectCamp(interactive)

	// Long-lived workers tied to the process root ctx. They survive
	// engine restarts (each tick checks engine state, no-ops when down).
	var workerDone []chan struct{}
	for _, s := range services {
		if s.run == nil {
			continue
		}
		d := make(chan struct{})
		workerDone = append(workerDone, d)
		go func(fn func(context.Context), d chan struct{}) {
			defer close(d)
			fn(ctx)
		}(s.run, d)
	}

	go func() {
		clog.Console("f2f UI on http://%s", bind)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			clog.Console("UI server error: %v (engine continues; fix --bind and restart)", err)
		}
	}()

	// OIDC provider listener (loopback). The handler reads live engine
	// state, so it's bound once and survives camp switches.
	go func() {
		oidcSrv := &http.Server{
			Addr:              net.JoinHostPort("127.0.0.1", strconv.Itoa(oidcPort)),
			Handler:           oidcSvc.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		if err := oidcSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			clog.Warn("main", "oidc listener: %v", err)
		}
	}()

	// db anti-entropy: pull from peers periodically to catch up gaps (push
	// on commit handles the live path). No-ops when not in a camp / no peers.
	go func() {
		t := time.NewTicker(7 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if eng.Status().CampID == "" {
					continue
				}
				c, cancel := context.WithTimeout(ctx, 5*time.Second)
				dbSync.PullAll(c)
				cancel()
			}
		}
	}()

	// Bring up the chosen camp (eng.Start → OnStarted → services start).
	if selErr != nil {
		clog.Console("camp select: %v", selErr)
	} else if selCamp != nil {
		cfg := engine.Config{
			LocalIP:  "100.64.0.1", // placeholder; engine derives the overlay-IP from pub
			Listen:   ":9000",
			Camp:     selCamp,
			Identity: selIdt,
		}
		if err := eng.Start(cfg); err != nil {
			clog.Console("start camp: %v", err)
		}
	} else {
		clog.Console("no camp selected — run `f2f camp new` / `join`, or use the UI")
	}

	<-ctx.Done()
	clog.Info("main", "shutting down…")

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	if err := eng.Stop(); err != nil {
		clog.Warn("main", "engine stop: %v", err)
	}
	for _, d := range workerDone {
		select {
		case <-d:
		case <-time.After(2 * time.Second):
			clog.Warn("main", "service worker did not exit in 2s")
		}
	}
	return nil
}

// busResolver adapts the engine's peer roster to bus.Resolver. Identity is
// the overlay IP (WireGuard-attested), so the bus needs no auth of its own.
// oidcBackend adapts the engine to services/oidc.Backend: the active
// camp's signing identity and a pub→name lookup for the attested visitor.
// (The issuer is derived per-request from the app host, not here.)
type oidcBackend struct{ eng *engine.Engine }

func (b oidcBackend) Identity() *identity.Identity { return b.eng.Identity() }

func (b oidcBackend) PeerName(pub string) string {
	for _, p := range b.eng.Status().Peers {
		if p.Pub == pub {
			return p.Name
		}
	}
	return ""
}

// profileFromBlocks reads a peer's block.profile (well-known "profiles" scope,
// keyed by pub) and returns its public passkey credentials and display name
// (first+last). Empty when there's no profile. Wired into OIDC via
// SetProfileSource so login verifies against the synced profile, not the local
// passkeys.json.
func profileFromBlocks(b *blocks.Manager, pub string) (creds []webauthn.Credential, first, last string) {
	blk := b.Block("profiles", pub)
	if blk == nil || len(blk.Heads) == 0 {
		return nil, "", ""
	}
	var c struct {
		First    string                `json:"first"`
		Last     string                `json:"last"`
		Passkeys []webauthn.Credential `json:"passkeys"`
	}
	if err := json.Unmarshal(blk.Heads[len(blk.Heads)-1].Content, &c); err != nil {
		return nil, "", ""
	}
	return c.Passkeys, c.First, c.Last
}

// oidcProfiles implements oidc.ProfileSource over the block engine: OIDC login
// reads passkey creds, display names, and the enrolled-users list straight from
// the synced block.profile (scope "profiles"), with no local passkeys.json.
type oidcProfiles struct{ blocks *blocks.Manager }

func (p oidcProfiles) Creds(pub string) []webauthn.Credential {
	c, _, _ := profileFromBlocks(p.blocks, pub)
	return c
}

func (p oidcProfiles) Profile(pub string) (name, given, family string) {
	_, first, last := profileFromBlocks(p.blocks, pub)
	return strings.TrimSpace(first + " " + last), first, last
}

func (p oidcProfiles) WithCreds() map[string]int {
	out := map[string]int{}
	for _, b := range p.blocks.Blocks("profiles") {
		if b == nil || b.Deleted || len(b.Heads) == 0 {
			continue
		}
		var c struct {
			Passkeys []webauthn.Credential `json:"passkeys"`
		}
		if json.Unmarshal(b.Heads[len(b.Heads)-1].Content, &c) == nil && len(c.Passkeys) > 0 {
			out[b.BID] = len(c.Passkeys)
		}
	}
	return out
}

// dbBus adapts *bus.Service to db.Bus. The only wrinkle is Handle: db.Bus
// uses a plain func type (to stay decoupled from mesh/bus), assignable to
// bus.HandlerFunc here.
type dbBus struct{ b *bus.Service }

func (a dbBus) Handle(typ string, fn func(string, []byte) ([]byte, error)) { a.b.Handle(typ, fn) }
func (a dbBus) Request(ctx context.Context, pub, typ string, payload []byte) ([]byte, error) {
	return a.b.Request(ctx, pub, typ, payload)
}
func (a dbBus) Notify(pub, typ string, payload []byte) error { return a.b.Notify(pub, typ, payload) }
func (a dbBus) Peers() []string                              { return a.b.Peers() }

type busResolver struct{ eng *engine.Engine }

func (r busResolver) AddrForPub(pub string) string {
	for _, p := range r.eng.Status().Peers {
		if !p.Self && p.Pub == pub {
			return p.OverlayV4
		}
	}
	return ""
}

func (r busResolver) PubForIP(ip string) string {
	for _, p := range r.eng.Status().Peers {
		if p.OverlayV4 == ip {
			return p.Pub
		}
	}
	return ""
}

func (r busResolver) NameForPub(pub string) string {
	for _, p := range r.eng.Status().Peers {
		if !p.Self && p.Pub == pub {
			return p.Name
		}
	}
	return ""
}

func (r busResolver) SelfPub() string { return r.eng.Status().IdentityPub }

func (r busResolver) Peers() []string {
	st := r.eng.Status()
	var out []string
	for _, p := range st.Peers {
		if p.Self || p.Pub == "" || p.OverlayV4 == "" || !p.InCamp {
			continue
		}
		// Skip offline members: dialing them just burns the 5s ping
		// timeout and spams "ping failed" — they reappear here as soon
		// as the camp roster marks them online again.
		if !p.Online {
			continue
		}
		// Defensive self-exclusion: the camp-owner entry can appear without
		// the Self flag set, which would make us ping ourselves.
		if p.Pub == st.IdentityPub || p.OverlayV4 == st.LocalIP {
			continue
		}
		out = append(out, p.Pub)
	}
	return out
}
