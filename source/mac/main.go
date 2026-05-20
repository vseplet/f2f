//go:build darwin

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

	"github.com/vseplet/f2f/source/mac/internal/engine"
	"github.com/vseplet/f2f/source/mac/internal/web"
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

	eng := engine.New()
	log.SetOutput(io.MultiWriter(os.Stderr, eng.LogTap()))

	cfg := engine.Config{
		LocalIP:     *localIP,
		PeerIP:      *peerIP,
		Listen:      *listen,
		Peer:        *peerAddr,
		EgressIface: *egressIface,
	}
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
	if err := fs.Parse(args); err != nil {
		return err
	}

	eng := engine.New()
	log.SetOutput(io.MultiWriter(os.Stderr, eng.LogTap()))

	srv := web.New(eng, *bind)
	// engine → web bridge: when the tunnel comes up, expose a tiny
	// inbox listener on the tunnel_ip so the remote peer can deliver
	// signalling through utun without us binding the UI to 0.0.0.0.
	eng.OnStarted = func(localIP string) {
		if err := srv.BindTunnel(localIP); err != nil {
			log.Printf("WARN: bind tunnel inbox: %v", err)
		}
	}
	eng.OnStopped = func() {
		_ = srv.UnbindTunnel()
	}
	// Tell the engine which TCP port to use when polling peers'
	// /api/domains over the tunnel — same one we host the UI on.
	if _, port, err := net.SplitHostPort(*bind); err == nil {
		eng.SetTunnelHTTPPort(port)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("UI listening on http://%s", *bind)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Println("shutting down…")
	case err := <-serverErr:
		log.Printf("HTTP server error: %v", err)
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	if err := eng.Stop(); err != nil {
		log.Printf("WARN: engine stop: %v", err)
	}
	return nil
}
