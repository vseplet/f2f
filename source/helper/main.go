// f2f-mac is the macOS-side CLI for the f2f UDP tunnel. Launches the
// web UI on 127.0.0.1:2202 (overridable via --bind). Needs sudo for
// utun + routing + pf.
package main

import (
	"context"
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
	"syscall"
	"time"

	"github.com/vseplet/f2f/source/helper/clog"
	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/engine"
	"github.com/vseplet/f2f/source/helper/identity"
	"github.com/vseplet/f2f/source/helper/mesh/bus"
	"github.com/vseplet/f2f/source/helper/mesh/gossip"
	"github.com/vseplet/f2f/source/helper/services/calls"
	"github.com/vseplet/f2f/source/helper/services/camp"
	"github.com/vseplet/f2f/source/helper/services/dns"
	"github.com/vseplet/f2f/source/helper/services/drop"
	"github.com/vseplet/f2f/source/helper/services/firewall"
	"github.com/vseplet/f2f/source/helper/services/messenger"
	"github.com/vseplet/f2f/source/helper/services/notify"
	"github.com/vseplet/f2f/source/helper/services/pki"
	"github.com/vseplet/f2f/source/helper/services/proxy"
	"github.com/vseplet/f2f/source/helper/services/tunnel"
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

	bind := flag.String("bind", defaultBind, "HTTP bind address for the loopback UI")
	console := flag.Bool("console", false, "also mirror logs to the console; by default logs go to the file only")
	flag.Parse()

	if err := run(*bind, *console); err != nil {
		clog.Fatal("%v", err)
	}
}

func run(bind string, console bool) error {
	store, err := config.NewStore()
	if err != nil {
		return fmt.Errorf("config store: %w", err)
	}

	eng := engine.New(store)
	eng.SetDefaultListen(":0") // ephemeral; camp learns reflex after NAT

	// Centralised logging: log.* → file (+ UI tap), console only with
	// --console. clog.Console() is the always-visible channel.
	logCloser, err := clog.Init(filepath.Join(store.Dir(), "f2f.log"), console, eng.LogTap())
	if err != nil {
		return err
	}
	defer logCloser.Close()

	fwSvc := firewall.New(store, eng)
	pkiSvc := pki.New(store, eng)
	dnsSvc := dns.New(store, eng)
	dropSvc := drop.New(eng)
	callsSvc := calls.New(store, eng)
	tunnelSvc := tunnel.New(store, eng)
	campSvc := camp.New(eng)
	proxySvc := proxy.New(dnsSvc, pkiSvc)

	// Local message/channel store — one SQLite file per camp under ~/.f2f
	// (<camp_id>.messenger.db), opened lazily.
	msgSvc := messenger.New(store.Dir())
	defer msgSvc.Close()

	// Peer-to-peer QUIC data bus over the overlay. Started when the overlay
	// comes up (OnStarted); it auto-meshes with every reachable peer.
	busSvc, err := bus.New(busResolver{eng: eng})
	if err != nil {
		return fmt.Errorf("bus: %w", err)
	}
	defer busSvc.Stop()

	// Notification hub — fans UI notifications out over SSE. Peers can push
	// notifications to us over the bus ("notify" type); bus activity (pings)
	// is surfaced too.
	notifySvc := notify.New()
	busSvc.Handle("notify", notifySvc.FromBus)
	busSvc.Events = func(kind, peerPub, text string) {
		notifySvc.Push(notify.Notification{Kind: kind, Title: text, From: peerPub})
	}

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

	srv := web.New(eng, store, fwSvc, pkiSvc, dnsSvc, dropSvc, callsSvc, tunnelSvc, campSvc, msgSvc, notifySvc, gossipSvc, bind)

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
							log.Printf("drop: PANIC during startup: %v", r)
						}
					}()
					log.Printf("drop: initialising torrent client …")
					if err := dropSvc.Start(st.CampID, localIP); err != nil {
						log.Printf("drop: %v (file sharing disabled)", err)
					}
				}()
				return nil
			},
			stop: dropSvc.Stop,
			run:  dropSvc.PollPeers,
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
		if err := srv.BindTunnel(localIP); err != nil {
			log.Printf("WARN: bind tunnel inbox: %v", err)
		}
		st := eng.Status()
		for _, s := range services {
			if s.start == nil {
				continue
			}
			if err := s.start(localIP, st); err != nil {
				log.Printf("%s: %v", s.name, err)
			}
		}
		// After services: pki has loaded the CA, so the proxy can bind
		// :443 with on-demand leaf certs (not just :80).
		if err := proxySvc.Start(localIP, st.CampID); err != nil {
			log.Printf("WARN: bind http proxies: %v", err)
		}
		// QUIC data bus on the overlay IP — auto-meshes with peers.
		if err := busSvc.Start(localIP); err != nil {
			log.Printf("WARN: start bus: %v", err)
		}
		gossipSvc.Start() // replicate NodeState across the mesh
		if st.CampID != "" && st.CampID != lastPortalCamp {
			clog.Console("portal: https://portal.%s.f2f", identity.CampLabel(st.CampID))
			lastPortalCamp = st.CampID
		}
	}
	eng.OnStopped = func() {
		_ = srv.UnbindTunnel()
		gossipSvc.Stop()
		_ = busSvc.Stop()
		_ = proxySvc.Stop()
		for i := len(services) - 1; i >= 0; i-- {
			s := services[i]
			if s.stop == nil {
				continue
			}
			if err := s.stop(); err != nil {
				log.Printf("WARN: %s stop: %v", s.name, err)
			}
		}
	}
	if _, port, err := net.SplitHostPort(bind); err == nil {
		eng.SetTunnelHTTPPort(port)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	go func() {
		if err := eng.StartLastCamp(); err != nil {
			log.Printf("autostart: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down…")

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	if err := eng.Stop(); err != nil {
		log.Printf("WARN: engine stop: %v", err)
	}
	for _, d := range workerDone {
		select {
		case <-d:
		case <-time.After(2 * time.Second):
			log.Printf("WARN: service worker did not exit in 2s")
		}
	}
	return nil
}

// busResolver adapts the engine's peer roster to bus.Resolver. Identity is
// the overlay IP (WireGuard-attested), so the bus needs no auth of its own.
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

func (r busResolver) SelfPub() string { return r.eng.Status().IdentityPub }

func (r busResolver) Peers() []string {
	st := r.eng.Status()
	var out []string
	for _, p := range st.Peers {
		if p.Self || p.Pub == "" || p.OverlayV4 == "" || !p.InCamp {
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
