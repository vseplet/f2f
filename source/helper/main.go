// f2f-mac is the macOS-side CLI for the f2f UDP tunnel. By default it
// launches the web UI on 127.0.0.1:2202 and lets the user drive the
// engine from a browser. `run` is a headless escape hatch (CLI only,
// no UI).
//
// Either mode needs sudo (utun + routing + pf).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/engine"
	"github.com/vseplet/f2f/source/helper/identity"
	"github.com/vseplet/f2f/source/helper/services/calls"
	"github.com/vseplet/f2f/source/helper/services/camp"
	"github.com/vseplet/f2f/source/helper/services/dns"
	"github.com/vseplet/f2f/source/helper/services/drop"
	"github.com/vseplet/f2f/source/helper/services/firewall"
	"github.com/vseplet/f2f/source/helper/services/pki"
	"github.com/vseplet/f2f/source/helper/services/tunnel"
	"github.com/vseplet/f2f/source/helper/ui/web"
)

const defaultBind = "127.0.0.1:2202"

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// First positional arg picks the mode. With no args, or with flags
	// like --bind, we default to UI.
	args := os.Args[1:]
	mode := "ui"
	if len(args) > 0 {
		switch args[0] {
		case "run":
			mode = "run"
			args = args[1:]
		case "-h", "--help", "help":
			usage()
			return
		}
	}
	var err error
	switch mode {
	case "run":
		err = runCmd(args)
	case "ui":
		err = uiCmd(args)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `f2f-mac — macOS-side traffic interceptor

Usage:
  sudo f2f-mac                          # launch web UI on %s
  sudo f2f-mac --bind 127.0.0.1:2202    # custom bind for the UI
  sudo f2f-mac run [--listen :PORT]     # headless mode (no UI)
                   [--local-ip 10.99.0.1] [--peer-ip 10.99.0.2]
                   [--egress-iface en0]
                   [--camp-url wss://… --name X --id Y]

Intercepts (domains/IPs to route through a specific peer) are managed
exclusively via the web UI — each entry must be bound to a peer at
creation time.

Rendezvous (Camp) mode:
  Both ends register on a shared camp; the engine adopts the other peer
  automatically once it announces.

  sudo f2f-mac run --listen :9000 \
                   --camp-url wss://f2f-camp.fly.dev/ws \
                   --name vasya --id beer

Manual rescue (if f2f-mac was kill -9'd and left state behind):
  sudo pfctl -a com.apple/f2f-mac -F all
  sudo sysctl -w net.inet.ip.forwarding=0   # only if it was 0 before f2f-mac
  sudo rm -f /var/run/f2f-mac.egress.json
`, defaultBind)
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	localIP := fs.String("local-ip", "10.99.0.1", "local end of the point-to-point address on utun")
	peerIP := fs.String("peer-ip", "10.99.0.2", "remote end of the point-to-point address on utun")
	listen := fs.String("listen", "", "UDP address to listen on (e.g. :9000)")
	peerAddr := fs.String("peer", "", "UDP address of the remote peer (e.g. 127.0.0.1:9001)")
	egressIface := fs.String("egress-iface", "", "physical interface to NAT tunnel traffic out of (empty = auto-detect default route)")
	campURL := fs.String("camp-url", "wss://f2f-camp.fly.dev/ws", "rendezvous WebSocket URL; Camp mode activates when --name and --id are both set")
	campStun := fs.String("camp-stun", "f2f-camp.fly.dev:3478", "STUN host:port for external endpoint discovery")
	campName := fs.String("name", "", "our identity on the rendezvous server (enables Camp mode together with --id)")
	campID := fs.String("id", "", "shared camp id on the rendezvous server (enables Camp mode together with --name)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	store, err := config.NewStore()
	if err != nil {
		return fmt.Errorf("config store: %w", err)
	}
	eng := engine.New(store)
	log.SetOutput(io.MultiWriter(os.Stderr, eng.LogTap()))

	cfg := engine.Config{
		LocalIP: *localIP,
		PeerIP:  *peerIP,
		Listen:  *listen,
		Peer:    *peerAddr,
	}
	_ = *egressIface // egress is now auto-detected inside services/tunnel
	if *campName != "" && *campID != "" {
		cfg.Camp = &engine.CampConfig{
			URL:      *campURL,
			Name:     *campName,
			ID:       *campID,
			StunAddr: *campStun,
		}
	}
	if err := eng.Start(cfg); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	log.Println("shutting down…")
	if err := eng.Stop(); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	return nil
}

