//go:build darwin

// Package config persists user-editable engine configuration under
// $HOME/.f2f/. Two file types live there:
//
//   state.json               — global pointer: last camp_id + list of known camps
//   <camp_id>.config.json    — per-camp: name, intercepts, my-domains, firewall,
//                              trusted-peer CA metadata, last-known peer roster
//
// Writes are atomic (tmp + rename) and chowned to $SUDO_USER so the
// user (not root) can manage these files from Finder.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
)

const dirName = ".f2f"

// Store is a singleton handle to the on-disk config directory.
// All file I/O serialises through mu so concurrent saves don't race
// each other's tmp-file rename.
type Store struct {
	mu  sync.Mutex
	dir string
}

// State is the global file ($HOME/.f2f/state.json). Holds the most
// recently used camp_id (for engine auto-start) and a roster of every
// camp the user has joined (for the UI dropdown).
type State struct {
	LastCampID string      `json:"last_camp_id,omitempty"`
	KnownCamps []KnownCamp `json:"known_camps,omitempty"`
}

type KnownCamp struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Camp is one per-camp config file ($HOME/.f2f/<camp_id>.config.json).
// Empty slices marshal as [], not null, so the file is friendly to
// hand-edit when nothing is set yet.
type Camp struct {
	CampID string `json:"camp_id"`
	// LegacyName carries the top-level "name" field that older builds
	// wrote before we moved name into Identity. Marshalled with
	// omitempty so it disappears on next save once migrated. Always
	// empty after normaliseCamp runs.
	LegacyName   string        `json:"name,omitempty"`
	Identity     Identity      `json:"identity"`
	Intercepts   []Intercept   `json:"intercepts"`
	MyDomains    []Domain      `json:"my_domains"`
	Firewall     []Firewall    `json:"firewall"`
	TrustedPeers []TrustedPeer `json:"trusted_peers"`
	PeerCatalog  []Peer        `json:"peer_catalog"`
}

// Identity is who you are in this camp: display name + public side of
// the per-camp Ed25519 keypair. Pub/Fingerprint are mirrored here so
// the UI can render them without reading /var/lib/f2f/identity/. The
// matching private key never appears in this file — it lives only
// under /var/lib/f2f/identity/<camp_id>/priv.key (mode 0600, root).
type Identity struct {
	Name        string `json:"name"`                  // alias shown to peers
	Pub         string `json:"pub,omitempty"`         // 64-hex Ed25519 public key
	Fingerprint string `json:"fingerprint,omitempty"` // first 16 hex of SHA-256(pub)
}

type Intercept struct {
	Spec string `json:"spec"`
	Peer string `json:"peer"`
}

type Domain struct {
	Name  string `json:"name"`
	Host  string `json:"host,omitempty"`
	Port  int    `json:"port,omitempty"`
	Proto string `json:"proto,omitempty"`
}

