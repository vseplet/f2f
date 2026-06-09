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
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vseplet/f2f/source/helper/mesh/bus"
	"github.com/vseplet/f2f/source/helper/mesh/engine"
	internaltorrent "github.com/vseplet/f2f/source/helper/services/drop/torrent"
)

// busTypeFiles is the bus message type serving our seeded-file catalog
// to peers.
const busTypeFiles = "files"

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
}

// New constructs a Service. campDir resolves a camp's directory
// (config.Store.CampDir). The engine must outlive the service.
func New(eng *engine.Engine, campDir func(campID string) string, b *bus.Service) *Service {
	return &Service{
		eng:       eng,
		bus:       b,
		campDir:   campDir,
		peerFiles: map[string][]PeerFile{},
	}
}

// Register installs the bus handler that serves our seeded-file list
// to peers (Path never leaves the machine — PeerFile has no such
// field). Call once after construction.
func (s *Service) Register() {
	if s.bus == nil {
		return
	}
	s.bus.Handle(busTypeFiles, func(string, []byte) ([]byte, error) {
		c := s.Client()
		if c == nil {
			return nil, fmt.Errorf("torrent client not running")
		}
		out := []PeerFile{}
		for _, h := range c.ListSeeds() {
			out = append(out, PeerFile{
				Name: h.Name, Size: h.Size,
				InfoHash: h.InfoHash, Magnet: h.Magnet,
			})
		}
		return json.Marshal(out)
	})
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
	t0 := time.Now()
	opts := internaltorrent.Options{
		ListenAddr:   net.JoinHostPort(localIP, fmt.Sprint(internaltorrent.DefaultPort)),
		SharedDir:    s.sharedDir,
		DownloadsDir: s.downloads,
	}
	log.Printf("torrent: binding on %s …", opts.ListenAddr)
	c, err := internaltorrent.New(opts)
	if err != nil {
		return fmt.Errorf("drop: bind %s: %w", opts.ListenAddr, err)
	}
	s.client = c
	log.Printf("torrent: ready in %v (shared=%s downloads=%s)",
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
		log.Printf("downloads: persist: %v", err)
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
			log.Printf("downloads: persist after remove: %v", err)
		}
	}
	return removed
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
// every 60s and pulling /api/files. First poll after 7s (let the
// camp poller discover peers). Resilient to engine being down.
func (s *Service) PollPeers(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(7 * time.Second):
	}
	s.pollOnce(ctx)
	ticker := time.NewTicker(60 * time.Second)
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
		log.Printf("downloads: parse %s: %v", path, err)
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
			log.Printf("downloads: restore %s: %v", sv.InfoHash, err)
			continue
		}
		added++
	}
	if added > 0 {
		log.Printf("downloads: restored %d previously-downloaded torrent(s)", added)
	}
}

// --- background goroutines ---

func (s *Service) rescanSharedDir(c *internaltorrent.Client, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("torrent: rescan %s: %v", dir, err)
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
			log.Printf("torrent: rescan %s: %v", name, err)
			continue
		}
		added++
		log.Printf("torrent: rescan re-seeded %s (%d bytes, info_hash=%s)", name, h.Size, h.InfoHash)
	}
	if added > 0 {
		log.Printf("torrent: rescan re-seeded %d file(s) from %s", added, dir)
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
			c.FeedPeers(d)
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
			log.Printf("downloads: %s stalled (%s with no progress) — drop+re-add",
				d.InfoHash, now.Sub(d.LastProgressAt).Round(time.Second))
			peers := append([]string(nil), d.Peers...)
			magnet := d.Magnet
			c.RemoveDownload(d.InfoHash)
			if _, err := c.AddDownload(magnet, peers); err != nil {
				log.Printf("downloads: stall recovery re-add %s: %v", d.InfoHash, err)
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
			log.Printf("downloads: file gone, dropping %s (%s)", sv.InfoHash, path)
			c.RemoveDownload(sv.InfoHash)
			removed = true
			continue
		}
		keep = append(keep, sv)
	}
	if removed {
		if err := s.saveDownloads(keep); err != nil {
			log.Printf("downloads: persist after prune: %v", err)
		}
	}
	for _, h := range c.ListSeeds() {
		if h.Path == "" {
			continue
		}
		if _, err := os.Stat(h.Path); err != nil {
			log.Printf("seeds: file gone, dropping %s (%s)", h.InfoHash, h.Path)
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
		log.Printf("chown: lookup %s: %v", su, err)
		return
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	_ = filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if cerr := os.Lchown(p, uid, gid); cerr != nil {
			log.Printf("chown: %s: %v", p, cerr)
		}
		return nil
	})
}
