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
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"github.com/vseplet/f2f/source/helper/clog"
)

// Resolver is implemented by engine.Engine — exposed as an interface so
// this package doesn't depend on the engine package.
type Resolver interface {
	// LookupHost resolves label (the part before <camp>.f2f) to an A
	// and/or AAAA address. Empty v4 → omit the A; empty v6 → omit the
	// AAAA. Returns ok=false on NXDOMAIN.
	LookupHost(label string) (host Host, ok bool)
}

// Host is the address pair (any of v4/v6 may be empty) that a label
// resolves to. v4 only present for self-published names (loopback) so
// legacy v4-only apps still reach our own services; peers are reached
// over overlay v6 exclusively.
type Host struct {
	V4 string
	V6 string
}

// Server is the live DNS listener. Created by Open, stopped via Close.
type Server struct {
	srv    *dns.Server
	addr   string // actual bound "ip:port" — useful when Open was given :0
	suffix string // ".<camp_id>.f2f." — lowercase, trailing dot
	res    Resolver

	// pinnedFn answers names OUTSIDE the camp zone — intercept
	// domains pinned to exit-peer-resolved IPs. Queries for them only
	// arrive here via per-domain OS resolver entries. nil = no pins.
	pinnedFn func(name string) []string

	// pinnedMissFn is consulted when pinnedFn has no answer: the name may
	// be a not-yet-seen subdomain of an active intercept zone (the OS
	// resolver scopes the whole zone to us, so www.myip.com lands here
	// even though only myip.com was pinned). The hook resolves it on the
	// exit peer, installs routes, and returns the fresh A records. nil
	// disables on-demand subdomains.
	pinnedMissFn func(name string) []string

	// Per-rcode query counters and last-query timestamp, for the UI's
	// diagnostics tab. Atomic — read concurrently with handle().
	totalQueries atomic.Int64
	noerrCount   atomic.Int64
	nxdomCount   atomic.Int64
	refusedCount atomic.Int64
	lastQueryMs  atomic.Int64
}

// Stats is a snapshot of DNS-server activity.
type Stats struct {
	Total       int64 `json:"total"`
	NoError     int64 `json:"noerror"`
	NXDomain    int64 `json:"nxdomain"`
	Refused     int64 `json:"refused"`
	LastQueryMs int64 `json:"last_query_ms"`
}

// Stats returns a snapshot of query counters. Safe for concurrent use.
func (s *Server) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	return Stats{
		Total:       s.totalQueries.Load(),
		NoError:     s.noerrCount.Load(),
		NXDomain:    s.nxdomCount.Load(),
		Refused:     s.refusedCount.Load(),
		LastQueryMs: s.lastQueryMs.Load(),
	}
}

// Open binds to bindAddr and starts answering. zone is the second-
// level label under .f2f — typically identity.CampLabel(camp_id),
// kept short enough to fit a DNS label.
//
// Pass "127.0.0.1:0" to let the kernel pick a free port; the actual
// bound address is then available via Server.Addr.
func Open(bindAddr, zone string, res Resolver, pinned, pinnedMiss func(name string) []string) (*Server, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", bindAddr)
	if err != nil {
		return nil, fmt.Errorf("dns: resolve %s: %w", bindAddr, err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("dns: listen %s: %w", bindAddr, err)
	}
	suffix := "." + strings.ToLower(zone) + ".f2f."
	s := &Server{
		srv:          &dns.Server{PacketConn: conn},
		addr:         conn.LocalAddr().String(),
		suffix:       suffix,
		res:          res,
		pinnedFn:     pinned,
		pinnedMissFn: pinnedMiss,
	}
	mux := dns.NewServeMux()
	mux.HandleFunc(strings.TrimPrefix(suffix, "."), s.handle)
	// Root pattern catches everything outside the camp zone — pinned
	// intercept domains routed here by per-domain resolver entries.
	// The mux picks the longest matching suffix, so the camp-zone
	// handler above still wins for its names.
	mux.HandleFunc(".", s.handlePinned)
	s.srv.Handler = mux

	started := make(chan error, 1)
	s.srv.NotifyStartedFunc = func() { started <- nil }
	go func() {
		if err := s.srv.ActivateAndServe(); err != nil {
			// Quietly ignore the "use of closed network connection" we
			// get on graceful shutdown.
			if !strings.Contains(err.Error(), "use of closed") {
				clog.Warn("dns", "serve: %v", err)
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
		return nil, fmt.Errorf("dns: activate %s timed out", s.addr)
	}
}

// Addr returns the actual bound address, including the
// kernel-assigned port when Open was called with ":0".
func (s *Server) Addr() string { return s.addr }

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
	s.totalQueries.Add(1)
	s.lastQueryMs.Store(time.Now().UnixMilli())
	defer func() {
		switch m.Rcode {
		case dns.RcodeSuccess:
			s.noerrCount.Add(1)
		case dns.RcodeNameError:
			s.nxdomCount.Add(1)
		case dns.RcodeRefused:
			s.refusedCount.Add(1)
		}
	}()
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
	if label == "" {
		m.Rcode = dns.RcodeNameError
		s.attachSOA(m)
		_ = w.WriteMsg(m)
		return
	}
	// Nested labels (gitea.mini) and wildcard subzones (*.mini) are
	// resolved via the Resolver — it walks MyDomains and peer domains
	// for both exact and wildcard matches.

	host, ok := s.res.LookupHost(label)
	if !ok {
		m.Rcode = dns.RcodeNameError
		s.attachSOA(m)
		_ = w.WriteMsg(m)
		return
	}
	if (q.Qtype == dns.TypeA || q.Qtype == dns.TypeANY) && host.V4 != "" {
		if addr := net.ParseIP(host.V4).To4(); addr != nil {
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
				A:   addr,
			})
		}
	}
	if (q.Qtype == dns.TypeAAAA || q.Qtype == dns.TypeANY) && host.V6 != "" {
		if addr6 := net.ParseIP(host.V6).To16(); addr6 != nil {
			m.Answer = append(m.Answer, &dns.AAAA{
				Hdr:  dns.RR_Header{Name: q.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 30},
				AAAA: addr6,
			})
		}
	}
	// Empty answer (qtype the host doesn't satisfy, or anything other
	// than A/AAAA/ANY) is a NOERROR negative response; attach SOA so
	// RFC 2308 negative cache stays at one second.
	if len(m.Answer) == 0 {
		s.attachSOA(m)
	}
	_ = w.WriteMsg(m)
}

