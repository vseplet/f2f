// Package drop is the user-facing service around the BitTorrent
// client used for camp file sharing. It owns:
//
//   - The anacrolix client lifecycle (Start binds it on the overlay
//     v4, Stop closes it).
//   - The on-disk catalog (uploads/ + downloads/, re-seeded across
//     restarts by rescanning the dirs + the block-referenced files).
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

	"slices"

	"github.com/vseplet/f2f/source/helper/clog"
	"github.com/vseplet/f2f/source/helper/db/blocks"
	"github.com/vseplet/f2f/source/helper/db/blocks/attach"
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
	blocks  *blocks.Manager            // source of "which file belongs to which scope"

	mu        sync.Mutex
	client    *internaltorrent.Client
	campID    string
	sharedDir string
	downloads string

	peerMu    sync.RWMutex
	peerFiles map[string][]PeerFile // pub → files

	// isMember reports whether a peer may see files scoped to a conversation.
	// Backed by the messenger (channel roster / dm pair). nil = nothing served.
	isMember func(scope, pub string) bool
}

// refFile is one torrent file referenced by a block: its info hash and magnet
// (for restore) plus the channel scopes it appears in (for access gating).
type refFile struct {
	Name     string
	InfoHash string
	Magnet   string
	Scopes   []string
}

// refFiles scans the blocks for every torrent attachment and returns, per
// info_hash, the file's magnet/name and the channel bids that reference it.
// This is the SOLE source of "which file belongs where" — no file-scopes.json,
// no public catalog. Pull-based (re-scanned on each use): OnApply doesn't fire
// for local writes, so an event index would miss our own shared files, and
// Blocks(scope) is incrementally cached so re-scans are cheap. Scope strings
// (message:/note:/channel:<bid>) map to the bid IsMember expects.
func (s *Service) refFiles() []refFile {
	if s.blocks == nil {
		return nil
	}
	byHash := map[string]*refFile{}
	for _, scope := range s.blocks.Scopes() {
		bid := scope
		for _, p := range []string{"message:", "note:", "channel:"} {
			if strings.HasPrefix(scope, p) {
				bid = strings.TrimPrefix(scope, p)
				break
			}
		}
		for _, blk := range s.blocks.Blocks(scope) {
			if blk == nil || blk.Deleted || len(blk.Heads) == 0 {
				continue
			}
			var c struct {
				attach.Attachment                    // file blocks: content.info_hash (flattened)
				File              *attach.Attachment `json:"file"` // messages: content.file.info_hash
			}
			if json.Unmarshal(blk.Heads[len(blk.Heads)-1].Content, &c) != nil {
				continue
			}
			for _, a := range []*attach.Attachment{&c.Attachment, c.File} {
				if a == nil || a.InfoHash == "" {
					continue
				}
				r := byHash[a.InfoHash]
				if r == nil {
					r = &refFile{Name: a.Name, InfoHash: a.InfoHash, Magnet: a.Magnet}
					byHash[a.InfoHash] = r
				}
				if !slices.Contains(r.Scopes, bid) {
					r.Scopes = append(r.Scopes, bid)
				}
			}
		}
	}
	out := make([]refFile, 0, len(byHash))
	for _, r := range byHash {
		out = append(out, *r)
	}
	return out
}

// SetMembershipCheck wires the conversation-membership predicate used to
// decide who may see a scoped (private) file in the catalog. Set once after
// construction (the messenger isn't built yet at drop.New time).
func (s *Service) SetMembershipCheck(fn func(scope, pub string) bool) {
	s.isMember = fn
}

