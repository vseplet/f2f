//go:build darwin

// f2f-mac is the macOS-side CLI for the f2f UDP tunnel.
//
// The `run` subcommand opens a utun interface, optionally installs host
// routes for --intercept, and (when --listen + --peer are set) shuttles
// packets between utun and a UDP peer. The exact same binary plays both
// ends of the tunnel; the only difference is which flags you pass.
//
// run needs sudo (utun + routing).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/vseplet/f2f/source/mac/internal/packet"
	"github.com/vseplet/f2f/source/mac/internal/route"
	"github.com/vseplet/f2f/source/mac/internal/tunnel"
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
  sudo f2f-mac run [--intercept LIST] [--listen :PORT --peer HOST:PORT]
                   [--local-ip 10.99.0.1] [--peer-ip 10.99.0.2]

  --intercept   comma-separated IPs/CIDRs/domains routed into utun.
                Optional — omit on the egress side.
  --listen      UDP address to receive from peer (e.g. :9000).
  --peer        UDP address of the remote peer (e.g. 10.0.0.5:9000).

Example (symmetric two-instance setup):
  # A (ingress side, routes some IPs into the tunnel):
  sudo f2f-mac run --intercept 198.51.100.5 \
                   --local-ip 10.99.0.1 --peer-ip 10.99.0.2 \
                   --listen :9000 --peer 10.0.0.5:9000

  # B (egress side, no intercept yet — just a tunnel endpoint):
  sudo f2f-mac run --local-ip 10.99.0.2 --peer-ip 10.99.0.1 \
                   --listen :9000 --peer A_PUBLIC_IP:9000
`)
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	intercept := fs.String("intercept", "", "comma-separated list of IPs, CIDRs, and domains to route into the tunnel")
	localIP := fs.String("local-ip", "10.99.0.1", "local end of the point-to-point address on utun")
	peerIP := fs.String("peer-ip", "10.99.0.2", "remote end of the point-to-point address on utun")
	listen := fs.String("listen", "", "UDP address to listen on (e.g. :9000); pair with --peer to enable peer mode")
	peerAddr := fs.String("peer", "", "UDP address of the remote peer (e.g. 127.0.0.1:9001); pair with --listen to enable peer mode")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*listen == "") != (*peerAddr == "") {
		return errors.New("--listen and --peer must both be set or both be empty")
	}
	prefixes, err := parseIntercept(*intercept)
	if err != nil {
		return err
	}

	peerMode := *listen != ""
	var udpConn *net.UDPConn
	var peer *net.UDPAddr
	if peerMode {
		laddr, err := net.ResolveUDPAddr("udp", *listen)
		if err != nil {
			return fmt.Errorf("resolve --listen: %w", err)
		}
		peer, err = net.ResolveUDPAddr("udp", *peerAddr)
		if err != nil {
			return fmt.Errorf("resolve --peer: %w", err)
		}
		udpConn, err = net.ListenUDP("udp", laddr)
		if err != nil {
			return fmt.Errorf("listen udp: %w", err)
		}
		defer udpConn.Close()
	}

	tun, err := tunnel.Open(*localIP, *peerIP)
	if err != nil {
		return fmt.Errorf("open tunnel: %w", err)
	}
	log.Printf("opened %s (local=%s peer=%s mtu=%d)", tun.Name(), *localIP, *peerIP, tunnel.MTU)
	if peerMode {
		log.Printf("UDP listening on %s, forwarding to peer %s", udpConn.LocalAddr(), peer)
	}

	rm := route.New(tun.Name())
	for _, p := range prefixes {
		if err := rm.Add(p); err != nil {
			log.Printf("WARN: %v", err)
			continue
		}
		log.Printf("route %s → %s", p, tun.Name())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// utun → UDP (or drop, if no peer)
	tunErr := make(chan error, 1)
	go func() {
		for {
			pkt, err := tun.Read()
			if err != nil {
				tunErr <- err
				return
			}
			if len(pkt) == 0 {
				continue
			}
			summary := packet.Summary(pkt)
			action := "drop"
			if peerMode {
				if _, werr := udpConn.WriteToUDP(pkt, peer); werr != nil {
					log.Printf("WARN: udp send: %v", werr)
					action = "→peer-failed"
				} else {
					action = "→peer"
				}
			}
			log.Printf("[%s] %s [%s]", tun.Name(), summary, action)
		}
	}()

	// UDP → utun
	udpErr := make(chan error, 1)
	if peerMode {
		go func() {
			buf := make([]byte, tunnel.MTU)
			for {
				n, from, err := udpConn.ReadFromUDP(buf)
				if err != nil {
					udpErr <- err
					return
				}
				pkt := buf[:n]
				summary := packet.Summary(pkt)
				if werr := tun.Write(pkt); werr != nil {
					log.Printf("WARN: utun write from %s: %v", from, werr)
					log.Printf("[udp %s] %s [→utun-failed]", from, summary)
				} else {
					log.Printf("[udp %s] %s [→utun]", from, summary)
				}
			}
		}()
	}

	select {
	case <-ctx.Done():
		log.Println("shutting down…")
	case err := <-tunErr:
		log.Printf("tun read stopped: %v", err)
	case err := <-udpErr:
		log.Printf("udp read stopped: %v", err)
	}

	if errs := rm.Cleanup(); len(errs) > 0 {
		for _, e := range errs {
			log.Printf("WARN: %v", e)
		}
	}
	if err := tun.Close(); err != nil {
		log.Printf("WARN: tun close: %v", err)
	}
	return nil
}

func parseIntercept(s string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, raw := range strings.Split(s, ",") {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		if p, err := netip.ParsePrefix(item); err == nil {
			prefixes = append(prefixes, p)
			continue
		}
		if a, err := netip.ParseAddr(item); err == nil {
			prefixes = append(prefixes, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		ips, err := net.LookupIP(item)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", item, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("resolve %q: no records", item)
		}
		for _, ip := range ips {
			a, ok := netip.AddrFromSlice(ip)
			if !ok {
				continue
			}
			a = a.Unmap()
			prefixes = append(prefixes, netip.PrefixFrom(a, a.BitLen()))
			log.Printf("resolved %s → %s", item, a)
		}
	}
	return prefixes, nil
}
