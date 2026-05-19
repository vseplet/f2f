//go:build darwin

// f2f-mac is the macOS-side CLI for the f2f UDP tunnel.
//
// Subcommand `run` is the thin CLI wrapper around internal/engine. The
// engine handles utun, UDP, routes, and (optionally) egress NAT setup; the
// CLI just parses flags and orchestrates start/signal/stop.
//
// Needs sudo (utun + routing + pf).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vseplet/f2f/source/mac/internal/engine"
	"github.com/vseplet/f2f/source/mac/internal/web"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		if err := runCmd(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "ui":
		if err := uiCmd(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `f2f-mac — macOS-side traffic interceptor

Usage:
  sudo f2f-mac run [--listen :PORT --peer HOST:PORT]
                   [--local-ip 10.99.0.1] [--peer-ip 10.99.0.2]
                   [--egress-iface en0]
                   [--camp-url wss://… --name X --id Y]
  sudo f2f-mac ui  [--bind 127.0.0.1:8080]

Intercepts (domains/IPs to route through a specific peer) are managed
exclusively via the web UI — each entry must be bound to a peer at
creation time.

Rendezvous (Camp) mode:
  Instead of supplying --peer, point at a camp server: each peer discovers
  its external UDP endpoint via STUN, registers under (--name, --id),
  and the engine adopts the other peer in the same camp automatically.

  # both sides:
  sudo f2f-mac run --listen :9000 \
                   --camp-url wss://f2f-camp.fly.dev/ws \
                   --name vasya --id beer

  ui              Start the local web UI. Configure and operate the engine
                  from a browser. Same engine as 'run', just driven over HTTP.

  --listen        UDP address to receive from peer (e.g. :9000).
  --peer          UDP address of the remote peer (e.g. 10.0.0.5:9000).
                  Auto-updates when traffic arrives from elsewhere.
  --egress-iface  physical interface to NAT tunnel traffic out of (e.g. en0).
                  Empty = auto-detect default route. Egress (pf NAT +
                  ip.forwarding=1) is always on; this flag is just an
                  override for multi-homed boxes.

Example (two-machine camp setup — both sides are symmetric):
  sudo f2f-mac run --listen :9000 \
                   --camp-url wss://f2f-camp.fly.dev/ws \
                   --name vasya --id beer

Manual rescue (if f2f-mac was kill -9'd and left state behind):
  sudo pfctl -a com.apple/f2f-mac -F all
  sudo sysctl -w net.inet.ip.forwarding=0   # only if it was 0 before f2f-mac
  sudo rm -f /var/run/f2f-mac.egress.json
`)
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
	// Route global log output through the engine's tap as well, so the UI
	// (and any future subscriber) sees the same lines that go to stderr.
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
	bind := fs.String("bind", "127.0.0.1:8080", "HTTP bind address; 127.0.0.1 by default to keep the UI local")
	if err := fs.Parse(args); err != nil {
		return err
	}

	eng := engine.New()
	log.SetOutput(io.MultiWriter(os.Stderr, eng.LogTap()))

	srv := web.New(eng, *bind)

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