func uiCmd(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	bind := fs.String("bind", defaultBind, "HTTP bind address for the loopback UI; default keeps the UI off the LAN")
	listen := fs.String("listen", ":0", "UDP address to listen on for peer transport; default :0 lets the kernel pick a free port (camp learns reflex after NAT anyway)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	store, err := config.NewStore()
	if err != nil {
		return fmt.Errorf("config store: %w", err)
	}

	eng := engine.New(store)
	eng.SetDefaultListen(*listen)
	log.SetOutput(io.MultiWriter(os.Stderr, eng.LogTap()))

	// Services riding on top of the engine. Construction is cheap and
	// stateless — actual lifecycle (firewall pf-anchor, dns server,
	// trust poll loop) is wired off eng.OnStarted/OnStopped below.
	fwSvc := firewall.New(store, eng)
	pkiSvc := pki.New(store, eng)
	dnsSvc := dns.New(store, eng)
	dropSvc := drop.New(eng)
	callsSvc := calls.New(eng)
	tunnelSvc := tunnel.New(store, eng)
	campSvc := camp.New(eng)

	srv := web.New(eng, fwSvc, pkiSvc, dnsSvc, dropSvc, callsSvc, tunnelSvc, campSvc, *bind)
	// engine → web bridge: when the tunnel comes up, expose a tiny
	// inbox listener on the tunnel_ip so the remote peer can deliver
	// signalling through utun without us binding the UI to 0.0.0.0.
	eng.OnStarted = func(localIP string) {
		if err := srv.BindTunnel(localIP); err != nil {
			log.Printf("WARN: bind tunnel inbox: %v", err)
		}
		if err := srv.BindProxies(localIP); err != nil {
			log.Printf("WARN: bind http proxies: %v", err)
		}
		// Install the inbound utun firewall. Failure here is non-fatal
		// — the tunnel still works, just without input filtering, and
		// the UI flags it via fwSvc.Active() returning false.
		st := eng.Status()
		if err := fwSvc.Start(st.UtunName, localIP, st.CampID); err != nil {
			log.Printf("firewall: %v (input not filtered)", err)
		} else {
			log.Printf("firewall: installed on %s scoped to %s/32", st.UtunName, localIP)
		}
		// Local DNS server + MyDomains catalog + peer-poll. Failure here
		// is non-fatal (HTTPS just resolves manually instead of via the
		// camp's .f2f zone).
		if err := dnsSvc.Start(st.CampID, identity.CampLabel(st.CampID)); err != nil {
			log.Printf("dns: %v (resolver disabled)", err)
		}
		// PKI: ensure local CA + load on-disk peer-CA cache.
		if err := pkiSvc.Start(st.CampID); err != nil {
			log.Printf("pki: %v", err)
		}
		// Tunnel: restore intercepts from camp config + install AWG
		// allowed-ips hook. Must run AFTER engine ready so peers map
		// is populated for HasPeerName checks.
		tunnelSvc.Start(st.CampID)
		// Camp HTTP roster poll. Engine already did the initial UDP
		// announce in Start; this kicks off the periodic peer-list
		// poll that drives engine.peers updates.
		if cc := eng.CampConfigSnapshot(); cc != nil {
			if err := campSvc.Start(*cc); err != nil {
				log.Printf("camp: %v (peer list disabled)", err)
			}
		}
		// BitTorrent client + rescan/restore. Non-fatal: file sharing
		// just becomes unavailable until next Start if this fails.
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("drop: PANIC during startup: %v (file sharing disabled)", r)
				}
			}()
			log.Printf("drop: initialising torrent client …")
			if err := dropSvc.Start(st.CampID, localIP); err != nil {
				log.Printf("drop: %v (file sharing disabled)", err)
			}
		}()
	}
	eng.OnStopped = func() {
		_ = srv.UnbindTunnel()
		_ = srv.UnbindProxies()
		if err := fwSvc.Stop(); err != nil {
			log.Printf("WARN: firewall stop: %v", err)
		}
		if err := dnsSvc.Stop(); err != nil {
			log.Printf("WARN: dns stop: %v", err)
		}
		if err := pkiSvc.Stop(); err != nil {
			log.Printf("WARN: pki stop: %v", err)
		}
		if err := dropSvc.Stop(); err != nil {
			log.Printf("WARN: drop stop: %v", err)
		}
		callsSvc.Reset()
		tunnelSvc.Stop()
		if err := campSvc.Stop(); err != nil {
			log.Printf("WARN: camp stop: %v", err)
		}
	}
	// Tell the engine which TCP port to use when polling peers'
	// /api/domains over the tunnel — same one we host the UI on.
	if _, port, err := net.SplitHostPort(*bind); err == nil {
		eng.SetTunnelHTTPPort(port)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Long-lived service workers. They survive engine restarts —
	// every poll consults engine.TunnelHTTPPort()/OnlinePeers() and
	// no-ops when the engine is down. ctx is the process root, so
	// they exit on ^C / SIGTERM together with the rest of main.
	pkiDone := make(chan struct{})
	go func() {
		defer close(pkiDone)
		pkiSvc.PollPeers(ctx)
	}()
	dnsPollDone := make(chan struct{})
	go func() {
		defer close(dnsPollDone)
		dnsSvc.PollPeers(ctx)
	}()
	dnsHealthDone := make(chan struct{})
	go func() {
		defer close(dnsHealthDone)
		dnsSvc.HealthCheck(ctx)
	}()
	dropPollDone := make(chan struct{})
	go func() {
		defer close(dropPollDone)
		dropSvc.PollPeers(ctx)
	}()
	fwPollDone := make(chan struct{})
	go func() {
		defer close(fwPollDone)
		fwSvc.PollPeers(ctx)
	}()
	tunnelRefreshDone := make(chan struct{})
	go func() {
		defer close(tunnelRefreshDone)
		tunnelSvc.RefreshDomainRoutes(ctx)
	}()
	callsPollDone := make(chan struct{})
	go func() {
		defer close(callsPollDone)
		callsSvc.PollPeers(ctx)
	}()

	go func() {
		log.Printf("UI listening on http://%s", *bind)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// UI listener failure is logged but non-fatal — the
			// engine keeps the tunnel/firewall/peerping up so an
			// in-flight call doesn't drop because the user picked
			// a bad --bind value. They can ^C and retry, or run
			// without UI if autostart did its job.
			log.Printf("UI server error: %v (engine continues; fix --bind and restart)", err)
		}
	}()

	// Auto-start with the last camp_id from $HOME/.f2f/state.json so
	// users don't have to open the browser to bring the tunnel up.
	// Runs in a goroutine — engine.Start blocks for several seconds
	// (utun, UDP, camp announce), and the UI should be reachable
	// during that window so users can watch / cancel. A non-nil error
	// here is non-fatal: leaves the engine stopped, user can start
	// manually from the UI.
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
	// Wait for service workers to drain before exit. ctx is already
	// done, so each select sees it on next pass.
	for _, done := range []chan struct{}{pkiDone, dnsPollDone, dnsHealthDone, dropPollDone, callsPollDone, fwPollDone, tunnelRefreshDone} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			log.Printf("WARN: service worker did not exit in 2s")
		}
	}
	return nil
}