// handlePinned answers queries for names outside the camp zone:
// intercept domains pinned to exit-peer-resolved A records. AAAA gets
// an empty NOERROR so apps settle on v4 immediately (the v4 routes
// are what the intercept covers). Unknown names → NXDOMAIN.
func (s *Server) handlePinned(w dns.ResponseWriter, req *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(req)
	m.Authoritative = true
	s.totalQueries.Add(1)
	s.lastQueryMs.Store(time.Now().UnixMilli())
	defer func() {
		switch m.Rcode {
		case dns.RcodeSuccess:
			s.noerrCount.Add(1)
		case dns.RcodeNameError:
			s.nxdomCount.Add(1)
		case dns.RcodeRefused:
			s.refusedCount.Add(1)
		}
	}()
	if len(req.Question) == 0 || req.Question[0].Qclass != dns.ClassINET {
		m.Rcode = dns.RcodeServerFailure
		_ = w.WriteMsg(m)
		return
	}
	q := req.Question[0]
	name := strings.ToLower(strings.TrimSuffix(q.Name, "."))
	var ips []string
	if s.pinnedFn != nil {
		ips = s.pinnedFn(name)
	}
	if len(ips) == 0 && s.pinnedMissFn != nil {
		// Not pinned, but possibly a subdomain of an active intercept
		// zone (www.myip.com under a myip.com intercept). Resolve it on
		// the exit peer and route it now; subsequent queries hit the
		// pin directly. Synchronous — the first lookup waits on the bus
		// round-trip, the OS resolver retries cover a cold miss.
		ips = s.pinnedMissFn(name)
	}
	if len(ips) == 0 {
		m.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(m)
		return
	}
	if q.Qtype == dns.TypeA || q.Qtype == dns.TypeANY {
		for _, ip := range ips {
			if addr := net.ParseIP(ip).To4(); addr != nil {
				m.Answer = append(m.Answer, &dns.A{
					Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
					A:   addr,
				})
			}
		}
	}
	_ = w.WriteMsg(m)
}

// attachSOA appends a minimal SOA RR with Minimum=1 to the authority
// section so RFC 2308 negative caching is capped at one second. The
// values don't need to be real — no one polls this zone over DNS, the
// data comes from the camp HTTP API. We just need MinTTL low so
// macOS's mDNSResponder cache doesn't pin a stale NXDOMAIN.
func (s *Server) attachSOA(m *dns.Msg) {
	zone := strings.TrimPrefix(s.suffix, ".")
	m.Ns = append(m.Ns, &dns.SOA{
		Hdr:     dns.RR_Header{Name: zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 1},
		Ns:      "ns." + zone,
		Mbox:    "hostmaster." + zone,
		Serial:  1,
		Refresh: 60,
		Retry:   30,
		Expire:  86400,
		Minttl:  1,
	})
}

// lookup scans the peer-domains catalog for the matching label and
// (Resolver.LookupHost replaced the old PeerDomains map walk.)
