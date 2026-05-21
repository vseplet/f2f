package lite

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	internaltorrent "github.com/vseplet/f2f/source/desktop/internal/torrent"
)

// statePrefix marks UDP packets that carry the peer's "state envelope"
// — JSON with files catalog and anything else worth broadcasting on
// a regular cadence. Distinct from 0xF2 (signal) so the receiver can
// route without parsing JSON. Single-byte hole-punch stays as 0xFF.
const statePrefix byte = 0xF3

// PeerFile mirrors the mac engine's shape so the drop-tab UI can be
// ported one-to-one. It's what a peer broadcasts about each seed.
type PeerFile struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	InfoHash string `json:"info_hash"`
	Magnet   string `json:"magnet"`
}

// stateEnvelope is the JSON payload of a 0xF3 packet. Everything peers
// need to know about each other lives here; broadcast every ~30s.
type stateEnvelope struct {
	Name  string     `json:"name"`
	Files []PeerFile `json:"files,omitempty"`
}

// SeededFile describes a file we are sharing — what the UI reads for
// the "my shared files" section.
type SeededFile struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	InfoHash string `json:"info_hash"`
	Magnet   string `json:"magnet"`
	Path     string `json:"path"`
}

// DownloadInfo is one row in the active-downloads / library state.
type DownloadInfo struct {
	InfoHash       string `json:"info_hash"`
	Name           string `json:"name"`
	Size           int64  `json:"size"`
	BytesCompleted int64  `json:"bytes_completed"`
	BytesMissing   int64  `json:"bytes_missing"`
	Complete       bool   `json:"complete"`
	Seeding        bool   `json:"seeding"`
	Path           string `json:"path,omitempty"`
	StartedAt      int64  `json:"started_at"`
}

