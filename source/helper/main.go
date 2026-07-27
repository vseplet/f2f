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
	"github.com/vseplet/f2f/source/helper/cli"
	"github.com/vseplet/f2f/source/helper/clog"
	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/db"
	"github.com/vseplet/f2f/source/helper/db/blocks"
	"github.com/vseplet/f2f/source/helper/db/blocks/channels"
	"github.com/vseplet/f2f/source/helper/db/blocks/message"
	"github.com/vseplet/f2f/source/helper/identity"
	"github.com/vseplet/f2f/source/helper/mesh/bus"
	"github.com/vseplet/f2f/source/helper/mesh/camp"
	"github.com/vseplet/f2f/source/helper/mesh/engine"
	"github.com/vseplet/f2f/source/helper/mesh/gossip"
	"github.com/vseplet/f2f/source/helper/services/calls"
	"github.com/vseplet/f2f/source/helper/services/dns"
	"github.com/vseplet/f2f/source/helper/services/drop"
	"github.com/vseplet/f2f/source/helper/services/firewall"
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

// version is the build version, stamped by CI via
// -ldflags "-X main.version=<tag>". "dev" for local builds.
var version = "dev"

// runMode is how this process runs. Only the CLI/startup path branches on it
// for now (camp selection + logging); the engine and services are identical.
// The restricted-services / peer-allowlist behaviour of service/task comes
// later — this step only wires the modes into the CLI.
type runMode int

const (
	modePortal  runMode = iota // interactive, with the web portal (default)
	modeService                // headless, long-lived, no human (a server)
	modeTask                   // headless, short-lived (a CI runner)
)

func (m runMode) String() string {
	switch m {
	case modeService:
		return "service"
	case modeTask:
		return "task"
	default:
		return "portal"
	}
}

// role is what this process publishes to the camp roster (peers act on it):
// portal is a "user" (a live person behind a browser/Touch ID), service a
// headless long-lived node, task an ephemeral CI runner. Distinct from
// String() so the portal mode reads as "user" in the roster.
func (m runMode) role() string {
	switch m {
	case modeService:
		return "service"
	case modeTask:
		return "task"
	default:
		return "user"
	}
}

// runOpts is the parsed CLI: bundles the flags so run()'s signature stays small
// as modes grow.
type runOpts struct {
	bind      string
	console   bool
	verbose   bool // --logs: debug-level logging
	autostart bool // legacy `f2f up`: non-interactive, last-used camp
	mode      runMode
	campID    string // service/task: the camp to bring up
	name      string // service/task: this node's display name
	keyRef    string // task: pre-generated identity key (import TBD)
}

// service is the uniform shape every f2f service is wrapped in inside
// main.go — start on engine ready, stop on engine teardown, and
// optionally one long-lived worker tied to the process root ctx.
// Closures avoid touching individual service packages (their public
// APIs are intentionally varied — drop wants a goroutine, calls has
// no Start, etc.); main just dispatches.
//
// on gates the whole entry by run mode: a disabled service is neither
// started, stopped, nor given a worker. This is the single lever for "don't
// run this feature in service/task mode" — every start site goes through the
// registry so the gate is uniform.
type service struct {
	name  string
	on    bool
	start func(localIP string, st engine.Status) error
	stop  func() error
	run   func(ctx context.Context) // nil = no worker
}

// features is the per-mode capability set: which optional services start. The
// substrate (engine + bus + camp) is always on and not represented here — a
// node without it isn't in the mesh. Everything else is gated.
type features struct {
	firewall bool
	dns      bool
	pki      bool
	tunnel   bool
	drop     bool
	secrets  bool
	remote   bool // apply shell/desktop exposure from camp config
	calls    bool
	proxy    bool // :80/:443 reverse proxy for *.f2f
	gossip   bool
	web      bool // loopback web UI (the portal)
	oidc     bool // built-in OpenID provider
	db       bool // block engine sync + the general channel
}

func allFeatures() features {
	return features{
		firewall: true, dns: true, pki: true, tunnel: true, drop: true,
		secrets: true, remote: true, calls: true, proxy: true, gossip: true,
		web: true, oidc: true, db: true,
	}
}

