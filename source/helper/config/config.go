// Package config persists user-editable engine configuration under
// $HOME/.f2f/. Two file types live there:
//
//	state.json               — global pointer: last camp_id + list of known camps
//	<camp_id>.config.json    — per-camp: name, intercepts, my-domains, firewall,
//	                           trusted-peer CA metadata, last-known peer roster
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
	"strings"
	"sync"
)

const dirName = ".f2f"

// Store is a singleton handle to the on-disk config directory and
// the single source of truth for camp config state. All in-memory
// state goes through mu — readers get deep copies, writers go through
// UpdateCamp which serialises read-modify-write atomically. Disk I/O
// is interleaved with the same mu so the in-memory copy and the file
// never diverge.
type Store struct {
	mu    sync.Mutex
	dir   string
	camps map[string]*Camp // canonical in-memory copy, keyed by camp_id
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
	LegacyName string `json:"name,omitempty"`
	// ServerURL / StunAddr point at the rendezvous server hosting
	// this camp. Defaults applied by NewCamp; can be overridden by
	// hand-editing the per-camp file. Used by mesh/camp to spin
	// up the announce client + HTTP poller.
	ServerURL string   `json:"server_url,omitempty"`
	StunAddr  string   `json:"stun_addr,omitempty"`
	Identity  Identity `json:"identity"`
	// Profile is the active user profile on THIS device — its user_id. The
	// profile itself (name, passkeys, …) lives as a block.user in db; this only
	// records which user_id this device operates as. Empty = no profile yet
	// (the UI then forces profile creation). See docs/IDENTITY.md.
	Profile      string        `json:"profile,omitempty"`
	Intercepts   []Intercept   `json:"intercepts"`
	MyDomains    []Domain      `json:"my_domains"`
	Firewall     []Firewall    `json:"firewall"`
	TrustedPeers []TrustedPeer `json:"trusted_peers"`
	PeerCatalog  []Peer        `json:"peer_catalog"`
	// Shell gates the remote-terminal service (services/shell). Opening a
	// shell on this machine is the single most dangerous capability, so the
	// long-term default is opt-in + an explicit allowlist of peer pubs.
	// (Currently permissive-by-default for testing — see services/shell.)
	Shell Shell `json:"shell"`
	// Vnc gates the remote-desktop bridge (services/vnc). Permissive-by-
	// default for testing too.
	Vnc Vnc `json:"vnc"`
}

// Shell is the per-camp policy for the remote-terminal service. Command
// overrides what the PTY runs (default: the system `login`, so OS
// authentication still applies). Enabled/Allowed will gate access once we
// flip off the testing default.
type Shell struct {
	Enabled bool     `json:"enabled"`
	Allowed []string `json:"allowed,omitempty"` // peer pubs allowed to open a shell
	Command string   `json:"command,omitempty"` // PTY program; empty = system login
}

// Vnc is the per-camp policy for the remote-desktop bridge (services/vnc),
// which proxies a peer to the host's local VNC server (e.g. macOS Screen
// Sharing on :5900). Auth is handled by that VNC server; Enabled/Allowed
// gate who can reach it. Addr overrides the local VNC endpoint.
type Vnc struct {
	Enabled bool     `json:"enabled"`
	Allowed []string `json:"allowed,omitempty"` // peer pubs allowed to open a desktop
	Addr    string   `json:"addr,omitempty"`    // local VNC server; empty = 127.0.0.1:5900
}

// DefaultCampServerURL and DefaultCampStunAddr are the production
// rendezvous endpoints we ship with. NewCamp stamps these into the
// per-camp file so users can hand-edit later (point at their own
// camp server) without code changes.
const (
	DefaultCampServerURL = "wss://f2f-camp.fly.dev/ws"
	DefaultCampStunAddr  = "f2f-camp.fly.dev:3478"
)

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
	Pub         string `json:"pub,omitempty"` // 64-hex Ed25519 pubkey; stable identity, used as the in-memory peers map key
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
	// Firewall is the last successful snapshot of the peer's
	// user-configured open-port list (from their /api/firewall). Same
	// persistence semantics as Domains. Built-in ports aren't stored —
	// every f2f peer has the same built-ins implicitly.
	Firewall []Firewall `json:"firewall,omitempty"`
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
	return &Store{dir: dir, camps: make(map[string]*Camp)}, nil
}

// SnapshotCamp returns a deep copy of the in-memory camp config for id,
// loading from disk on first access (subsequent calls hit the cache).
// Returns (nil, nil) when no on-disk camp exists yet — caller decides
// how to handle (typically: prompt for name on first start).
func (s *Store) SnapshotCamp(id string) (*Camp, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.ensureLoadedLocked(id)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, nil
	}
	return deepCopyCamp(c), nil
}

// UpdateCamp atomically loads the camp config (from cache or disk),
// applies fn to it, persists the result to disk, and updates the
// cache. fn sees the canonical state and mutates it in place. The
// whole RMW happens under s.mu so concurrent updates from engine
// and services don't lose each other.
//
// Returns an error if no on-disk camp exists for id yet — callers
// must SaveCamp first to create the file (typically engine.Start
// after a successful camp announce).
func (s *Store) UpdateCamp(id string, fn func(*Camp)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.ensureLoadedLocked(id)
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("camp %s not initialised yet", id)
	}
	fn(c)
	normaliseCamp(c, id)
	return s.writeJSON(s.campPath(id), c)
}

