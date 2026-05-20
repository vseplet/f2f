//go:build darwin

// Package dns runs a minimal DNS server on 127.0.0.1 that answers
// <name>.<camp_id>.f2f queries from a peer-domain catalog. The macOS
// system resolver routes these queries here via /etc/resolver/<camp_id>.f2f.
//
// We only handle A records — every peer's published name resolves to
// its tunnel_ip on the camp's /24. Anything else returns SERVFAIL.
package dns

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// Resolver is implemented by engine.Engine — exposed as an interface so
// this package doesn't depend on the engine package.
type Resolver interface {
	PeerDomains() map[string][]DomainEntry
}

// DomainEntry mirrors engine.DomainEntry so the two packages don't share
// a struct definition. The DNS server only needs the Name field.
type DomainEntry struct {
	Name  string
	Port  int
	Proto string
}

// Server is the live DNS listener. Created by Open, stopped via Close.
type Server struct {
	srv    *dns.Server
	suffix string // ".<camp_id>.f2f." — lowercase, trailing dot
	res    Resolver
}

// Open binds to bindAddr (typically "127.0.0.1:5353") and starts
// answering. campID is the camp identifier used as the second-level
// label under .f2f.
func Open(bindAddr, campID string, res Resolver) (*Server, error) {
	suffix := "." + strings.ToLower(campID) + ".f2f."
	s := &Server{
		srv:    &dns.Server{Net: "udp", Addr: bindAddr},
		suffix: suffix,
		res:    res,
	}
	mux := dns.NewServeMux()
	mux.HandleFunc(strings.TrimPrefix(suffix, "."), s.handle)
	s.srv.Handler = mux

	started := make(chan error, 1)
	s.srv.NotifyStartedFunc = func() { started <- nil }
	go func() {
		if err := s.srv.ListenAndServe(); err != nil {
			// Quietly ignore the "use of closed network connection" we
			// get on graceful shutdown.
			if !strings.Contains(err.Error(), "use of closed") {
				log.Printf("dns: serve: %v", err)
			}
		}
	}()
	// Wait briefly for the socket to bind so callers can rely on the
	// resolver being ready when /etc/resolver gets dropped.
	select {
	case <-started:
		return s, nil
	case <-time.After(2 * time.Second):
		_ = s.srv.Shutdown()
		return nil, fmt.Errorf("dns: bind %s timed out", bindAddr)
	}
}

// Close shuts down the listener.
func (s *Server) Close() error {
	if s == nil || s.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.srv.ShutdownContext(ctx)
}

// handle answers A queries for *.suffix. Anything else returns SERVFAIL.
func (s *Server) handle(w dns.ResponseWriter, req *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(req)
	m.Authoritative = true
	if len(req.Question) == 0 {
		m.Rcode = dns.RcodeServerFailure
		_ = w.WriteMsg(m)
		return
	}
	q := req.Question[0]
	if q.Qclass != dns.ClassINET {
		m.Rcode = dns.RcodeServerFailure
		_ = w.WriteMsg(m)
		return
	}
	name := strings.ToLower(q.Name)
	if !strings.HasSuffix(name, s.suffix) {
		m.Rcode = dns.RcodeRefused
		_ = w.WriteMsg(m)
		return
	}
	label := strings.TrimSuffix(name, s.suffix)
	if label == "" || strings.Contains(label, ".") {
		// Multi-level under the TLD aren't supported.
		m.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(m)
		return
	}

	ip := s.lookup(label)
	if ip == "" {
		m.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(m)
		return
	}
	if q.Qtype == dns.TypeA || q.Qtype == dns.TypeANY {
		addr := net.ParseIP(ip).To4()
		if addr != nil {
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
				A:   addr,
			})
		}
	}
	// For AAAA queries we deliberately respond NOERROR with empty answer
	// (no v6 in our overlay). Apps then fall back to A.
	_ = w.WriteMsg(m)
}

// lookup scans the peer-domains catalog for the matching label and
// returns the owning peer's tunnel_ip. Case-insensitive.
func (s *Server) lookup(label string) string {
	all := s.res.PeerDomains()
	for tunnelIP, entries := range all {
		for _, e := range entries {
			if strings.EqualFold(e.Name, label) {
				return tunnelIP
			}
		}
	}
	return ""
}