// featuresFor maps a run mode to its capability set. Presets for now; per-feature
// override flags can layer on later.
//
//	portal  — everything (the human workstation).
//	service — headless server: hosts/serves, no heavy media (calls) or file
//	          sharing (drop). The web feature IS on, but only for its loopback
//	          API (127.0.0.1:2202) that `f2f tui` drives — the portal.<zone>.f2f
//	          domain is loopback-only and never exposed to peers. OIDC IS on too:
//	          it serves co-located apps (e.g. Gitea) over the proxy, exactly what
//	          a headless IdP node needs. Tunable starting point.
//	task    — ephemeral client: substrate only (engine/bus/camp). It dials OUT
//	          to a chosen node and exposes nothing, so nothing optional starts —
//	          not even the firewall (it gates by port, but the task serves no
//	          ports; real per-peer restriction is the allowlist, coming later).
func featuresFor(m runMode) features {
	switch m {
	case modeService:
		return features{
			firewall: true, dns: true, pki: true, tunnel: true,
			secrets: true, remote: true, proxy: true, gossip: true, db: true,
			oidc: true,
			// web = the loopback API server (127.0.0.1:2202). On a headless node
			// nobody opens the portal HTML, but `f2f tui` — the whole reason a
			// service node needs managing without a browser — talks to this API.
			web: true,
		}
	case modeTask:
		return features{} // substrate only
	default:
		return allFeatures()
	}
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	args := os.Args[1:]

	// `f2f camp …` — disabled: creating/joining a camp is done through the
	// interactive portal (bare `f2f`), so the subcommand is a duplicate. Kept
	// commented for reference.
	// if len(args) > 0 && args[0] == "camp" {
	// 	store, err := config.NewStore()
	// 	if err != nil {
	// 		fmt.Fprintln(os.Stderr, "config store:", err)
	// 		os.Exit(1)
	// 	}
	// 	if err := cli.RunCamp(store, args[1:]); err != nil {
	// 		fmt.Fprintln(os.Stderr, "error:", err)
	// 		os.Exit(1)
	// 	}
	// 	return
	// }

	// `f2f tui …` — terminal control panel for a running helper (headless
	// nodes / VPS without a browser). Thin loopback client to the same API the
	// web portal uses: status, cert trust, domains, tunnels, shell/desktop
	// exposure, OIDC clients, calls.
	if len(args) > 0 && args[0] == "tui" {
		if err := cli.RunTUI(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	// `f2f version` / `--version` — print the build version and exit.
	if len(args) > 0 && (args[0] == "version" || args[0] == "--version" || args[0] == "-version") {
		fmt.Println(version)
		return
	}

	// `f2f up` — disabled: `--service` is the headless bring-up now. Left
	// commented; autostart stays false so bare `f2f` is interactive.
	autostart := false
	// if len(args) > 0 && args[0] == "up" {
	// 	autostart = true
	// 	args = args[1:]
	// }

	// Three run modes, selected by flag; bare `f2f` (no mode flag) is the
	// interactive portal, exactly as before.
	//   f2f                                              → portal (interactive)
	//   f2f --service --camp <id> --name <n>             → service (headless)
	//   f2f --task    --camp <id> --name <n> --key <k>   → task    (ephemeral)
	fs := flag.NewFlagSet("f2f", flag.ExitOnError)
	bind := fs.String("bind", defaultBind, "HTTP bind address for the loopback UI")
	console := fs.Bool("console", false, "also mirror logs to the console; by default logs go to the file only")
	logs := fs.Bool("logs", false, "verbose (debug-level) logging; implies --console so you see them")
	service := fs.Bool("service", false, "headless service mode (requires --camp, --name)")
	task := fs.Bool("task", false, "ephemeral task mode (requires --camp, --name, --key)")
	campID := fs.String("camp", "", "camp_id to bring up (service/task modes)")
	name := fs.String("name", "", "this node's display name (service/task modes)")
	keyRef := fs.String("key", "", "pre-generated identity key (task mode)")
	_ = fs.Parse(args)

	opts := runOpts{
		bind:      *bind,
		console:   *console || *logs, // verbose logs are useless if not shown
		verbose:   *logs,
		autostart: autostart,
		mode:      modePortal,
		campID:    *campID,
		name:      *name,
		keyRef:    *keyRef,
	}
	switch {
	case *service && *task:
		fmt.Fprintln(os.Stderr, "error: --service and --task are mutually exclusive")
		os.Exit(1)
	case *task:
		opts.mode = modeTask
	case *service:
		opts.mode = modeService
	}
	if opts.mode != modePortal {
		if opts.campID == "" || opts.name == "" {
			fmt.Fprintf(os.Stderr, "error: --camp and --name are required in %s mode\n", opts.mode)
			os.Exit(1)
		}
		// Headless: there's no portal to read logs from, so surface them.
		opts.console = true
	}
	if opts.mode == modeTask && opts.keyRef == "" {
		fmt.Fprintln(os.Stderr, "error: --key is required in task mode")
		os.Exit(1)
	}

	if err := run(opts); err != nil {
		clog.Fatal("%v", err)
	}
}

func run(opts runOpts) error {
	bind, console := opts.bind, opts.console
	feat := featuresFor(opts.mode)
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
	if opts.verbose {
		clog.SetLevel(clog.LevelDebug) // --logs overrides the F2F_LOG default
	}
	clog.Console("f2f %s (%s/%s) — %s mode", version, runtime.GOOS, runtime.GOARCH, opts.mode)

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
	campSvc := camp.New(eng, store, version, opts.mode.role())
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

	srv := web.New(eng, store, fwSvc, pkiSvc, dnsSvc, dropSvc, callsSvc, tunnelSvc, campSvc, dbSvc, gossipSvc, shellSvc, vncSvc, oidcSvc, secretsSvc, blocksMgr, channelsMgr, msgMgr, bind)
	srv.SetVersion(version)
	srv.RegisterBus(busSvc) // inbound meet signalling + bus-first outbound
	// Remote block entries (sync) → live-refresh any open editor in the browser.
	dbSvc.OnApply(srv.OnFrameApplied)

	// Service registry — the SINGLE list of every start site. Camp-lifetime
	// entries (start/stop) run on engine up/down; process-lifetime entries (run)
	// are spawned once for the whole process. Order is start order (Stop is the
	// reverse); the substrate (camp, bus) is always on, the rest gated by feat.
	//
	// Note on ordering: proxy must come after pki (it needs the loaded CA to bind
	// :443), so the camp-lifetime block below stays pki → … → proxy.
	services := []service{
		{
			name: "firewall", on: feat.firewall,
			start: func(localIP string, st engine.Status) error { return fwSvc.Start(st.UtunName, localIP, st.CampID) },
			stop:  fwSvc.Stop,
			run:   fwSvc.PollPeers,
		},
		{
			name: "dns", on: feat.dns,
			start: func(_ string, st engine.Status) error {
				return dnsSvc.Start(st.CampID, identity.CampLabel(st.CampID), st.LocalIP)
			},
			stop: dnsSvc.Stop,
			run:  dnsSvc.PollPeers,
		},
		{
			name: "dns-health", on: feat.dns,
			run: dnsSvc.HealthCheck,
		},
		{
			name: "pki", on: feat.pki,
			start: func(_ string, st engine.Status) error { return pkiSvc.Start(st.CampID) },
			stop:  pkiSvc.Stop,
			run:   pkiSvc.PollPeers,
		},
		{
			name: "tunnel", on: feat.tunnel,
			start: func(_ string, st engine.Status) error { tunnelSvc.Start(st.CampID); return nil },
			stop:  func() error { tunnelSvc.Stop(); return nil },
			run:   tunnelSvc.RefreshDomainRoutes,
		},
		{
			name: "camp", on: true, // substrate: rendezvous
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
			name: "drop", on: feat.drop,
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
			name: "secrets", on: feat.secrets,
			start: func(_ string, st engine.Status) error { secretsSvc.Start(st.CampID); return nil },
		},
		{
			// Apply the persisted remote-access exposure (which channels may open
			// our shell / desktop) from the camp config.
			name: "remote", on: feat.remote,
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
			name: "calls", on: feat.calls,
			stop: func() error { callsSvc.Reset(); return nil },
			run:  callsSvc.PollPeers,
		},
		// --- moved here from inline OnStarted (camp-lifetime) ---
		{
			// After pki: the CA is loaded, so the proxy can bind :443 with
			// on-demand leaf certs (not just :80).
			name: "proxy", on: feat.proxy,
			start: func(localIP string, st engine.Status) error { return proxySvc.Start(localIP, st.CampID) },
			stop:  proxySvc.Stop,
		},
		{
			name: "bus", on: true, // substrate: QUIC data bus, auto-meshes with peers
			start: func(localIP string, _ engine.Status) error { return busSvc.Start(localIP) },
			stop:  busSvc.Stop,
		},
		{
			name: "gossip", on: feat.gossip,
			start: func(_ string, _ engine.Status) error { gossipSvc.Start(); return nil },
			stop:  func() error { gossipSvc.Stop(); return nil },
		},
		{
			// Ensure the camp-wide general channel exists (everyone has it). No-op
			// if already present locally or pulled from a peer. Needs the block db.
			name: "general", on: feat.db,
			start: func(_ string, st engine.Status) error {
				if st.CampID == "" {
					return nil
				}
				if id := eng.Identity(); id != nil {
					if _, err := channelsMgr.EnsureGeneral(id); err != nil {
						return err
					}
				}
				return nil
			},
		},
		// --- process-lifetime workers (moved here from run() goroutines) ---
		{
			name: "web", on: feat.web,
			run: func(_ context.Context) {
				clog.Console("f2f UI on http://%s", bind)
				if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					clog.Console("UI server error: %v (engine continues; fix --bind and restart)", err)
				}
			},
		},
		{
			name: "oidc", on: feat.oidc,
			run: func(ctx context.Context) {
				oidcSrv := &http.Server{
					Addr:              net.JoinHostPort("127.0.0.1", strconv.Itoa(oidcPort)),
					Handler:           oidcSvc.Handler(),
					ReadHeaderTimeout: 10 * time.Second,
				}
				go func() {
					<-ctx.Done()
					c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					_ = oidcSrv.Shutdown(c)
				}()
				if err := oidcSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					clog.Warn("main", "oidc listener: %v", err)
				}
			},
		},
		{
			// db anti-entropy: pull from peers periodically to catch up gaps (push
			// on commit handles the live path). No-ops when not in a camp / no peers.
			name: "db-sync", on: feat.db,
			run: func(ctx context.Context) {
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
			},
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
			if !s.on || s.start == nil {
				continue
			}
			if err := s.start(localIP, st); err != nil {
				clog.Error("main", "%s start: %v", s.name, err)
			}
		}
		// The portal banner is for a human at a browser — only portal mode has
		// one. A --service node runs the loopback API (for `f2f tui`) but its
		// portal.<zone>.f2f is loopback-only and unvisited, so don't advertise it.
		if opts.mode == modePortal && feat.proxy && st.CampID != "" && st.CampID != lastPortalCamp {
			clog.Console("portal: https://portal.%s.f2f", identity.CampLabel(st.CampID))
			lastPortalCamp = st.CampID
		}
	}
	eng.OnStopped = func() {
		for i := len(services) - 1; i >= 0; i-- {
			s := services[i]
			if !s.on || s.stop == nil {
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
	var (
		selCamp *config.Camp
		selIdt  *identity.Identity
		selErr  error
	)
	switch opts.mode {
	case modeService, modeTask:
		// Non-interactive, explicit camp: register it (idempotent; also marks
		// it last-used) then select it. No picker.
		if opts.mode == modeTask && opts.keyRef != "" {
			// TODO: import the pre-generated identity from opts.keyRef so the
			// task runs under a known fp. Not wired yet — logged for now.
			clog.Console("task key provided — identity import not yet implemented")
		}
		if _, jerr := mgr.Join(opts.campID, opts.name); jerr != nil {
			selErr = jerr
		} else {
			selCamp, selIdt, selErr = mgr.SelectCamp(false)
		}
	default: // modePortal
		interactive := !opts.autostart && cli.Interactive()
		selCamp, selIdt, selErr = mgr.SelectCamp(interactive)
	}

	// Long-lived workers tied to the process root ctx. They survive
	// engine restarts (each tick checks engine state, no-ops when down).
	// Includes the web UI, OIDC listener and db-sync — all now registry
	// entries, so the feat gate is the single place that turns them off.
	var workerDone []chan struct{}
	for _, s := range services {
		if !s.on || s.run == nil {
			continue
		}
		d := make(chan struct{})
		workerDone = append(workerDone, d)
		go func(fn func(context.Context), d chan struct{}) {
			defer close(d)
			fn(ctx)
		}(s.run, d)
	}

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
