package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	return &Store{dir: t.TempDir(), camps: make(map[string]*Camp)}
}

// TestCacheSplit: caches (peer_catalog / trusted_peers) persist to cache.json,
// not config.json, and reload intact.
func TestCacheSplit(t *testing.T) {
	s := testStore(t)
	const id = "cafef00d_x"
	c := NewCamp(id, "me")
	c.MyDomains = []Domain{{Name: "gitea", Port: 3000}}
	c.PeerCatalog = []Peer{{Name: "alice", Pub: "aa"}}
	c.TrustedPeers = []TrustedPeer{{PeerName: "alice", Fingerprint: "ff"}}
	if err := s.SaveCamp(id, c); err != nil {
		t.Fatal(err)
	}

	cfg, _ := os.ReadFile(s.campPath(id))
	if strings.Contains(string(cfg), "peer_catalog") || strings.Contains(string(cfg), "trusted_peers") {
		t.Fatalf("config.json must not carry caches:\n%s", cfg)
	}
	if !strings.Contains(string(cfg), "my_domains") {
		t.Fatalf("config.json missing intent (my_domains):\n%s", cfg)
	}
	cache, err := os.ReadFile(s.cachePath(id))
	if err != nil {
		t.Fatalf("cache.json not written: %v", err)
	}
	if !strings.Contains(string(cache), "peer_catalog") || !strings.Contains(string(cache), "trusted_peers") {
		t.Fatalf("cache.json must carry caches:\n%s", cache)
	}

	// Reload from disk (fresh store) → caches restored from cache.json.
	s2 := testStore(t)
	s2.dir = s.dir
	got, err := s2.SnapshotCamp(id)
	if err != nil || got == nil {
		t.Fatalf("reload: %v", err)
	}
	if len(got.PeerCatalog) != 1 || got.PeerCatalog[0].Name != "alice" {
		t.Fatalf("peer_catalog not restored: %+v", got.PeerCatalog)
	}
	if len(got.TrustedPeers) != 1 || got.TrustedPeers[0].Fingerprint != "ff" {
		t.Fatalf("trusted_peers not restored: %+v", got.TrustedPeers)
	}
}

// TestCacheMigration: an old config.json that still embeds the caches migrates
// them into cache.json on first load, and config.json is rewritten without them.
func TestCacheMigration(t *testing.T) {
	s := testStore(t)
	const id = "deadbeef_x"
	// Hand-write a legacy config.json carrying the caches inline.
	legacy := map[string]any{
		"camp_id":       id,
		"identity":      map[string]string{"name": "me"},
		"my_domains":    []any{},
		"peer_catalog":  []map[string]any{{"name": "bob", "pub": "bb"}},
		"trusted_peers": []map[string]any{{"peer_name": "bob", "fingerprint": "cc"}},
	}
	data, _ := json.MarshalIndent(legacy, "", "  ")
	if err := os.MkdirAll(filepath.Dir(s.campPath(id)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.campPath(id), data, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := s.SnapshotCamp(id)
	if err != nil || got == nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.PeerCatalog) != 1 || got.PeerCatalog[0].Pub != "bb" {
		t.Fatalf("legacy peer_catalog not migrated: %+v", got.PeerCatalog)
	}
	if len(got.TrustedPeers) != 1 || got.TrustedPeers[0].Fingerprint != "cc" {
		t.Fatalf("legacy trusted_peers not migrated: %+v", got.TrustedPeers)
	}

	// Persist (a normal update) → config.json drops the caches, cache.json has them.
	if err := s.UpdateCamp(id, func(c *Camp) {}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(s.campPath(id))
	if strings.Contains(string(cfg), "peer_catalog") || strings.Contains(string(cfg), "trusted_peers") {
		t.Fatalf("migrated config.json still carries caches:\n%s", cfg)
	}
	cache, err := os.ReadFile(s.cachePath(id))
	if err != nil || !strings.Contains(string(cache), "bb") {
		t.Fatalf("cache.json missing migrated data: %v\n%s", err, cache)
	}
}
