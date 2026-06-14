// Package drop is the user-facing service around the BitTorrent
// client used for camp file sharing. It owns:
//
//   - The anacrolix client lifecycle (Start binds it on the overlay
//     v4, Stop closes it).
//   - The on-disk catalog (shared/ + downloads/, persisted across
//     restarts via rescan + downloads.json).
//   - Reconciliation loops: prune missing files, re-feed peer
//     addresses to stalled downloads, chown root-owned writes back
//     to the invoking user.
//   - Cross-peer file discovery over the bus (PollPeers).
//
// Constructed once in main.go; Start runs on eng.OnStarted and Stop
// on eng.OnStopped. The peers' file lists are cached in-memory and
// surfaced through PeerFiles(pub).
package drop

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vseplet/f2f/source/helper/clog"
	"github.com/vseplet/f2f/source/helper/mesh/bus"
	"github.com/vseplet/f2f/source/helper/mesh/engine"
	internaltorrent "github.com/vseplet/f2f/source/helper/services/drop/torrent"
)

// busTypeFiles is the bus message type serving our seeded-file catalog to
// peers. busTypeFilesNext is the namespaced name we're migrating to: accept
// both during the wire rollout, keep requesting the old one, flip later.
const (
	busTypeFiles     = "files"
	busTypeFilesNext = "drop.files"
)

// PeerFile is one file entry as published by a peer's /api/files.
// Path is stripped — peer-facing data only.
type PeerFile struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	InfoHash string `json:"info_hash"`
	Magnet   string `json:"magnet"`
}

// Service owns the live torrent client + the catalog of files
// polled from peers. Construct once in main.go and outlive camp
// transitions; Start/Stop are driven by engine lifecycle hooks.
type Service struct {
	eng     *engine.Engine
	bus     *bus.Service
	campDir func(campID string) string // ~/.f2f/<camp_id>/ (creates it)

	mu        sync.Mutex
	client    *internaltorrent.Client
	campID    string
	sharedDir string
	downloads string

	peerMu    sync.RWMutex
	peerFiles map[string][]PeerFile // pub → files

	// scopes pins a seeded file to one conversation (channel id or dm key).
	// A scoped file is private: it's hidden from the public catalog and only
	// served to peers who are members of that conversation (see isMember).
	// infohash → scope.
	scopeMu sync.RWMutex
	scopes  map[string]string

	// isMember reports whether a peer may see files scoped to a conversation.
	// Backed by the messenger (channel roster / dm pair). nil = scoped files
	// stay fully private (served to nobody but ourselves).
	isMember func(scope, pub string) bool
}

// SetMembershipCheck wires the conversation-membership predicate used to
// decide who may see a scoped (private) file in the catalog. Set once after
// construction (the messenger isn't built yet at drop.New time).
func (s *Service) SetMembershipCheck(fn func(scope, pub string) bool) {
	s.isMember = fn
}

// New constructs a Service. campDir resolves a camp's directory
// (config.Store.CampDir). The engine must outlive the service.
func New(eng *engine.Engine, campDir func(campID string) string, b *bus.Service) *Service {
	return &Service{
		eng:       eng,
		bus:       b,
		campDir:   campDir,
		peerFiles: map[string][]PeerFile{},
		scopes:    map[string]string{},
	}
}

// Register installs the bus handler that serves our seeded-file list
// to peers (Path never leaves the machine — PeerFile has no such
// field). Call once after construction.
func (s *Service) Register() {
	if s.bus == nil {
		return
	}
	filesHandler := func(fromPub string, _ []byte) ([]byte, error) {
		c := s.Client()
		if c == nil {
			return nil, fmt.Errorf("torrent client not running")
		}
		out := []PeerFile{}
		for _, h := range c.ListSeeds() {
			// A scoped file is private: serve it only to members of its
			// conversation, so non-members don't see it but members' downloads
			// still find it in our catalog (and don't get retired as zombies).
			if scope := s.scopeOf(h.InfoHash); scope != "" {
				if s.isMember == nil || !s.isMember(scope, fromPub) {
					continue
				}
			}
			out = append(out, PeerFile{
				Name: h.Name, Size: h.Size,
				InfoHash: h.InfoHash, Magnet: h.Magnet,
			})
		}
		return json.Marshal(out)
	}
	s.bus.Handle(busTypeFiles, filesHandler)
	s.bus.Handle(busTypeFilesNext, filesHandler) // accept the new name during rollout
}

