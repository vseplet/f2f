// Package proxy serves this peer's published *.f2f domains to browsers
// over the standard web ports (:80 / :443) on both loopback and the
// tunnel IP. HTTPS is terminated with leaf certs issued on demand by
// the local CA; requests are reverse-proxied to the local upstream
// resolved from the published-domains list.
//
// Lifecycle: Start on engine OnStarted (binds the listeners), Stop on
// OnStopped. The whole service is rebuilt on a camp switch, so the
// camp_id is a Start-time snapshot rather than a live engine read —
// matching how firewall/pki receive it from the caller.
package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vseplet/f2f/source/helper/clog"
	"github.com/vseplet/f2f/source/helper/identity"
	"github.com/vseplet/f2f/source/helper/services/dns"
	"github.com/vseplet/f2f/source/helper/services/pki"
)

// attestHeader carries the AmneziaWG-attested pub of the calling peer to
// backends. Kept in sync with services/oidc.attestHeader — the OIDC
// provider reads it to identify the visitor. The proxy strips any inbound
// copy and re-sets it from the connection, so a backend can trust it.
const attestHeader = "X-F2F-Peer"

// oidcPrefix is the path under which the built-in OIDC provider is served
// on every domain this peer hosts. The issuer becomes <app-host>/oidc.
const oidcPrefix = "/oidc"

// Service owns the :80/:443 reverse-proxy listeners. Constructed once;
// Start/Stop run on the engine lifecycle.
type Service struct {
	dns *dns.Service
	pki *pki.Service
	// oidcPort is the loopback port the built-in OIDC provider listens on
	// (0 disables the built-in id.<zone>.f2f route).
	oidcPort int
	// peerByOverlayIP resolves a tunnel client IP to its pub for the
	// attested-identity header. nil ⇒ no attestation injected.
	peerByOverlayIP func(ip string) string

	mu   sync.Mutex
	srvs []*http.Server
	// campID is the running camp's id, snapshotted at Start. Read by
	// handleProxy to derive the .f2f zone suffix.
	campID atomic.Pointer[string]
}

// New constructs the proxy service. dns supplies the published-domain
// lookup; pki supplies the leaf-cert issuer for HTTPS termination.
// oidcPort/peerByOverlayIP wire the built-in OIDC provider route (see
// attestHeader); pass 0/nil to disable it.
func New(dnsSvc *dns.Service, pkiSvc *pki.Service, oidcPort int, peerByOverlayIP func(ip string) string) *Service {
	return &Service{dns: dnsSvc, pki: pkiSvc, oidcPort: oidcPort, peerByOverlayIP: peerByOverlayIP}
}

// Start brings up the reverse-proxy listeners for camp campID, bound on
// 127.0.0.1 and (if set) tunnelIP. HTTPS is enabled only when a CA is
// available. Bind failures are logged, not fatal. Idempotent.
func (s *Service) Start(tunnelIP, campID string) error {
	_ = s.Stop()
	id := campID
	s.campID.Store(&id)

	httpAddrs := []string{net.JoinHostPort("127.0.0.1", "80")}
	if tunnelIP != "" {
		httpAddrs = append(httpAddrs, net.JoinHostPort(tunnelIP, "80"))
	}
	for _, a := range httpAddrs {
		s.startListener(a, nil)
	}

	if ca := s.pki.MyCA(); ca != nil {
		tlsCfg := &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				host := strings.ToLower(strings.TrimSpace(hello.ServerName))
				if host == "" {
					return nil, fmt.Errorf("tls: empty SNI")
				}
				return ca.IssueLeaf(host)
			},
		}
		tlsAddrs := []string{net.JoinHostPort("127.0.0.1", "443")}
		if tunnelIP != "" {
			tlsAddrs = append(tlsAddrs, net.JoinHostPort(tunnelIP, "443"))
		}
		for _, a := range tlsAddrs {
			s.startListener(a, tlsCfg)
		}
	}
	return nil
}

// startListener brings up one listener (HTTP if tlsCfg is nil, HTTPS
// otherwise) and stashes it for shutdown.
func (s *Service) startListener(addr string, tlsCfg *tls.Config) {
	// Loopback listeners also serve local-only routes (the built-in
	// portal → web UI); tunnel-facing listeners must not, so a peer can
	// never reach our UI through the overlay.
	loopback := strings.HasPrefix(addr, "127.0.0.1")
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.proxyHandler(loopback),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsCfg,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		scheme := "HTTP"
		if tlsCfg != nil {
			scheme = "HTTPS"
		}
		clog.Info("proxy", "bind %s %s: %v (skipping)", scheme, addr, err)
		return
	}
	if tlsCfg != nil {
		ln = tls.NewListener(ln, tlsCfg)
	}
	s.mu.Lock()
	s.srvs = append(s.srvs, srv)
	s.mu.Unlock()
	go func() {
		scheme := "HTTP"
		if tlsCfg != nil {
			scheme = "HTTPS"
		}
		clog.Info("proxy", "%s listening on %s", scheme, addr)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			clog.Warn("proxy", "proxy %s: %v", addr, err)
		}
	}()
}

// Stop shuts down every active listener. Idempotent.
func (s *Service) Stop() error {
	s.mu.Lock()
	srvs := s.srvs
	s.srvs = nil
	s.mu.Unlock()
	for _, srv := range srvs {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
		clog.Info("proxy", "stopped %s", srv.Addr)
	}
	return nil
}

