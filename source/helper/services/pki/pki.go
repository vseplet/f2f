// Package pki is the user-facing service for everything cert /
// trust-related in a camp:
//
//   - "my CA": the camp-pinned local certificate authority used to
//     issue leaf certs for the reverse-proxy (HTTPS termination on
//     <name>.<camp>.f2f). Loaded from disk or generated fresh on each
//     camp Start, then installed into the system trust store so the
//     OS recognises it as a root.
//   - "peer CAs": every camp peer publishes its own local CA at
//     /api/ca-cert. PollPeers fetches each one, persists it on disk
//     plus records metadata in the camp config; Install adds a
//     discovered CA to the system trust store on explicit user click
//     (because the keychain prompt is intrusive); Remove is the
//     inverse.
//
// Layering:
//
//   - helper/ca: low-level primitives — Generate/Load/Save/IssueLeaf,
//     EnsureSystemTrust. Pure CA logic, no service state.
//   - helper/platform: OS-specific TrustStoreAdd/Remove/Contains for
//     the macOS keychain or Linux trust dir.
//   - this package: lifecycle (Start/Stop), poll loop, in-memory
//     mirror of discovered peer CAs, persistence into camp config.
package pki

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/engine"
	"github.com/vseplet/f2f/source/helper/identity"
	"github.com/vseplet/f2f/source/helper/platform"
)

// myCADir is where the local CA's ca.crt/ca.key live. Persisted
// across runs so leaf certs issued in one session remain valid in
// the next.
const myCADir = "/var/lib/f2f/ca"

// peerCAsRootDir is the on-disk parent for per-camp peer-CA caches.
// Each camp gets its own subdir keyed by camp_id so CAs from camp A
// don't leak into camp B's UI.
const peerCAsRootDir = "/var/lib/f2f/trusted-peers"

// PeerEntry is one row in the UI's trusted-peers panel.
type PeerEntry struct {
	PeerName    string `json:"peer_name"`
	CommonName  string `json:"common_name"`
	Fingerprint string `json:"fingerprint"`
	InstalledAt int64  `json:"installed_at"`
	Installed   bool   `json:"installed"`
}

// Service owns both the local CA and the cache of discovered peer
// CAs. Construct once in main.go and outlive camp transitions —
// Start picks up new camp identity, Stop teardown is mostly a no-op
// since CAs are kept on disk between runs.
type Service struct {
	store *config.Store
	eng   *engine.Engine

	mu        sync.Mutex
	myCA   *CA                // current camp's local CA (HTTPS termination)
	peers     map[string]PeerEntry // fingerprint → discovered peer CA
	campID    string              // active camp; set in Start/Load
}

// New constructs a Service. store and engine must outlive it.
// store holds persisted peer-CA metadata; engine is consulted only
// for live peer iteration + the tunnel HTTP port at poll time.
func New(store *config.Store, eng *engine.Engine) *Service {
	return &Service{
		store: store,
		eng:   eng,
		peers: make(map[string]PeerEntry),
	}
}

// Start ensures the local CA is loaded (or freshly generated for
// this camp) and installs it into the system trust store. Also
// loads the on-disk peer-CA cache for the camp so the UI panel is
// populated immediately. campID is typically obtained from
// eng.Status().CampID by the caller.
//
// Failures here are non-fatal: HTTPS termination just falls back to
// browser warnings, and peer-CA discovery still works via PollPeers
// even if my CA failed.
func (s *Service) Start(campID string) error {
	s.mu.Lock()
	s.campID = campID
	s.mu.Unlock()
	if err := s.ensureMyCA(campID); err != nil {
		log.Printf("ca: %v (https disabled)", err)
	}
	s.loadPeerCAs()
	return nil
}

// Stop is currently a no-op — the local CA stays on disk and in the
// system trust store across runs; peer-CA caches likewise. Provided
// for symmetry with other services and to host future cleanup if a
// camp switch needs to drop the running CA.
func (s *Service) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.myCA = nil
	return nil
}

// MyCA returns the local CA used to issue leaf certs for the HTTPS
// reverse proxy. nil when Start failed or hasn't run yet.
func (s *Service) MyCA() *CA {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.myCA
}