// Start binds the BT client on the overlay v4 alias for the given
// camp, restores previously-shared and -downloaded torrents, and
// kicks off the chown/prune background goroutines. Failure here is
// non-fatal — file sharing just becomes unavailable until next Start.
func (s *Service) Start(campID, localIP string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		return fmt.Errorf("drop: already started")
	}
	s.campID = campID
	s.sharedDir = filepath.Join(s.campDir(campID), "shared")
	s.downloads = filepath.Join(s.campDir(campID), "drops")
	s.loadScopes()
	t0 := time.Now()
	opts := internaltorrent.Options{
		ListenAddr:   net.JoinHostPort(localIP, fmt.Sprint(internaltorrent.DefaultPort)),
		SharedDir:    s.sharedDir,
		DownloadsDir: s.downloads,
	}
	clog.Info("torrent", "binding on %s …", opts.ListenAddr)
	c, err := internaltorrent.New(opts)
	if err != nil {
		return fmt.Errorf("drop: bind %s: %w", opts.ListenAddr, err)
	}
	s.client = c
	clog.Info("torrent", "ready in %v (shared=%s downloads=%s)",
		time.Since(t0).Round(time.Millisecond), opts.SharedDir, opts.DownloadsDir)
	chownToUser(opts.SharedDir)
	chownToUser(opts.DownloadsDir)
	go s.rescanSharedDir(c, opts.SharedDir)
	go s.restoreDownloads(c)
	go s.chownLoop(opts.SharedDir, opts.DownloadsDir)
	go s.pruneLoop(c)
	return nil
}

// Stop closes the torrent client. Idempotent.
func (s *Service) Stop() error {
	s.mu.Lock()
	c := s.client
	s.client = nil
	s.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.Close()
}

// Client returns the live torrent client (nil if not started).
// Web layer uses this to expose ListSeeds/ListDownloads in /api/files.
func (s *Service) Client() *internaltorrent.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// DownloadsDir returns the user-visible on-disk path for incoming
// downloads. Empty before Start.
func (s *Service) DownloadsDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.downloads
}

// AddDownload wraps Client.AddDownload and persists the entry so it
// survives restart. Idempotent — re-adding the same info_hash is a
// no-op.
func (s *Service) AddDownload(magnet string, peers []string) (*internaltorrent.Download, error) {
	c := s.Client()
	if c == nil {
		return nil, fmt.Errorf("drop: torrent client not running")
	}
	d, err := c.AddDownload(magnet, peers)
	if err != nil {
		return nil, err
	}
	saved := s.loadSavedDownloads()
	for _, sv := range saved {
		if sv.InfoHash == d.InfoHash {
			return d, nil
		}
	}
	saved = append(saved, savedDownload{
		Magnet: magnet, InfoHash: d.InfoHash, Peers: peers,
	})
	if err := s.saveDownloads(saved); err != nil {
		clog.Warn("downloads", "persist: %v", err)
	}
	return d, nil
}

// RemoveDownload cancels (or unseeds) a download by info_hash and
// drops it from downloads.json so it doesn't come back on restart.
// Files on disk are NOT deleted; pruneLoop handles that case when
// the user removes them via Finder.
func (s *Service) RemoveDownload(infoHash string) bool {
	c := s.Client()
	removed := false
	if c != nil {
		removed = c.RemoveDownload(infoHash)
	}
	saved := s.loadSavedDownloads()
	kept := saved[:0]
	for _, sv := range saved {
		if sv.InfoHash == infoHash {
			continue
		}
		kept = append(kept, sv)
	}
	if len(kept) != len(saved) {
		if err := s.saveDownloads(kept); err != nil {
			clog.Warn("downloads", "persist after remove: %v", err)
		}
	}
	return removed
}

// metaVerdict is what to do with a download still fetching metadata,
// decided from its source peers' online state + published catalogs.
type metaVerdict int