type Firewall struct {
	Port        int    `json:"port"`
	Protocol    string `json:"protocol"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// TrustedPeer is a fingerprint-level mirror of one row in the UI's
// trusted-peers panel. The actual PEM stays under
// /var/lib/f2f/trusted-peers/<peer_name>.crt — this entry exists so the
// UI can render the list and the user can remove a CA by fingerprint
// without going through the keychain.
type TrustedPeer struct {
	PeerName    string `json:"peer_name"`
	CommonName  string `json:"common_name,omitempty"`
	Fingerprint string `json:"fingerprint"`
	InstalledAt int64  `json:"installed_at,omitempty"`
}

// Peer is one row of the persisted peer catalog. Mirrors what camp
// reports plus our local view. On engine restart this is the seed for
// e.peers so the UI can show known nodes (Online=false, no UDP target)
// even before the first camp poll completes.
type Peer struct {
	Name        string `json:"name"`
	TunnelIP    string `json:"tunnel_ip"`
	PublicIP    string `json:"public_ip,omitempty"`
	UDPPort     int    `json:"udp_port,omitempty"`
	UDPEndpoint string `json:"udp_endpoint,omitempty"`
	JoinedAt    int64  `json:"joined_at,omitempty"`
	LastSeenAt  int64  `json:"last_seen_at,omitempty"`
	Online      bool   `json:"online"`
	// Domains is the last successful snapshot of names this peer was
	// publishing via /api/domains. Kept across poll failures so the
	// UI doesn't flicker, and across engine restarts so we know what
	// to expect from this peer before the first poll lands. Refreshed
	// only on a successful poll — never reset on transient errors.
	Domains []Domain `json:"domains,omitempty"`
}

// NewStore resolves $HOME/.f2f/, creates it if missing, chowns to
// $SUDO_USER, and returns a handle. The engine creates exactly one
// Store on Start and reuses it for every save.
func NewStore() (*Store, error) {
	dir := filepath.Join(userHome(), dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	chownToUser(dir)
	return &Store{dir: dir}, nil
}

// Dir is the on-disk directory backing this store.
func (s *Store) Dir() string { return s.dir }

// statePath / campPath build the absolute paths under s.dir.
func (s *Store) statePath() string             { return filepath.Join(s.dir, "state.json") }
func (s *Store) campPath(id string) string {
	return filepath.Join(s.dir, id+".config.json")
}

// LoadState reads state.json. A missing file is not an error — returns
// a zero-value State so the caller can populate and save.
func (s *Store) LoadState() (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse state.json: %w", err)
	}
	return &st, nil
}

// SaveState writes state.json atomically.
func (s *Store) SaveState(st *State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeJSON(s.statePath(), st)
}

// LoadCamp reads a per-camp config. Missing file returns (nil, nil)
// so callers can distinguish "never configured" from a read error.
func (s *Store) LoadCamp(id string) (*Camp, error) {
	if !validCampID(id) {
		return nil, fmt.Errorf("invalid camp_id %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.campPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Camp
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s.config.json: %w", id, err)
	}
	normaliseCamp(&c, id)
	return &c, nil
}

// SaveCamp writes a per-camp config atomically. CampID on the input
// is overwritten with id (defensive — single source of truth is the
// filename).
func (s *Store) SaveCamp(id string, c *Camp) error {
	if !validCampID(id) {
		return fmt.Errorf("invalid camp_id %q", id)
	}
	if c == nil {
		return errors.New("nil camp config")
	}
	c.CampID = id
	normaliseCamp(c, id)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeJSON(s.campPath(id), c)
}

// writeJSON marshals v to a tmp file in the same directory and renames
// over the target — making the swap atomic on the same filesystem.
// chowns the file to $SUDO_USER so the user can manage it from Finder.
func (s *Store) writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	chownToUser(path)
	return nil
}

// normaliseCamp guarantees non-nil slices and a populated CampID so
// callers never need to nil-check before iterating. Returns true when
// the in-memory shape diverges from what would unmarshal cleanly —
// caller should force a save to migrate the on-disk file.
func normaliseCamp(c *Camp, id string) (migrated bool) {
	if c.CampID == "" {
		c.CampID = id
	}
	if c.LegacyName != "" {
		if c.Identity.Name == "" {
			c.Identity.Name = c.LegacyName
		}
		c.LegacyName = ""
		migrated = true
	}

	if c.Intercepts == nil {
		c.Intercepts = []Intercept{}
	}
	if c.MyDomains == nil {
		c.MyDomains = []Domain{}
	}
	if c.Firewall == nil {
		c.Firewall = []Firewall{}
	}
	if c.TrustedPeers == nil {
		c.TrustedPeers = []TrustedPeer{}
	}
	if c.PeerCatalog == nil {
		c.PeerCatalog = []Peer{}
	}
	return migrated
}

// NewCamp returns a zero-valued Camp with non-nil slices, ready to
// populate and save.
func NewCamp(id, name string) *Camp {
	c := &Camp{CampID: id, Identity: Identity{Name: name}}
	normaliseCamp(c, id)
	return c
}

var campIDRe = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,128}$`)

// validCampID rejects ids that would escape s.dir or break the
// filename. camp server already slugs ids; this is belt-and-braces.
func validCampID(id string) bool { return campIDRe.MatchString(id) }

// userHome returns the home directory of the invoking (non-root)
// user. f2f-mac runs as root via sudo; we want files to live under
// the user's home and be readable from Finder, so we resolve via
// SUDO_USER first.
func userHome() string {
	if su := os.Getenv("SUDO_USER"); su != "" {
		return filepath.Join("/Users", su)
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/tmp"
}

// chownToUser switches ownership of one path (no recursion) to
// $SUDO_USER. Engine runs as root via sudo — anything we write is
// owned by root by default. Best-effort: failures are silent.
func chownToUser(path string) {
	su := os.Getenv("SUDO_USER")
	if su == "" {
		return
	}
	u, err := user.Lookup(su)
	if err != nil {
		return
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	_ = os.Lchown(path, uid, gid)
}