// ensureLoadedLocked returns the cached *Camp for id, lazily loading
// from disk on first request. Caller holds s.mu. (nil, nil) means no
// file exists yet — the caller must SaveCamp / NewCamp to bring the
// camp into existence.
func (s *Store) ensureLoadedLocked(id string) (*Camp, error) {
	if !validCampID(id) {
		return nil, fmt.Errorf("invalid camp_id %q", id)
	}
	if c, ok := s.camps[id]; ok {
		return c, nil
	}
	data, err := os.ReadFile(s.campPath(id))
	if errors.Is(err, os.ErrNotExist) {
		// migration: fall back to the legacy flat file. Next SaveCamp
		// rewrites it under the per-camp dir.
		if old, oerr := os.ReadFile(s.oldCampPath(id)); oerr == nil {
			data, err = old, nil
		} else {
			return nil, nil
		}
	}
	if err != nil {
		return nil, err
	}
	var c Camp
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s.config.json: %w", id, err)
	}
	normaliseCamp(&c, id)
	s.camps[id] = &c
	return &c, nil
}

// deepCopyCamp returns a fresh *Camp with no aliasing to src. Cheap
// (slices/strings copied by value), but exhaustive — readers can
// mutate the result without affecting cache state.
func deepCopyCamp(src *Camp) *Camp {
	if src == nil {
		return nil
	}
	d := *src
	d.Intercepts = append([]Intercept(nil), src.Intercepts...)
	d.MyDomains = append([]Domain(nil), src.MyDomains...)
	d.PeerCatalog = make([]Peer, len(src.PeerCatalog))
	for i, p := range src.PeerCatalog {
		pp := p
		pp.Domains = append([]Domain(nil), p.Domains...)
		pp.Firewall = append([]Firewall(nil), p.Firewall...)
		d.PeerCatalog[i] = pp
	}
	d.TrustedPeers = append([]TrustedPeer(nil), src.TrustedPeers...)
	d.Firewall = append([]Firewall(nil), src.Firewall...)
	return &d
}

// Dir is the on-disk directory backing this store.
func (s *Store) Dir() string { return s.dir }

// CampDir returns (creating + chowning) the per-camp directory
// ~/.f2f/<camp_id>/ where ALL of a camp's state lives — config, the
// messenger/notifications DBs, the per-camp log, shared/ and drops/.
// Empty id maps to "_root". This is the single source of truth for the
// per-camp layout; other packages build their paths under it.
func (s *Store) CampDir(id string) string {
	seg := id
	if seg == "" {
		seg = "_root"
	}
	// defensive: never let a camp_id escape s.dir (it's validated upstream,
	// but this is the filesystem boundary for every per-camp artifact).
	seg = strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(seg)
	dir := filepath.Join(s.dir, seg)
	_ = os.MkdirAll(dir, 0o755)
	chownToUser(dir)
	return dir
}

// statePath / campPath build the absolute paths under s.dir.
func (s *Store) statePath() string { return filepath.Join(s.dir, "state.json") }

// campPath is the per-camp config file: ~/.f2f/<camp_id>/config.json.
func (s *Store) campPath(id string) string {
	seg := id
	if seg == "" {
		seg = "_root"
	}
	return filepath.Join(s.dir, seg, "config.json")
}

// oldCampPath is the legacy flat location (~/.f2f/<camp_id>.config.json),
// read as a one-time fallback so existing camps migrate without loss.
func (s *Store) oldCampPath(id string) string {
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
// Equivalent to SnapshotCamp — kept for back-compat.
func (s *Store) LoadCamp(id string) (*Camp, error) {
	return s.SnapshotCamp(id)
}

// SaveCamp writes a per-camp config atomically and updates the cache.
// CampID on the input is overwritten with id (defensive — single
// source of truth is the filename). Primarily used at engine.Start
// to bring a fresh camp into existence; subsequent mutations should
// go through UpdateCamp so concurrent updates serialise properly.
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
	s.camps[id] = deepCopyCamp(c)
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
	// Ensure the parent dir exists (per-camp dirs are created on demand).
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	chownToUser(dir)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
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
	if c.ServerURL == "" {
		c.ServerURL = DefaultCampServerURL
		migrated = true
	}
	if c.StunAddr == "" {
		c.StunAddr = DefaultCampStunAddr
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

// NewCamp returns a zero-valued Camp with non-nil slices and default
// rendezvous endpoints, ready to populate and save.
func NewCamp(id, name string) *Camp {
	c := &Camp{
		CampID:    id,
		ServerURL: DefaultCampServerURL,
		StunAddr:  DefaultCampStunAddr,
		Identity:  Identity{Name: name},
	}
	normaliseCamp(c, id)
	return c
}

var campIDRe = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,128}$`)

// validCampID rejects ids that would escape s.dir or break the
// filename. camp server already slugs ids; this is belt-and-braces.
func validCampID(id string) bool { return campIDRe.MatchString(id) }

// userHome returns the home directory of the invoking (non-root)
// user. The helper runs as root via sudo; we want files to live
// under the user's home and be readable from their file manager,
// so we resolve via SUDO_USER first and look up the actual home
// from the OS user database — /Users/<name> on macOS,
// /home/<name> on linux, etc.
func userHome() string {
	if su := os.Getenv("SUDO_USER"); su != "" {
		if u, err := user.Lookup(su); err == nil && u.HomeDir != "" {
			return u.HomeDir
		}
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