// New constructs a Service. campDir resolves a camp's directory
// (config.Store.CampDir); blocksMgr is the block engine drop scans to learn
// which torrent files are referenced and in which scope. The engine must
// outlive the service.
func New(eng *engine.Engine, campDir func(campID string) string, b *bus.Service, blocksMgr *blocks.Manager) *Service {
	return &Service{
		eng:       eng,
		bus:       b,
		campDir:   campDir,
		blocks:    blocksMgr,
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
	filesHandler := func(fromPub string, _ []byte) ([]byte, error) {
		c := s.Client()
		if c == nil {
			return nil, fmt.Errorf("torrent client not running")
		}
		// A file is served only if it's referenced by a block in some scope the
		// asking peer is a member of. No public catalog: a seed no block points
		// to (or that the peer shares no scope with) is invisible to them.
		scopesByHash := map[string][]string{}
		for _, r := range s.refFiles() {
			scopesByHash[r.InfoHash] = r.Scopes
		}
		out := []PeerFile{}
		for _, h := range c.ListSeeds() {
			if !s.servableTo(scopesByHash[h.InfoHash], fromPub) {
				continue
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
	camp := s.campDir(campID)
	s.sharedDir = filepath.Join(camp, "uploads")
	s.downloads = filepath.Join(camp, "downloads")
	// Relocate from the old names on first run after the rename.
	migrateDir(filepath.Join(camp, "shared"), s.sharedDir)
	migrateDir(filepath.Join(camp, "drops"), s.downloads)
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
	go func() {
		s.rescanSharedDir(c, opts.SharedDir) // re-seed my uploads first…
		s.restoreReferenced(c)               // …then resume/seed files the blocks reference
	}()
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

// PathForInfoHash returns the on-disk path of a locally-present torrent file —
// a seed we share (uploads/) or a download we've fetched (downloads/) — or ""
// if we don't have it. Used to "reveal in Finder" by info hash.
func (s *Service) PathForInfoHash(infoHash string) string {
	c := s.Client()
	if c == nil {
		return ""
	}
	for _, h := range c.ListSeeds() {
		if h.InfoHash == infoHash {
			return h.Path
		}
	}
	for _, d := range c.ListDownloads() {
		if d.InfoHash == infoHash {
			return c.DownloadPath(d)
		}
	}
	return ""
}

// AddDownload wraps Client.AddDownload. Idempotent — re-adding the same
// info_hash is a no-op. Survival across restarts no longer needs a downloads
// file: the block that references the file carries its magnet, and Start
// resumes every referenced file (see restoreReferenced).
func (s *Service) AddDownload(magnet string, peers []string) (*internaltorrent.Download, error) {
	c := s.Client()
	if c == nil {
		return nil, fmt.Errorf("drop: torrent client not running")
	}
	return c.AddDownload(magnet, peers)
}

// RemoveDownload cancels (or unseeds) a download by info_hash for this session.
// Files on disk are NOT deleted; pruneLoop handles that when the user removes
// them via Finder. Note: if a block still references the file and its bytes are
// on disk, Start's restoreReferenced re-adds it next launch — removal sticks
// only once the referencing block (message/note) is gone too.
func (s *Service) RemoveDownload(infoHash string) bool {
	c := s.Client()
	if c == nil {
		return false
	}
	return c.RemoveDownload(infoHash)
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

// Share seeds a local file and returns its peer-facing metadata
// (name/size/info_hash/magnet) for embedding in the block (message/note/…) that
// references it. The file's scope is NOT recorded here — it's the scope of that
// block, surfaced via the SetFileIndex resolver.
func (s *Service) Share(path string) (PeerFile, error) {
	c := s.Client()
	if c == nil {
		return PeerFile{}, fmt.Errorf("drop: torrent client not running")
	}
	h, err := c.AddSeed(path)
	if err != nil {
		return PeerFile{}, err
	}
	return PeerFile{Name: h.Name, Size: h.Size, InfoHash: h.InfoHash, Magnet: h.Magnet}, nil
}

// servableTo reports whether a file referenced in the given scopes may be
// served to pub: true iff pub is a member of at least one of them. No scopes
// (no block references the file) ⇒ not served — there is no public catalog.
func (s *Service) servableTo(scopes []string, pub string) bool {
	if s.isMember == nil {
		return false
	}
	for _, sc := range scopes {
		if s.isMember(sc, pub) {
			return true
		}
	}
	return false
}

// restoreReferenced resumes/re-seeds files the blocks reference whose bytes we
// already have in downloads/ — derived from the block index (magnet) + disk
// (presence), no downloads.json. It does NOT fetch files we never downloaded:
// only those with local bytes are (re-)added, so we don't pull the whole camp's
// catalog on every start. Files I share live in uploads/ and are re-seeded by
// rescanSharedDir before this runs.
func (s *Service) restoreReferenced(c *internaltorrent.Client) {
	seeded := map[string]bool{}
	for _, h := range c.ListSeeds() {
		seeded[h.InfoHash] = true
	}
	peers := s.campPeerAddrs()
	added := 0
	for _, r := range s.refFiles() {
		if r.Magnet == "" || seeded[r.InfoHash] || !s.haveLocalBytes(r.Name) {
			continue
		}
		if _, err := c.AddDownload(r.Magnet, peers); err != nil {
			clog.Warn("downloads", "resume %s: %v", r.InfoHash, err)
			continue
		}
		added++
	}
	if added > 0 {
		clog.Debug("downloads", "resumed %d previously-downloaded file(s)", added)
	}
}

// haveLocalBytes reports whether a downloaded file (complete or partial) is
// present on disk — the torrent stores it as downloads/<name>.
func (s *Service) haveLocalBytes(name string) bool {
	if name == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(s.downloads, name))
	return err == nil
}

// campPeerAddrs lists online camp peers as torrent peer addresses
// (overlayIP:torrentPort) to seed a resumed download with — there's no tracker
// on the overlay, so peers are fed explicitly.
func (s *Service) campPeerAddrs() []string {
	var out []string
	for _, p := range s.eng.OnlinePeersForCAPoll() {
		if p.Host != "" {
			out = append(out, net.JoinHostPort(p.Host, fmt.Sprint(internaltorrent.DefaultPort)))
		}
	}
	return out
}

// migrateDir renames oldPath→newPath when newPath doesn't exist and oldPath
// does — used to relocate uploads/ (was shared/) and downloads/ (was drops/).
func migrateDir(oldPath, newPath string) {
	if _, err := os.Stat(newPath); err == nil {
		return
	}
	if _, err := os.Stat(oldPath); err != nil {
		return
	}
	_ = os.Rename(oldPath, newPath)
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
		clog.Debug("torrent", "rescan re-seeded %s (%d bytes, info_hash=%s)", name, h.Size, h.InfoHash)
	}
	if added > 0 {
		clog.Debug("torrent", "rescan re-seeded %d file(s) from %s", added, dir)
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
	// Drop a completed download whose file the user deleted from disk
	// (Finder) — walk the live download set, no persisted list needed.
	for _, d := range c.ListDownloads() {
		if d.Torrent == nil || d.Torrent.Info() == nil {
			continue
		}
		total := d.Torrent.Info().TotalLength()
		if total <= 0 || d.Torrent.BytesCompleted() < total {
			continue // still fetching — keep
		}
		path := c.DownloadPath(d)
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			clog.Info("downloads", "file gone, dropping %s (%s)", d.InfoHash, path)
			c.RemoveDownload(d.InfoHash)
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
