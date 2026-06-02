// Package trust is the user-facing service layer for peer-CA discovery
// and system-keychain management.
//
// Each f2f peer publishes its locally-issued CA at /api/ca-cert. The
// poll loop walks online peers every 30s, fetches their CA, persists
// it under /var/lib/f2f/trusted-peers/<camp>/<peer>.crt, and records
// metadata in the in-memory map. Installation into the macOS system
// trust store is a deliberate user action via Install — the macOS
// password prompt is triggered there and only there.
package trust

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
	"github.com/vseplet/f2f/source/helper/platform"
)

// Entry is one row in the UI's trusted-peers panel.
type Entry struct {
	PeerName    string `json:"peer_name"`
	CommonName  string `json:"common_name"`
	Fingerprint string `json:"fingerprint"`
	InstalledAt int64  `json:"installed_at"`
	Installed   bool   `json:"installed"`
}

// Service owns the in-memory trust set and the poll loop. Construct
// once in main.go and outlive camp transitions. Load is idempotent
// (additive) and safe to call from each engine.OnStarted hook so the
// per-camp on-disk cache is picked up after camp switches.
type Service struct {
	eng *engine.Engine

	mu  sync.Mutex
	cas map[string]Entry // keyed by fingerprint
}

// New constructs a Service. The engine must outlive the service.
func New(eng *engine.Engine) *Service {
	return &Service{
		eng: eng,
		cas: make(map[string]Entry),
	}
}

// List returns a snapshot of the current trust set, sorted by peer
// name then fingerprint for stable UI rendering.
func (s *Service) List() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.cas))
	for _, t := range s.cas {
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

// Load reads the per-camp on-disk record of which peer CAs were
// already discovered (so we don't keychain-install them again on
// every engine restart). Idempotent and additive — running it twice
// just re-asserts the same entries; running it after a camp switch
// merges the new camp's directory into the same in-memory set.
func (s *Service) Load() {
	dir := s.eng.TrustedPeersDir()
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
		s.cas[fp] = Entry{
			PeerName:    strings.TrimSuffix(en.Name(), ".crt"),
			CommonName:  cert.Subject.CommonName,
			Fingerprint: fp,
			InstalledAt: installedAt,
			Installed:   installed,
		}
	}
}

// Run blocks until ctx is done, polling every online peer's
// /api/ca-cert once on a 30s tick (first run delayed by 5s to let
// the camp poller discover peers). New CAs are persisted to disk and
// added to the in-memory set; installation into the trust store is
// deferred to Install.
//
// The service is resilient to the engine being temporarily down:
// TunnelHTTPPort empty or zero online peers just causes a tick to
// be skipped, not the loop to exit.
func (s *Service) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Second):
	}
	s.pollAll(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.pollAll(ctx)
	}
}

func (s *Service) pollAll(ctx context.Context) {
	port := s.eng.TunnelHTTPPort()
	if port == "" {
		log.Printf("ca-poll: tunnel HTTP port not set (engine running without UI?) — skipping")
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
// It does NOT touch the system keychain — that's Install's job, which
// is gated on an explicit UI click so we surface the macOS password
// prompt only on deliberate user action.
func (s *Service) discover(peerName string, pemBytes []byte) {
	cert, err := parseCACert(pemBytes)
	if err != nil {
		log.Printf("ca: peer %s: parse: %v", peerName, err)
		return
	}
	fp := certFingerprint(cert)

	s.mu.Lock()
	if _, seen := s.cas[fp]; seen {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	dir := s.eng.TrustedPeersDir()
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

	entry := Entry{
		PeerName:    peerName,
		CommonName:  cert.Subject.CommonName,
		Fingerprint: fp,
		Installed:   installed,
	}
	if installed {
		entry.InstalledAt = time.Now().Unix()
	}
	s.mu.Lock()
	s.cas[fp] = entry
	s.mu.Unlock()
	s.eng.UpsertTrustedPeerInCamp(entryToCampPeer(entry))
	log.Printf("ca: discovered peer %s CA (fp %s, installed=%v)", peerName, fp, installed)
}

// Remove drops one peer CA — deletes the on-disk PEM, the keychain
// entry, the in-memory cache, and the camp config entry. Idempotent:
// missing pieces are skipped without error. Engine must be running so
// the camp config can be persisted.
func (s *Service) Remove(fingerprint string) error {
	if fingerprint == "" {
		return fmt.Errorf("empty fingerprint")
	}
	s.mu.Lock()
	entry, ok := s.cas[fingerprint]
	if ok {
		delete(s.cas, fingerprint)
	}
	s.mu.Unlock()
	if !ok {
		return nil
	}
	certPath := filepath.Join(s.eng.TrustedPeersDir(), entry.PeerName+".crt")
	if err := os.Remove(certPath); err != nil && !os.IsNotExist(err) {
		log.Printf("ca: remove %s: %v", certPath, err)
	}
	if err := platform.TrustStoreRemove(entry.CommonName); err != nil {
		log.Printf("ca: trust store remove %s: %v", entry.CommonName, err)
	}
	s.eng.RemoveTrustedPeerFromCamp(fingerprint)
	return nil
}

// Install adds a previously-discovered peer CA to the system trust
// store as a trusted root. macOS prompts for the admin password.
// Triggered by the user clicking "install" in the UI.
func (s *Service) Install(fp string) error {
	s.mu.Lock()
	entry, ok := s.cas[fp]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown peer CA %s", fp)
	}
	if entry.Installed {
		return nil
	}
	certPath := filepath.Join(s.eng.TrustedPeersDir(), entry.PeerName+".crt")
	log.Printf("ca: installing peer %s CA (fp %s) — macOS will prompt for password", entry.PeerName, fp)
	if err := platform.TrustStoreAdd(certPath); err != nil {
		return fmt.Errorf("install peer %s: %w", entry.PeerName, err)
	}
	s.mu.Lock()
	entry.Installed = true
	entry.InstalledAt = time.Now().Unix()
	s.cas[fp] = entry
	s.mu.Unlock()
	s.eng.UpsertTrustedPeerInCamp(entryToCampPeer(entry))
	log.Printf("ca: installed peer %s CA", entry.PeerName)
	return nil
}

func entryToCampPeer(e Entry) config.TrustedPeer {
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
// system trust store, which keys by full hash.
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
