//go:build darwin

// Package torrent wraps anacrolix/torrent for f2f's BitTorrent-based
// file sharing across a camp.
//
// Configuration is intentionally restrictive: no DHT, no public
// trackers, no PEX, no UPnP. The only way peers discover each other
// is via /api/files on the tunnel listener — discovery is f2f's job,
// transport is BT's job.
//
// The client binds on <tunnel_ip>:6881 so other peers in our overlay
// can reach it through utun. Outgoing connections to peers also use
// the tunnel automatically since destinations are in 10.99.0.0/24.
package torrent

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

const (
	// Piece length for files we create: 1 MiB is a sane default
	// (anacrolix default is similar). Smaller = more overhead, bigger
	// = slower start. We can revisit if needed.
	pieceLength = 1 << 20

	// Default port; engine can override via NewOptions.
	DefaultPort = 6881
)

// Options configures a Client. All fields except ListenAddr have
// sensible defaults.
type Options struct {
	// ListenAddr is host:port to bind on, e.g. "10.99.0.2:6881". For
	// f2f this is <tunnel_ip>:6881 — only reachable through utun.
	ListenAddr string
	// SharedDir is the catalog where seeded files live. The torrent
	// client uses filepath-based storage; files at this path get
	// seeded.
	SharedDir string
	// DownloadsDir is where completed downloads land. anacrolix
	// writes incoming pieces directly into this directory under each
	// torrent's name.
	DownloadsDir string
}

// Client wraps the underlying anacrolix client + bookkeeping for our
// own torrents and discovered ones.
type Client struct {
	opts     Options
	atc      *atorrent.Client
	mu       sync.Mutex
	seeding  map[string]*SeedHandle // info_hash hex → seed entry
	loading  map[string]*Download   // info_hash hex → in-flight download
}

// SeedHandle tracks one file we're sharing.
type SeedHandle struct {
	InfoHash string
	Name     string
	Size     int64
	Magnet   string
	Path     string // absolute path of file on disk
	Torrent  *atorrent.Torrent
}

// Download tracks one in-flight (or completed) incoming transfer.
// Peers is the list we feed to anacrolix on AddDownload and re-feed
// periodically while the torrent isn't complete — anacrolix sometimes
// fails the initial dial (peer busy with another torrent) and won't
// retry on its own without DHT/PEX/trackers, which we've disabled.
// Magnet keeps the original URI so the engine can drop+re-add the
// torrent to recover from a stall (peer restarted mid-download,
// anacrolix won't aggressively re-dial without trackers).
type Download struct {
	InfoHash       string
	Name           string
	Size           int64
	Torrent        *atorrent.Torrent
	StartedAt      time.Time
	Peers          []string
	Magnet         string
	LastProgressAt time.Time
	LastBytes      int64
}

// New brings up a fresh BT client on the configured ListenAddr.
func New(opts Options) (*Client, error) {
	if err := os.MkdirAll(opts.SharedDir, 0o755); err != nil {
		return nil, fmt.Errorf("torrent: mkdir shared: %w", err)
	}
	if err := os.MkdirAll(opts.DownloadsDir, 0o755); err != nil {
		return nil, fmt.Errorf("torrent: mkdir downloads: %w", err)
	}

	cfg := atorrent.NewDefaultClientConfig()
	cfg.DataDir = opts.DownloadsDir
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.DisablePEX = true
	cfg.NoUpload = false
	cfg.Seed = true
	cfg.DefaultStorage = storage.NewFile(opts.DownloadsDir)

	// Anacrolix opens listeners on BOTH tcp4 and tcp6 with the same
	// host string — passing a v6 host would fail tcp4 bind and vice
	// versa. Force the family that matches the host: v6 host → disable
	// v4, v4 host → disable v6.
	host, port, err := splitHostPort(opts.ListenAddr)
	if err != nil {
		return nil, err
	}
	if strings.Contains(host, ":") {
		cfg.DisableIPv4 = true
		cfg.DisableIPv6 = false
	} else {
		cfg.DisableIPv4 = false
		cfg.DisableIPv6 = true
	}
	cfg.SetListenAddr(net.JoinHostPort(host, port))

	atc, err := atorrent.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("torrent: client: %w", err)
	}

	return &Client{
		opts:    opts,
		atc:     atc,
		seeding: map[string]*SeedHandle{},
		loading: map[string]*Download{},
	}, nil
}