// proxyHandler returns the reverse-proxy handler for one listener. It
// maps the Host header's label within our <label>.f2f zone to a
// published domain and forwards to its upstream (defaulting to
// 127.0.0.1). Anything outside our zone or without a matching label is
// a 404. loopback listeners additionally serve local-only routes (the
// built-in portal); tunnel-facing listeners only see MyDomains.
func (s *Service) proxyHandler(loopback bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.handleProxy(loopback, w, r)
	}
}

func (s *Service) handleProxy(loopback bool, w http.ResponseWriter, r *http.Request) {
	campID := ""
	if p := s.campID.Load(); p != nil {
		campID = strings.ToLower(strings.TrimSpace(*p))
	}
	if campID == "" {
		http.Error(w, "engine not in a camp", http.StatusServiceUnavailable)
		return
	}
	zone := identity.CampLabel(campID)
	suffix := "." + zone + ".f2f"

	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil && h != "" {
		host = h
	}
	host = strings.ToLower(host)
	if !strings.HasSuffix(host, suffix) {
		http.Error(w, "not in this camp's f2f zone", http.StatusNotFound)
		return
	}
	label := strings.TrimSuffix(host, suffix)
	if label == "" {
		http.Error(w, "bad subdomain", http.StatusNotFound)
		return
	}

	// Attested identity of the caller: resolve the tunnel client IP to a
	// pub. We always strip any inbound X-F2F-Peer (a backend must never
	// trust a client-supplied value) and re-set it from the connection.
	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	attestedPub := ""
	if s.peerByOverlayIP != nil {
		attestedPub = s.peerByOverlayIP(clientIP)
	}

	// Dedicated per-peer OIDC issuer host (auth-<fp>.<zone>.f2f): serve
	// the provider at the host root on both loopback and tunnel. This is
	// the canonical issuer shown in the admin UI.
	if s.oidcPort != 0 && label == s.dns.OIDCLabel() && s.dns.OIDCLabel() != "" {
		s.forward(w, r, "127.0.0.1", s.oidcPort, host, attestedPub, "")
		return
	}

	// Built-in OIDC provider, also co-located under the /oidc prefix on
	// ANY domain we host: the app's domain already routes here over the
	// tunnel, so a visiting peer's browser reaches our IdP at e.g.
	// https://grafana.<zone>.f2f/oidc. We strip the prefix before
	// forwarding and tell the backend the external base via
	// X-Forwarded-Prefix so it builds the right issuer.
	if s.oidcPort != 0 && (r.URL.Path == oidcPrefix || strings.HasPrefix(r.URL.Path, oidcPrefix+"/")) {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, oidcPrefix)
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		s.forward(w, r, "127.0.0.1", s.oidcPort, host, attestedPub, oidcPrefix)
		return
	}

	// Two-pass match against our domains: exact wins over wildcard.
	// Loopback also matches local-only routes (portal → web UI).
	var (
		port   int
		upHost string
	)
	mine := s.dns.MyDomains()
	if loopback {
		mine = s.dns.LocalRoutes()
	}
	for _, d := range mine {
		if !dns.IsWildcardLabel(d.Name) && strings.EqualFold(d.Name, label) {
			port = d.Port
			upHost = d.Host
			break
		}
	}
	if port == 0 {
		for _, d := range mine {
			if dns.IsWildcardLabel(d.Name) && dns.MatchesWildcard(d.Name, label) {
				port = d.Port
				upHost = d.Host
				break
			}
		}
	}
	if upHost == "" {
		upHost = "127.0.0.1"
	}
	if port == 0 {
		http.Error(w, "no such domain published locally", http.StatusNotFound)
		return
	}

	s.forward(w, r, upHost, port, host, attestedPub, "")
}

// forward reverse-proxies r to upHost:port, setting the standard
// X-Forwarded-* headers and the attested X-F2F-Peer (always stripping any
// client-supplied copy first). host is the original Host (for logs);
// prefix, when non-empty, is the external path prefix stripped before
// forwarding (sent on as X-Forwarded-Prefix).
func (s *Service) forward(w http.ResponseWriter, r *http.Request, upHost string, port int, host, attestedPub, prefix string) {
	target, _ := url.Parse("http://" + net.JoinHostPort(upHost, strconv.Itoa(port)))
	proxy := httputil.NewSingleHostReverseProxy(target)

	// Tell the backend the request reached us over HTTPS (we terminate
	// TLS and forward plain HTTP). Without this, apps like Gitea
	// generate http:// links. r.TLS is non-nil only on the :443 listener.
	fwdProto := "http"
	if r.TLS != nil {
		fwdProto = "https"
	}
	fwdHost := r.Host
	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.Header.Set("X-Forwarded-Proto", fwdProto)
		req.Header.Set("X-Forwarded-Host", fwdHost)
		if clientIP != "" {
			req.Header.Set("X-Forwarded-For", clientIP)
		}
		// Anti-spoof: never trust an inbound value; set only what the
		// overlay attests.
		req.Header.Del(attestHeader)
		if attestedPub != "" {
			req.Header.Set(attestHeader, attestedPub)
		}
		if prefix != "" {
			req.Header.Set("X-Forwarded-Prefix", prefix)
		}
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		clog.Warn("proxy", "%s → %s: %v", host, target, err)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}
