//go:build darwin

// f2f-mac is a CLI tool that opens a utun interface on macOS and routes
// the IPs/domains given on the command line into it. At this milestone the
// program only logs the intercepted packets — there is no peer or upstream
// yet. Run it as root (sudo). Ctrl+C tears the interface down cleanly.
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
  sudo f2f-mac run --intercept <list> [--local-ip 10.99.0.1] [--peer-ip 10.99.0.2]

  <list> is a comma-separated set of IPs, CIDR blocks, and domain names.
  Domains are resolved once at startup and the resulting addresses are
  installed as host routes through the utun interface.

Example:
  sudo f2f-mac run --intercept 1.1.1.1,example.com

`)
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	intercept := fs.String("intercept", "", "comma-separated list of IPs, CIDRs, and domains to route into the tunnel")
	localIP := fs.String("local-ip", "10.99.0.1", "local end of the point-to-point address on utun")
	peerIP := fs.String("peer-ip", "10.99.0.2", "remote end of the point-to-point address on utun")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*intercept) == "" {
		return errors.New("--intercept is required")
	}
	prefixes, err := parseIntercept(*intercept)
	if err != nil {
		return err
	}
	if len(prefixes) == 0 {
		return errors.New("no IPs to intercept after parsing --intercept")
	}

	tun, err := tunnel.Open(*localIP, *peerIP)
	if err != nil {
		return fmt.Errorf("open tunnel: %w", err)
	}
	log.Printf("opened %s (local=%s peer=%s mtu=%d)", tun.Name(), *localIP, *peerIP, tunnel.MTU)

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

	readErr := make(chan error, 1)
	go func() {
		for {
			pkt, err := tun.Read()
			if err != nil {
				readErr <- err
				return
			}
			if len(pkt) == 0 {
				continue
			}
			log.Printf("[%s] %s", tun.Name(), packet.Summary(pkt))
		}
	}()

	select {
	case <-ctx.Done():
		log.Println("shutting down…")
	case err := <-readErr:
		log.Printf("tun read stopped: %v", err)
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