const (
	metaWait     metaVerdict = iota // online source still lists it (or not polled yet) — keep fetching
	metaUnshared                    // online source no longer lists it — gone, remove
	metaOffline                     // no source online — keep pending, resumes when it returns
)

// metadataVerdict maps a download's source addresses to online peers
// and inspects their last-polled file catalogs:
//   - any online source still lists this info hash      → metaWait
//   - online source(s) present, catalog known, none list it → metaUnshared
//   - online source(s) present but catalog not polled yet   → metaWait (give it time)
//   - no source online                                   → metaOffline
func (s *Service) metadataVerdict(d *internaltorrent.Download) metaVerdict {
	onlinePub := map[string]string{} // overlay v4 host → pub, online peers only
	for _, p := range s.eng.OnlinePeersForCAPoll() {
		if p.Host != "" {
			onlinePub[p.Host] = p.Pub
		}
	}
	var anyOnline, anyShares, anyCatalog bool
	s.peerMu.RLock()
	defer s.peerMu.RUnlock()
	for _, addr := range d.Peers {
		host := addr
		if h, _, err := net.SplitHostPort(addr); err == nil {
			host = h
		}
		pub, ok := onlinePub[host]
		if !ok || pub == "" {
			continue // this source is offline
		}
		anyOnline = true
		files, polled := s.peerFiles[pub]
		if !polled {
			continue // online but we haven't polled its catalog yet
		}
		anyCatalog = true
		for _, f := range files {
			if f.InfoHash == d.InfoHash {
				anyShares = true
			}
		}
	}
	switch {
	case !anyOnline:
		return metaOffline
	case anyShares:
		return metaWait
	case anyCatalog:
		return metaUnshared // online, catalog known, file not in it → un-shared
	default:
		return metaWait // online but not polled yet
	}
}

// SourceOnline reports whether a download (by info hash) has at least
// one source peer online right now. Surfaced in the UI so a
// metadata-stuck download reads as "source offline" instead of a
// perpetual "fetching…". Unknown hash → false.
func (s *Service) SourceOnline(infoHash string) bool {
	c := s.Client()
	if c == nil {
		return false
	}
	onlinePub := map[string]struct{}{}
	for _, p := range s.eng.OnlinePeersForCAPoll() {
		if p.Host != "" {
			onlinePub[p.Host] = struct{}{}
		}
	}
	for _, d := range c.ListDownloads() {
		if d.InfoHash != infoHash {
			continue
		}
		for _, addr := range d.Peers {
			host := addr
			if h, _, err := net.SplitHostPort(addr); err == nil {
				host = h
			}
			if _, ok := onlinePub[host]; ok {
				return true
			}
		}
	}
	return false
}

// PeerFiles returns the cached list of files published by one peer
// (empty before the first successful poll).
func (s *Service) PeerFiles(pub string) []PeerFile {
	s.peerMu.RLock()
	defer s.peerMu.RUnlock()
	list, ok := s.peerFiles[pub]
	if !ok {
		return nil
	}
	out := make([]PeerFile, len(list))
	copy(out, list)
	return out
}

// PollPeers blocks until ctx is done, walking every online peer
// every 15s and pulling their file catalog over the bus. First poll
// after 7s (let the camp poller discover peers). Resilient to engine
// being down. The cadence also bounds how fast we notice a peer
// un-sharing a file (→ a stuck download is dropped as a zombie) and
// how fast a peer's newly-shared files show up in the camp library —
// cheap over QUIC, so kept tighter than the old 60s.
func (s *Service) PollPeers(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(7 * time.Second):
	}
	s.pollOnce(ctx)
	ticker := time.NewTicker(15 * time.Second)
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
	if s.bus == nil {
		return
	}
	targets := s.eng.OnlinePeersForCAPoll()
	if len(targets) == 0 {
		return
	}
	for _, t := range targets {
		if t.Pub == "" {
			continue
		}
		files, err := s.fetchPeerFiles(ctx, t.Pub)
		if err != nil {
			continue
		}
		s.peerMu.Lock()
		s.peerFiles[t.Pub] = files
		s.peerMu.Unlock()
	}
}