// Close stops the BT client. Idempotent.
func (c *Client) Close() error {
	if c == nil || c.atc == nil {
		return nil
	}
	c.atc.Close()
	return nil
}

// AddSeed registers a local file for seeding. Returns the handle so
// callers can expose the magnet URI / info_hash. Idempotent —
// re-seeding the same path returns the cached handle.
func (c *Client) AddSeed(path string) (*SeedHandle, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("torrent: stat %s: %w", abs, err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("torrent: %s is a directory (multi-file torrents not implemented)", abs)
	}

	// Build metainfo from the file. anacrolix has metainfo helpers for
	// single-file torrents.
	info := metainfo.Info{
		PieceLength: pieceLength,
		Name:        filepath.Base(abs),
		Length:      fi.Size(),
	}
	if err := info.BuildFromFilePath(abs); err != nil {
		return nil, fmt.Errorf("torrent: build info: %w", err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		return nil, fmt.Errorf("torrent: marshal info: %w", err)
	}
	mi := &metainfo.MetaInfo{
		InfoBytes: infoBytes,
	}
	ih := mi.HashInfoBytes()
	ihHex := ih.HexString()

	c.mu.Lock()
	if h, ok := c.seeding[ihHex]; ok {
		c.mu.Unlock()
		return h, nil
	}
	c.mu.Unlock()

	// AddTorrent expects the file to live in cfg.DataDir/<name>. To
	// avoid duplicating large files, we'll use a custom file-storage
	// rooted at the file's parent dir.
	parent := filepath.Dir(abs)
	t, _ := c.atc.AddTorrentOpt(atorrent.AddTorrentOpts{
		InfoHash: ih,
		Storage:  storage.NewFile(parent),
	})
	if err := t.SetInfoBytes(mi.InfoBytes); err != nil {
		return nil, fmt.Errorf("torrent: set info: %w", err)
	}
	t.AllowDataUpload()
	t.DownloadAll() // ensures storage knows we have all pieces

	h := &SeedHandle{
		InfoHash: ihHex,
		Name:     info.Name,
		Size:     info.TotalLength(),
		Magnet:   mi.Magnet(&ih, &info).String(),
		Path:     abs,
		Torrent:  t,
	}
	c.mu.Lock()
	c.seeding[ihHex] = h
	c.mu.Unlock()
	return h, nil
}

// RemoveSeed drops a torrent we were seeding. The underlying file on
// disk is NOT deleted.
func (c *Client) RemoveSeed(infoHashHex string) error {
	c.mu.Lock()
	h, ok := c.seeding[infoHashHex]
	if ok {
		delete(c.seeding, infoHashHex)
	}
	c.mu.Unlock()
	if !ok {
		return nil
	}
	h.Torrent.Drop()
	return nil
}

// ListSeeds returns a copy of the active seed list, sorted by name
// (then info_hash) so callers get stable ordering across calls.
func (c *Client) ListSeeds() []*SeedHandle {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*SeedHandle, 0, len(c.seeding))
	for _, h := range c.seeding {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].InfoHash < out[j].InfoHash
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// AddDownload starts pulling a torrent identified by infoHash. Initial
// peers are provided by the caller (we never use DHT/trackers). The
// list is also stashed on the Download struct so the engine can
// re-feed it periodically — anacrolix sometimes fails the initial
// dial (peer busy with another torrent we're sharing) and without
// DHT/PEX/trackers it has no way to find the same peer again.
func (c *Client) AddDownload(magnetOrHash string, peerAddrs []string) (*Download, error) {
	t, err := c.atc.AddMagnet(magnetOrHash)
	if err != nil {
		return nil, fmt.Errorf("torrent: add magnet: %w", err)
	}
	feedPeers(t, peerAddrs)
	now := time.Now()
	d := &Download{
		InfoHash:       t.InfoHash().HexString(),
		Torrent:        t,
		StartedAt:      now,
		Peers:          append([]string(nil), peerAddrs...),
		Magnet:         magnetOrHash,
		LastProgressAt: now,
	}
	c.mu.Lock()
	c.loading[d.InfoHash] = d
	c.mu.Unlock()
	// Start downloading in the background once metadata arrives.
	go func() {
		<-t.GotInfo()
		c.mu.Lock()
		if existing, ok := c.loading[d.InfoHash]; ok {
			existing.Name = t.Info().Name
			existing.Size = t.Info().TotalLength()
		}
		c.mu.Unlock()
		t.DownloadAll()
	}()
	return d, nil
}

// FeedPeers re-adds the recorded peer list to the torrent. Called
// from the engine's refresh loop while a download is in flight.
func (c *Client) FeedPeers(d *Download) {
	if d == nil || d.Torrent == nil || len(d.Peers) == 0 {
		return
	}
	feedPeers(d.Torrent, d.Peers)
}

func feedPeers(t *atorrent.Torrent, peerAddrs []string) {
	if t == nil || len(peerAddrs) == 0 {
		return
	}
	addPeers := make([]atorrent.PeerInfo, 0, len(peerAddrs))
	for _, a := range peerAddrs {
		addPeers = append(addPeers, atorrent.PeerInfo{
			Addr:   strAddr(a),
			Source: atorrent.PeerSourceDirect,
		})
	}
	t.AddPeers(addPeers)
}

// ListDownloads returns a copy of all known downloads (in progress +
// done), sorted by start time so newest stays at the bottom.
func (c *Client) ListDownloads() []*Download {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*Download, 0, len(c.loading))
	for _, d := range c.loading {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].InfoHash < out[j].InfoHash
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

// DownloadPath returns the absolute on-disk path of a completed
// download's primary file. Empty string if the torrent has no info
// yet (metadata-fetch still in flight) or no files.
func (c *Client) DownloadPath(d *Download) string {
	if d == nil || d.Torrent == nil || d.Torrent.Info() == nil {
		return ""
	}
	files := d.Torrent.Files()
	if len(files) == 0 {
		return ""
	}
	return filepath.Join(c.opts.DownloadsDir, files[0].Path())
}

// RemoveDownload drops a torrent we were downloading/seeding and
// forgets it. Used by the engine's prune loop when the user has
// deleted the file from disk — keeps the in-memory and UI state in
// sync with reality. Returns true if an entry was actually removed.
func (c *Client) RemoveDownload(infoHashHex string) bool {
	c.mu.Lock()
	d, ok := c.loading[infoHashHex]
	if ok {
		delete(c.loading, infoHashHex)
	}
	c.mu.Unlock()
	if !ok {
		return false
	}
	if d.Torrent != nil {
		d.Torrent.Drop()
	}
	return true
}

// SharedDir returns the directory where seeded files live. UI uploads
// drop files here.
func (c *Client) SharedDir() string { return c.opts.SharedDir }

// ---- helpers ----

// strAddr lets us pass "host:port" strings as net.Addr to anacrolix
// without dialing right away — it does the dial itself.
type strAddr string

func (s strAddr) Network() string { return "tcp" }
func (s strAddr) String() string  { return string(s) }

func splitHostPort(addr string) (string, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", "", fmt.Errorf("torrent: bad listen addr %q: %w", addr, err)
	}
	if !strings.Contains(host, ":") && host == "" {
		host = "0.0.0.0"
	}
	return host, port, nil
}