// ensureMyCA loads the on-disk CA, regenerates it if missing or
// pinned to a different camp_id, and installs the cert into the
// system trust store. Called from Start; safe to invoke repeatedly.
func (s *Service) ensureMyCA(campID string) error {
	if campID == "" {
		return fmt.Errorf("ca: no camp_id")
	}
	zone := identity.CampLabel(campID)

	loaded, err := LoadCA(myCADir)
	if err != nil {
		log.Printf("ca: load: %v (will regenerate)", err)
		loaded = nil
	}
	if loaded != nil && !loaded.MatchesZone(zone) {
		log.Printf("ca: existing CA pinned to a different camp_id; rotating")
		_ = loaded.RemoveSystemTrust()
		loaded = nil
	}
	if loaded == nil {
		fresh, gerr := GenerateCA(zone)
		if gerr != nil {
			return fmt.Errorf("generate: %w", gerr)
		}
		if serr := fresh.Save(myCADir); serr != nil {
			return fmt.Errorf("save: %w", serr)
		}
		log.Printf("ca: generated %s (fp %s)", fresh.CommonName(), fresh.Fingerprint())
		loaded = fresh
	} else {
		log.Printf("ca: loaded %s (fp %s)", loaded.CommonName(), loaded.Fingerprint())
	}
	if loaded.IsSystemTrusted() {
		log.Printf("ca: already in system trust store (fp %s) — skipping install", loaded.Fingerprint())
	} else if err := loaded.EnsureSystemTrust(CACertPath(myCADir)); err != nil {
		log.Printf("ca: install in system trust store: %v (https will show warnings)", err)
	} else {
		log.Printf("ca: installed in system trust store (fp %s)", loaded.Fingerprint())
	}
	s.mu.Lock()
	s.myCA = loaded
	s.mu.Unlock()
	return nil
}

// peerCAsDir returns the per-camp cache dir for s.campID. Empty
// campID falls back to the root (legacy --peer mode, no camps).
func (s *Service) peerCAsDir() string {
	if s.campID == "" {
		return peerCAsRootDir
	}
	return filepath.Join(peerCAsRootDir, s.campID)
}

// ListPeerCAs returns a snapshot of currently-known peer CAs, sorted
// by peer name then fingerprint for stable UI rendering.
func (s *Service) ListPeerCAs() []PeerEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PeerEntry, 0, len(s.peers))
	for _, t := range s.peers {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PeerName == out[j].PeerName {
			return out[i].Fingerprint < out[j].Fingerprint
		}
		return out[i].PeerName < out[j].PeerName
	})
	return out
}

// loadPeerCAs reads the per-camp on-disk record of which peer CAs
// were already discovered. Idempotent and additive — running it
// twice re-asserts the same entries; running it after a camp switch
// merges the new camp's directory into the same in-memory set.
func (s *Service) loadPeerCAs() {
	dir := s.peerCAsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, en := range entries {
		if en.IsDir() || !strings.HasSuffix(en.Name(), ".crt") {
			continue
		}
		full := filepath.Join(dir, en.Name())
		body, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		block, _ := pem.Decode(body)
		if block == nil {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		fp := certFingerprint(cert)
		full256 := strings.ToUpper(fmt.Sprintf("%x", sha256.Sum256(cert.Raw)))
		installed := platform.TrustStoreContains(full256)
		installedAt := int64(0)
		if installed {
			if fi, err := os.Stat(full); err == nil {
				installedAt = fi.ModTime().Unix()
			}
		}
		s.peers[fp] = PeerEntry{
			PeerName:    strings.TrimSuffix(en.Name(), ".crt"),
			CommonName:  cert.Subject.CommonName,
			Fingerprint: fp,
			InstalledAt: installedAt,
			Installed:   installed,
		}
	}
}

// PollPeers blocks until ctx is done, polling every online peer's
// /api/ca-cert on a 30s tick (first run delayed 5s for the camp
// poller to discover peers). New CAs are persisted to disk + added
// to the in-memory set; install into the trust store is deferred to
// InstallPeerCA (gated on UI click — keychain prompts the user).
//
// Resilient to engine being temporarily down: TunnelHTTPPort empty
// or zero online peers just causes a tick to be skipped.
func (s *Service) PollPeers(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Second):
	}
	s.pollOnce(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.pollOnce(ctx)
	}
}