// PeerLibraryEntry is a flat row in the camp-library list: one peer's
// one file. The UI groups by peer if it wants.
type PeerLibraryEntry struct {
	Peer       string `json:"peer"`
	PeerTunnel string `json:"peer_tunnel"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	InfoHash   string `json:"info_hash"`
	Magnet     string `json:"magnet"`
}

// torrentEnabled reports whether the BT client is up and ready to
// accept calls. Callers in app.go return ServiceUnavailable when not.
func (c *Client) torrentEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.torrent != nil
}

// startTorrent boots the BT client bound to 0.0.0.0:<ephemeral>. The
// lite client has no tunnel_ip, so we use any free port and tell peers
// about it via the state envelope when they need to dial back.
//
// Runs in a goroutine: anacrolix.NewClient can be slow to initialise
// (storage setup, listener bind, internal state). Blocking Start
// would freeze the UI in "starting…" for several seconds.
func (c *Client) startTorrent() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("torrent: PANIC during startup: %v (file sharing disabled)", r)
		}
	}()
	opts := internaltorrent.Options{
		ListenAddr:   "0.0.0.0:0",
		SharedDir:    c.torrentSharedDir(),
		DownloadsDir: c.torrentDownloadsDir(),
	}
	log.Printf("torrent: initialising client …")
	t0 := time.Now()
	tc, err := internaltorrent.New(opts)
	if err != nil {
		log.Printf("torrent: %v (file sharing disabled)", err)
		return
	}
	c.mu.Lock()
	c.torrent = tc
	c.mu.Unlock()
	log.Printf("torrent: ready in %v (shared=%s downloads=%s)",
		time.Since(t0).Round(time.Millisecond), opts.SharedDir, opts.DownloadsDir)

	// Re-seed shared/ + replay saved downloads in the background.
	go c.rescanSharedDir(tc, opts.SharedDir)
	go c.restoreDownloads(tc)
}

func (c *Client) stopTorrent() {
	c.mu.Lock()
	tc := c.torrent
	c.torrent = nil
	c.mu.Unlock()
	if tc != nil {
		_ = tc.Close()
	}
}

// ----- shared/downloads paths -----

func (c *Client) torrentSharedDir() string {
	return filepath.Join(userHome(), "Library", "Application Support", "f2f-desktop", "shared")
}
func (c *Client) torrentDownloadsDir() string {
	return filepath.Join(userHome(), "Downloads", "f2f-drops")
}

func userHome() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/tmp"
}

// ----- API exposed to app.go (Wails bindings) -----

func (c *Client) MyFiles() []SeededFile {
	c.mu.Lock()
	tc := c.torrent
	c.mu.Unlock()
	if tc == nil {
		return []SeededFile{}
	}
	seeds := tc.ListSeeds()
	out := make([]SeededFile, 0, len(seeds))
	for _, h := range seeds {
		out = append(out, SeededFile{
			Name: h.Name, Size: h.Size, InfoHash: h.InfoHash,
			Magnet: h.Magnet, Path: h.Path,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].InfoHash < out[j].InfoHash
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// AddSeedFromPath registers a local file for sharing. The file is
// copied into the shared directory first, so removing the original
// later doesn't break the seed.
func (c *Client) AddSeedFromPath(srcPath string) (*SeededFile, error) {
	c.mu.Lock()
	tc := c.torrent
	c.mu.Unlock()
	if tc == nil {
		return nil, fmt.Errorf("torrent client not running")
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return nil, fmt.Errorf("open source: %w", err)
	}
	defer src.Close()
	if err := os.MkdirAll(tc.SharedDir(), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir shared: %w", err)
	}
	dstPath := filepath.Join(tc.SharedDir(), filepath.Base(srcPath))
	dst, err := os.Create(dstPath)
	if err != nil {
		return nil, fmt.Errorf("create dst: %w", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		_ = os.Remove(dstPath)
		return nil, fmt.Errorf("copy: %w", err)
	}
	dst.Close()
	h, err := tc.AddSeed(dstPath)
	if err != nil {
		return nil, err
	}
	return &SeededFile{
		Name: h.Name, Size: h.Size, InfoHash: h.InfoHash,
		Magnet: h.Magnet, Path: h.Path,
	}, nil
}

// AddSeedBytes is the path used by drag-drop in the browser: the JS
// reads the file bytes and ships them across the Wails binding rather
// than handing over a real filesystem path.
func (c *Client) AddSeedBytes(name string, data []byte) (*SeededFile, error) {
	c.mu.Lock()
	tc := c.torrent
	c.mu.Unlock()
	if tc == nil {
		return nil, fmt.Errorf("torrent client not running")
	}
	if err := os.MkdirAll(tc.SharedDir(), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir shared: %w", err)
	}
	dstPath := filepath.Join(tc.SharedDir(), filepath.Base(name))
	if err := os.WriteFile(dstPath, data, 0o644); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	h, err := tc.AddSeed(dstPath)
	if err != nil {
		return nil, err
	}
	return &SeededFile{
		Name: h.Name, Size: h.Size, InfoHash: h.InfoHash,
		Magnet: h.Magnet, Path: h.Path,
	}, nil
}

func (c *Client) RemoveSeed(infoHashHex string) error {
	c.mu.Lock()
	tc := c.torrent
	c.mu.Unlock()
	if tc == nil {
		return fmt.Errorf("torrent client not running")
	}
	return tc.RemoveSeed(infoHashHex)
}

// AddDownload wraps the BT client's AddDownload, persists the entry
// to downloads.json so it survives restart, and is idempotent against
// re-adding the same info_hash.
func (c *Client) AddDownload(magnet string, peers []string) (*DownloadInfo, error) {
	c.mu.Lock()
	tc := c.torrent
	c.mu.Unlock()
	if tc == nil {
		return nil, fmt.Errorf("torrent client not running")
	}
	d, err := tc.AddDownload(magnet, peers)
	if err != nil {
		return nil, err
	}
	saved := loadSavedDownloads()
	dup := false
	for _, s := range saved {
		if s.InfoHash == d.InfoHash {
			dup = true
			break
		}
	}
	if !dup {
		saved = append(saved, savedDownload{Magnet: magnet, InfoHash: d.InfoHash, Peers: peers})
		if err := saveDownloads(saved); err != nil {
			log.Printf("downloads: persist: %v", err)
		}
	}
	return c.buildDownloadInfo(tc, d), nil
}

func (c *Client) ListDownloads() []DownloadInfo {
	c.mu.Lock()
	tc := c.torrent
	c.mu.Unlock()
	if tc == nil {
		return []DownloadInfo{}
	}
	in := tc.ListDownloads()
	out := make([]DownloadInfo, 0, len(in))
	for _, d := range in {
		out = append(out, *c.buildDownloadInfo(tc, d))
	}
	return out
}

func (c *Client) buildDownloadInfo(tc *internaltorrent.Client, d *internaltorrent.Download) *DownloadInfo {
	info := &DownloadInfo{
		InfoHash:  d.InfoHash,
		Name:      d.Name,
		Size:      d.Size,
		StartedAt: d.StartedAt.Unix(),
	}
	if d.Torrent != nil && d.Torrent.Info() != nil {
		total := d.Torrent.Info().TotalLength()
		done := d.Torrent.BytesCompleted()
		info.BytesCompleted = done
		info.BytesMissing = total - done
		info.Complete = total > 0 && done >= total
		info.Seeding = info.Complete && d.Torrent.Seeding()
		if info.Complete {
			info.Path = tc.DownloadPath(d)
		}
	}
	return info
}

// RevealFile opens Finder with the given file selected. Restricted to
// paths inside SharedDir or DownloadsDir so the binding can't be
// abused as a generic "open anything" backdoor.
func (c *Client) RevealFile(path string) error {
	c.mu.Lock()
	tc := c.torrent
	c.mu.Unlock()
	if tc == nil {
		return fmt.Errorf("torrent client not running")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	shared, _ := filepath.Abs(tc.SharedDir())
	downloads, _ := filepath.Abs(c.torrentDownloadsDir())
	if !strings.HasPrefix(abs, shared+string(filepath.Separator)) &&
		!strings.HasPrefix(abs, downloads+string(filepath.Separator)) {
		return fmt.Errorf("path outside f2f-managed dirs")
	}
	if _, err := os.Stat(abs); err != nil {
		return err
	}
	return revealInFinder(abs)
}

// ----- catalog broadcast -----

// MyState builds the envelope this peer sends to others. Currently
// just identity + files; room to grow.
func (c *Client) myState() stateEnvelope {
	return stateEnvelope{
		Name:  c.cfg.Name,
		Files: c.myFilesForBroadcast(),
	}
}

func (c *Client) myFilesForBroadcast() []PeerFile {
	c.mu.Lock()
	tc := c.torrent
	c.mu.Unlock()
	if tc == nil {
		return nil
	}
	seeds := tc.ListSeeds()
	out := make([]PeerFile, 0, len(seeds))
	for _, h := range seeds {
		out = append(out, PeerFile{
			Name: h.Name, Size: h.Size,
			InfoHash: h.InfoHash, Magnet: h.Magnet,
		})
	}
	return out
}

// stateBroadcastLoop ticks every 20 seconds and sends our current
// state envelope to every known peer. Smaller cadence than peer-list
// poll so a new peer learns about us shortly after joining.
func (c *Client) stateBroadcastLoop(ctx context.Context) {
	defer c.workers.Done()
	// Send once almost immediately so a freshly-started peer sees us
	// quickly. Then keep the periodic cadence.
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.broadcastState()
		t.Reset(20 * time.Second)
	}
}

func (c *Client) broadcastState() {
	st := c.myState()
	body, err := json.Marshal(st)
	if err != nil {
		log.Printf("state: marshal: %v", err)
		return
	}
	if len(body) > 60000 {
		// Single UDP packet limit is ~64KiB, but real-world MTU is
		// much smaller (~1400 bytes payload). Above ~60KB we're
		// almost guaranteed to be dropped by something. Truncate file
		// list rather than send a packet that vanishes.
		log.Printf("state: payload %d bytes — truncating files", len(body))
		st.Files = st.Files[:len(st.Files)/2]
		body, _ = json.Marshal(st)
	}
	pkt := make([]byte, 1+len(body))
	pkt[0] = statePrefix
	copy(pkt[1:], body)
	c.mu.Lock()
	udp := c.udp
	addrs := make([]*net.UDPAddr, 0, len(c.peers))
	for _, p := range c.peers {
		if p.udpAddr != nil {
			addrs = append(addrs, p.udpAddr)
		}
	}
	c.mu.Unlock()
	if udp == nil {
		return
	}
	for _, a := range addrs {
		if _, err := udp.WriteToUDP(pkt, a); err != nil {
			log.Printf("state: send to %s: %v", a, err)
		}
	}
}

// handleStatePacket caches the peer's broadcast envelope. UI reads
// from peerFiles via Library().
func (c *Client) handleStatePacket(from *peer, body []byte) {
	var st stateEnvelope
	if err := json.Unmarshal(body, &st); err != nil {
		log.Printf("state: parse from %s: %v", from.info.TunnelIP, err)
		return
	}
	c.mu.Lock()
	from.files = st.Files
	c.mu.Unlock()
}

// Library returns a flat list of every peer file we know about. The
// drop-tab UI groups by peer and shows download buttons.
func (c *Client) Library() []PeerLibraryEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []PeerLibraryEntry{}
	for _, p := range c.peers {
		for _, f := range p.files {
			out = append(out, PeerLibraryEntry{
				Peer:       p.info.Name,
				PeerTunnel: p.info.TunnelIP,
				Name:       f.Name,
				Size:       f.Size,
				InfoHash:   f.InfoHash,
				Magnet:     f.Magnet,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Peer == out[j].Peer {
			return out[i].Name < out[j].Name
		}
		return out[i].Peer < out[j].Peer
	})
	return out
}

// ----- shared/downloads persistence -----

func (c *Client) rescanSharedDir(tc *internaltorrent.Client, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("torrent: rescan %s: %v", dir, err)
		return
	}
	added := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		h, err := tc.AddSeed(path)
		if err != nil {
			log.Printf("torrent: rescan %s: %v", name, err)
			continue
		}
		added++
		log.Printf("torrent: re-seeded %s (%d bytes, info_hash=%s)", name, h.Size, h.InfoHash)
	}
	if added > 0 {
		log.Printf("torrent: rescan re-seeded %d file(s)", added)
	}
}

func (c *Client) restoreDownloads(tc *internaltorrent.Client) {
	saved := loadSavedDownloads()
	if len(saved) == 0 {
		return
	}
	added := 0
	for _, s := range saved {
		if _, err := tc.AddDownload(s.Magnet, s.Peers); err != nil {
			log.Printf("downloads: restore %s: %v", s.InfoHash, err)
			continue
		}
		added++
	}
	if added > 0 {
		log.Printf("downloads: restored %d previously-downloaded torrent(s)", added)
	}
}

type savedDownload struct {
	Magnet   string   `json:"magnet"`
	InfoHash string   `json:"info_hash"`
	Peers    []string `json:"peers,omitempty"`
}

func downloadsStatePath() string {
	return filepath.Join(userHome(), "Library", "Application Support", "f2f-desktop", "downloads.json")
}

func loadSavedDownloads() []savedDownload {
	data, err := os.ReadFile(downloadsStatePath())
	if err != nil {
		return nil
	}
	var out []savedDownload
	if err := json.Unmarshal(data, &out); err != nil {
		log.Printf("downloads: parse: %v", err)
		return nil
	}
	return out
}

func saveDownloads(list []savedDownload) error {
	path := downloadsStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// ----- prune loop -----

func (c *Client) pruneLoop(ctx context.Context) {
	defer c.workers.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	c.pruneOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		c.pruneOnce()
		c.refeedActiveDownloads()
	}
}

func (c *Client) pruneOnce() {
	c.mu.Lock()
	tc := c.torrent
	c.mu.Unlock()
	if tc == nil {
		return
	}
	removed := false
	saved := loadSavedDownloads()
	keep := saved[:0]
	for _, s := range saved {
		var d *internaltorrent.Download
		for _, x := range tc.ListDownloads() {
			if x.InfoHash == s.InfoHash {
				d = x
				break
			}
		}
		if d == nil || d.Torrent == nil || d.Torrent.Info() == nil {
			keep = append(keep, s)
			continue
		}
		total := d.Torrent.Info().TotalLength()
		if total <= 0 || d.Torrent.BytesCompleted() < total {
			keep = append(keep, s)
			continue
		}
		path := tc.DownloadPath(d)
		if path == "" {
			keep = append(keep, s)
			continue
		}
		if _, err := os.Stat(path); err != nil {
			log.Printf("downloads: file gone, dropping %s (%s)", s.InfoHash, path)
			tc.RemoveDownload(s.InfoHash)
			removed = true
			continue
		}
		keep = append(keep, s)
	}
	if removed {
		if err := saveDownloads(keep); err != nil {
			log.Printf("downloads: persist after prune: %v", err)
		}
	}
	for _, h := range tc.ListSeeds() {
		if h.Path == "" {
			continue
		}
		if _, err := os.Stat(h.Path); err != nil {
			log.Printf("seeds: file gone, dropping %s (%s)", h.InfoHash, h.Path)
			_ = tc.RemoveSeed(h.InfoHash)
		}
	}
}

const stallAfter = 90 * time.Second

func (c *Client) refeedActiveDownloads() {
	c.mu.Lock()
	tc := c.torrent
	c.mu.Unlock()
	if tc == nil {
		return
	}
	now := time.Now()
	for _, d := range tc.ListDownloads() {
		if d.Torrent == nil || d.Torrent.Info() == nil {
			tc.FeedPeers(d)
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
		if now.Sub(d.LastProgressAt) > stallAfter && d.Magnet != "" {
			log.Printf("downloads: %s stalled — drop+re-add", d.InfoHash)
			peers := append([]string(nil), d.Peers...)
			magnet := d.Magnet
			tc.RemoveDownload(d.InfoHash)
			if _, err := tc.AddDownload(magnet, peers); err != nil {
				log.Printf("downloads: stall re-add %s: %v", d.InfoHash, err)
			}
			continue
		}
		tc.FeedPeers(d)
	}
}

// ----- helpers -----

// revealInFinder runs `open -R <abs>` so the user sees the file
// selected in Finder. Errors are surfaced to the caller.
func revealInFinder(abs string) error {
	cmd := exec.Command("/usr/bin/open", "-R", abs)
	return cmd.Start()
}
