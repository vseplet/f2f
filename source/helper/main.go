// f2f-mac is the macOS-side CLI for the f2f UDP tunnel. Launches the
// web UI on 127.0.0.1:2202 (overridable via --bind). Needs sudo for
// utun + routing + pf.
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

	bind := flag.String("bind", defaultBind, "HTTP bind address for the loopback UI")
	flag.Parse()

	if err := run(*bind); err != nil {
		log.Fatal(err)
	}
}

func run(bind string) error {
	store, err := config.NewStore()
	if err != nil {
		return fmt.Errorf("config store: %w", err)
	}

	eng := engine.New(store)
	eng.SetDefaultListen(":0") // ephemeral; camp learns reflex after NAT
	log.SetOutput(io.MultiWriter(os.Stderr, eng.LogTap()))

	fwSvc := firewall.New(store, eng)
	pkiSvc := pki.New(store, eng)
	dnsSvc := dns.New(store, eng)
	dropSvc := drop.New(eng)
	callsSvc := calls.New(eng)
	tunnelSvc := tunnel.New(store, eng)
	campSvc := camp.New(eng)

	srv := web.New(eng, fwSvc, pkiSvc, dnsSvc, dropSvc, callsSvc, tunnelSvc, campSvc, bind)

	eng.OnStarted = func(localIP string) {
		if err := srv.BindTunnel(localIP); err != nil {
			log.Printf("WARN: bind tunnel inbox: %v", err)
		}
		if err := srv.BindProxies(localIP); err != nil {
			log.Printf("WARN: bind http proxies: %v", err)
		}
		st := eng.Status()
		if err := fwSvc.Start(st.UtunName, localIP, st.CampID); err != nil {
			log.Printf("firewall: %v (input not filtered)", err)
		} else {
			log.Printf("firewall: installed on %s scoped to %s/32", st.UtunName, localIP)
		}
		if err := dnsSvc.Start(st.CampID, identity.CampLabel(st.CampID)); err != nil {
			log.Printf("dns: %v (resolver disabled)", err)
		}
		if err := pkiSvc.Start(st.CampID); err != nil {
			log.Printf("pki: %v", err)
		}
		tunnelSvc.Start(st.CampID)
		if cc := eng.CampConfigSnapshot(); cc != nil {
			if err := campSvc.Start(*cc); err != nil {
				log.Printf("camp: %v (peer list disabled)", err)
			}
		}
		// Drop's anacrolix client can take a moment to bind and has
		// been known to panic during init; isolate it in a goroutine.
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
		if err := campSvc.Stop(); err != nil {
			log.Printf("WARN: camp stop: %v", err)
		}
		tunnelSvc.Stop()
		callsSvc.Reset()
		if err := dropSvc.Stop(); err != nil {
			log.Printf("WARN: drop stop: %v", err)
		}
		if err := pkiSvc.Stop(); err != nil {
			log.Printf("WARN: pki stop: %v", err)
		}
		if err := dnsSvc.Stop(); err != nil {
			log.Printf("WARN: dns stop: %v", err)
		}
		if err := fwSvc.Stop(); err != nil {
			log.Printf("WARN: firewall stop: %v", err)
		}
	}
	if _, port, err := net.SplitHostPort(bind); err == nil {
		eng.SetTunnelHTTPPort(port)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Long-lived service workers. They survive engine restarts (each
	// poll consults engine state and no-ops when down) and exit on
	// ^C / SIGTERM along with the rest of the process.
	workers := []func(context.Context){
		pkiSvc.PollPeers,
		dnsSvc.PollPeers,
		dnsSvc.HealthCheck,
		dropSvc.PollPeers,
		fwSvc.PollPeers,
		tunnelSvc.RefreshDomainRoutes,
		callsSvc.PollPeers,
	}
	done := make([]chan struct{}, len(workers))
	for i, w := range workers {
		done[i] = make(chan struct{})
		go func(fn func(context.Context), d chan struct{}) {
			defer close(d)
			fn(ctx)
		}(w, done[i])
	}

	go func() {
		log.Printf("UI listening on http://%s", bind)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("UI server error: %v (engine continues; fix --bind and restart)", err)
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
	for _, d := range done {
		select {
		case <-d:
		case <-time.After(2 * time.Second):
			log.Printf("WARN: service worker did not exit in 2s")
		}
	}
	return nil
}