func (s *Service) pollOnce(ctx context.Context) {
	port := s.eng.TunnelHTTPPort()
	if port == "" {
		log.Printf("ca-poll: tunnel HTTP port not set — skipping")
		return
	}
	targets := s.eng.OnlinePeersForCAPoll()
	if len(targets) == 0 {
		return
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for _, t := range targets {
		if t.Host == "" {
			continue
		}
		url := "http://" + net.JoinHostPort(t.Host, port) + "/api/ca-cert"
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("ca-poll: peer %s: GET %s: %v", t.Name, url, err)
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if err != nil {
			log.Printf("ca-poll: peer %s: read body: %v", t.Name, err)
			continue
		}
		if resp.StatusCode != 200 {
			log.Printf("ca-poll: peer %s: HTTP %d (peer running an old f2f-mac without /api/ca-cert?)", t.Name, resp.StatusCode)
			continue
		}
		s.discover(t.Name, body)
	}
}

// discover parses the PEM body, computes a fingerprint, persists the
// cert to disk, and records it as a known-but-not-installed peer CA.
// Does NOT touch the system keychain — that's InstallPeerCA's job,
// gated on explicit UI click so we surface the macOS password prompt
// only on deliberate user action.
func (s *Service) discover(peerName string, pemBytes []byte) {
	cert, err := parseCACert(pemBytes)
	if err != nil {
		log.Printf("ca: peer %s: parse: %v", peerName, err)
		return
	}
	fp := certFingerprint(cert)

	s.mu.Lock()
	if _, seen := s.peers[fp]; seen {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	dir := s.peerCAsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("ca: mkdir %s: %v", dir, err)
		return
	}
	certPath := filepath.Join(dir, peerName+".crt")
	if err := os.WriteFile(certPath, pemBytes, 0o644); err != nil {
		log.Printf("ca: write %s: %v", certPath, err)
		return
	}

	full256 := strings.ToUpper(fmt.Sprintf("%x", sha256.Sum256(cert.Raw)))
	installed := platform.TrustStoreContains(full256)

	entry := PeerEntry{
		PeerName:    peerName,
		CommonName:  cert.Subject.CommonName,
		Fingerprint: fp,
		Installed:   installed,
	}
	if installed {
		entry.InstalledAt = time.Now().Unix()
	}
	s.mu.Lock()
	s.peers[fp] = entry
	campID := s.campID
	s.mu.Unlock()
	s.upsertPeerInCamp(campID, entry)
	log.Printf("ca: discovered peer %s CA (fp %s, installed=%v)", peerName, fp, installed)
}

// InstallPeerCA adds a previously-discovered peer CA to the system
// trust store as a trusted root. macOS prompts for the admin
// password. Triggered by the user clicking "install" in the UI.
func (s *Service) InstallPeerCA(fp string) error {
	s.mu.Lock()
	entry, ok := s.peers[fp]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown peer CA %s", fp)
	}
	if entry.Installed {
		return nil
	}
	certPath := filepath.Join(s.peerCAsDir(), entry.PeerName+".crt")
	log.Printf("ca: installing peer %s CA (fp %s) — macOS will prompt for password", entry.PeerName, fp)
	if err := platform.TrustStoreAdd(certPath); err != nil {
		return fmt.Errorf("install peer %s: %w", entry.PeerName, err)
	}
	s.mu.Lock()
	entry.Installed = true
	entry.InstalledAt = time.Now().Unix()
	s.peers[fp] = entry
	campID := s.campID
	s.mu.Unlock()
	s.upsertPeerInCamp(campID, entry)
	log.Printf("ca: installed peer %s CA", entry.PeerName)
	return nil
}

// RemovePeerCA drops one peer CA — deletes the on-disk PEM, the
// keychain entry, the in-memory cache, and the camp config record.
// Idempotent.
func (s *Service) RemovePeerCA(fingerprint string) error {
	if fingerprint == "" {
		return fmt.Errorf("empty fingerprint")
	}
	s.mu.Lock()
	entry, ok := s.peers[fingerprint]
	if ok {
		delete(s.peers, fingerprint)
	}
	campID := s.campID
	s.mu.Unlock()
	if !ok {
		return nil
	}
	certPath := filepath.Join(s.peerCAsDir(), entry.PeerName+".crt")
	if err := os.Remove(certPath); err != nil && !os.IsNotExist(err) {
		log.Printf("ca: remove %s: %v", certPath, err)
	}
	if err := platform.TrustStoreRemove(entry.CommonName); err != nil {
		log.Printf("ca: trust store remove %s: %v", entry.CommonName, err)
	}
	if campID != "" {
		_ = s.store.UpdateCamp(campID, func(c *config.Camp) {
			kept := c.TrustedPeers[:0]
			for _, t := range c.TrustedPeers {
				if t.Fingerprint == fingerprint {
					continue
				}
				kept = append(kept, t)
			}
			c.TrustedPeers = kept
		})
	}
	return nil
}

// upsertPeerInCamp upserts (by fingerprint) a peer-CA record into
// the camp config and persists. No-op when no camp is active. Common
// path for discover + install.
func (s *Service) upsertPeerInCamp(campID string, e PeerEntry) {
	if campID == "" {
		return
	}
	_ = s.store.UpdateCamp(campID, func(c *config.Camp) {
		for i, ex := range c.TrustedPeers {
			if ex.Fingerprint == e.Fingerprint {
				c.TrustedPeers[i] = peerEntryToConfig(e)
				return
			}
		}
		c.TrustedPeers = append(c.TrustedPeers, peerEntryToConfig(e))
	})
}

func peerEntryToConfig(e PeerEntry) config.TrustedPeer {
	return config.TrustedPeer{
		PeerName:    e.PeerName,
		CommonName:  e.CommonName,
		Fingerprint: e.Fingerprint,
		InstalledAt: e.InstalledAt,
	}
}

// certFingerprint returns the short (first 8 bytes of SHA-256, hex)
// fingerprint used as the in-memory key and surfaced to the UI. The
// full SHA-256 is computed separately when matching against the
// system trust store (which keys by full hash).
func certFingerprint(cert *x509.Certificate) string {
	h := sha256.Sum256(cert.Raw)
	return fmt.Sprintf("%x", h[:8])
}

func parseCACert(p []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(p)
	if block == nil {
		return nil, fmt.Errorf("not a PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}