// fetchPeerFiles pulls one peer's published file list over the bus.
func (s *Service) fetchPeerFiles(ctx context.Context, pub string) ([]PeerFile, error) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := s.bus.Request(rctx, pub, busTypeFiles, nil)
	if err != nil {
		return nil, err
	}
	var files []PeerFile
	if err := json.Unmarshal(raw, &files); err != nil {
		return nil, err
	}
	return files, nil
}

// --- persistence ---

type savedDownload struct {
	Magnet   string   `json:"magnet"`
	InfoHash string   `json:"info_hash"`
	Peers    []string `json:"peers,omitempty"`
}

// ShareToScope seeds a file and pins it to a conversation (channel id or dm
// key) so it stays out of the public catalog. Returns the seed's peer-facing
// metadata (name/size/info_hash/magnet) for embedding in a chat message.
func (s *Service) ShareToScope(path, scope string) (PeerFile, error) {
	c := s.Client()
	if c == nil {
		return PeerFile{}, fmt.Errorf("drop: torrent client not running")
	}
	h, err := c.AddSeed(path)
	if err != nil {
		return PeerFile{}, err
	}
	s.scopeMu.Lock()
	s.scopes[h.InfoHash] = scope
	s.scopeMu.Unlock()
	s.saveScopes()
	return PeerFile{Name: h.Name, Size: h.Size, InfoHash: h.InfoHash, Magnet: h.Magnet}, nil
}

// scopeOf returns the conversation a seeded file is pinned to ("" = public).
func (s *Service) scopeOf(infoHash string) string {
	s.scopeMu.RLock()
	defer s.scopeMu.RUnlock()
	return s.scopes[infoHash]
}

func (s *Service) scopesStatePath() string {
	return filepath.Join(s.campDir(s.campID), "file-scopes.json")
}

func (s *Service) loadScopes() {
	data, err := os.ReadFile(s.scopesStatePath())
	if err != nil {
		return
	}
	m := map[string]string{}
	if err := json.Unmarshal(data, &m); err != nil {
		clog.Warn("torrent", "parse scopes: %v", err)
		return
	}
	s.scopeMu.Lock()
	s.scopes = m
	s.scopeMu.Unlock()
}

func (s *Service) saveScopes() {
	s.scopeMu.RLock()
	data, err := json.MarshalIndent(s.scopes, "", "  ")
	s.scopeMu.RUnlock()
	if err != nil {
		return
	}
	path := s.scopesStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		clog.Warn("torrent", "save scopes: %v", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		clog.Warn("torrent", "save scopes: %v", err)
	}
}

func (s *Service) downloadsStatePath() string {
	return filepath.Join(s.campDir(s.campID), "downloads.json")
}

func (s *Service) loadSavedDownloads() []savedDownload {
	path := s.downloadsStatePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []savedDownload
	if err := json.Unmarshal(data, &out); err != nil {
		clog.Warn("downloads", "parse %s: %v", path, err)
		return nil
	}
	return out
}

func (s *Service) saveDownloads(list []savedDownload) error {
	path := s.downloadsStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (s *Service) restoreDownloads(c *internaltorrent.Client) {
	saved := s.loadSavedDownloads()
	if len(saved) == 0 {
		return
	}
	added := 0
	for _, sv := range saved {
		if _, err := c.AddDownload(sv.Magnet, sv.Peers); err != nil {
			clog.Warn("downloads", "restore %s: %v", sv.InfoHash, err)
			continue
		}
		added++
	}
	if added > 0 {
		clog.Info("downloads", "restored %d previously-downloaded torrent(s)", added)
	}
}

// --- background goroutines ---

func (s *Service) rescanSharedDir(c *internaltorrent.Client, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		clog.Warn("torrent", "rescan %s: %v", dir, err)
		return
	}
	added := 0
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		h, err := c.AddSeed(path)
		if err != nil {
			clog.Warn("torrent", "rescan %s: %v", name, err)
			continue
		}
		added++
		clog.Info("torrent", "rescan re-seeded %s (%d bytes, info_hash=%s)", name, h.Size, h.InfoHash)
	}
	if added > 0 {
		clog.Info("torrent", "rescan re-seeded %d file(s) from %s", added, dir)
	}
}

func (s *Service) chownLoop(dirs ...string) {
	for _, d := range dirs {
		chownToUser(d)
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		for _, d := range dirs {
			chownToUser(d)
		}
	}
}

func (s *Service) pruneLoop(c *internaltorrent.Client) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	s.pruneOnce(c)
	for range ticker.C {
		s.pruneOnce(c)
		s.refeedActiveDownloads(c)
	}
}

func (s *Service) refeedActiveDownloads(c *internaltorrent.Client) {
	const stallAfter = 90 * time.Second
	now := time.Now()
	for _, d := range c.ListDownloads() {
		if d.Torrent == nil || d.Torrent.Info() == nil {
			// Still fetching metadata. Decide by the source's published
			// catalog, NOT by mere online/offline: an offline source is
			// just temporarily away (will reshare), but a source that's
			// online and no longer lists the file has genuinely
			// un-shared it — only then is the download a dead zombie.
			switch s.metadataVerdict(d) {
			case metaUnshared:
				clog.Info("downloads", "%s no longer shared by any online peer — removing", d.InfoHash)
				s.RemoveDownload(d.InfoHash)
			case metaWait:
				c.FeedPeers(d) // online source still offers it (or catalog not polled yet)
			case metaOffline:
				// source away — keep pending, resumes when it returns.
			}
			continue
		}
		total := d.Torrent.Info().TotalLength()
		done := d.Torrent.BytesCompleted()
		if total > 0 && done >= total {
			continue
		}
		if done > d.LastBytes {
			d.LastBytes = done
			d.LastProgressAt = now
		}
		stalled := now.Sub(d.LastProgressAt) > stallAfter
		if stalled && d.Magnet != "" {
			clog.Info("downloads", "%s stalled (%s with no progress) — drop+re-add",
				d.InfoHash, now.Sub(d.LastProgressAt).Round(time.Second))
			peers := append([]string(nil), d.Peers...)
			magnet := d.Magnet
			c.RemoveDownload(d.InfoHash)
			if _, err := c.AddDownload(magnet, peers); err != nil {
				clog.Warn("downloads", "stall recovery re-add %s: %v", d.InfoHash, err)
			}
			continue
		}
		c.FeedPeers(d)
	}
}

func (s *Service) pruneOnce(c *internaltorrent.Client) {
	removed := false
	saved := s.loadSavedDownloads()
	keep := saved[:0]
	for _, sv := range saved {
		var d *internaltorrent.Download
		for _, x := range c.ListDownloads() {
			if x.InfoHash == sv.InfoHash {
				d = x
				break
			}
		}
		if d == nil || d.Torrent == nil || d.Torrent.Info() == nil {
			keep = append(keep, sv)
			continue
		}
		total := d.Torrent.Info().TotalLength()
		complete := total > 0 && d.Torrent.BytesCompleted() >= total
		if !complete {
			keep = append(keep, sv)
			continue
		}
		path := c.DownloadPath(d)
		if path == "" {
			keep = append(keep, sv)
			continue
		}
		if _, err := os.Stat(path); err != nil {
			clog.Info("downloads", "file gone, dropping %s (%s)", sv.InfoHash, path)
			c.RemoveDownload(sv.InfoHash)
			removed = true
			continue
		}
		keep = append(keep, sv)
	}
	if removed {
		if err := s.saveDownloads(keep); err != nil {
			clog.Warn("downloads", "persist after prune: %v", err)
		}
	}
	for _, h := range c.ListSeeds() {
		if h.Path == "" {
			continue
		}
		if _, err := os.Stat(h.Path); err != nil {
			clog.Info("seeds", "file gone, dropping %s (%s)", h.InfoHash, h.Path)
			_ = c.RemoveSeed(h.InfoHash)
		}
	}
}

// --- root↔user helpers ---

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

func chownToUser(path string) {
	su := os.Getenv("SUDO_USER")
	if su == "" {
		return
	}
	u, err := user.Lookup(su)
	if err != nil {
		clog.Warn("chown", "lookup %s: %v", su, err)
		return
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	_ = filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if cerr := os.Lchown(p, uid, gid); cerr != nil {
			clog.Warn("chown", "%s: %v", p, cerr)
		}
		return nil
	})
}
