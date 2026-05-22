//go:build darwin

package engine

import (
	"errors"
	"log"
	"os"
	"path/filepath"

	"github.com/vseplet/f2f/source/mac/internal/config"
	"github.com/vseplet/f2f/source/mac/internal/keychain"
	"github.com/vseplet/f2f/source/mac/internal/rendezvous"
)

// ensureStore lazily opens $HOME/.f2f/. Called from Start so test code
// that just constructs an Engine doesn't touch the filesystem.
func (e *Engine) ensureStore() error {
	if e.store != nil {
		return nil
	}
	s, err := config.NewStore()
	if err != nil {
		return err
	}
	e.store = s
	return nil
}

// loadOrCreateCamp loads <camp_id>.config.json (or creates a fresh
// Camp in memory if no file exists yet). name overrides whatever was
// on disk — the caller passes the value from /api/start, which we
// treat as the source of truth when the user explicitly typed it.
// When name is empty, we keep the on-disk value.
func (e *Engine) loadOrCreateCamp(id, name string) (*config.Camp, error) {
	c, err := e.store.LoadCamp(id)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return config.NewCamp(id, name), nil
	}
	if name != "" {
		c.Name = name
	}
	return c, nil
}

// persistCampLocked writes e.camp to disk. Must be called with e.mu
// held (or, more loosely, while we know nothing else mutates e.camp).
// Errors are logged, not returned — losing one save shouldn't crash
// the engine; the next mutation will retry.
func (e *Engine) persistCampLocked() {
	if e.store == nil || e.camp == nil {
		return
	}
	if err := e.store.SaveCamp(e.camp.CampID, e.camp); err != nil {
		log.Printf("config: save %s.config.json: %v", e.camp.CampID, err)
	}
}

// upsertKnownCamp adds (or refreshes) a camp entry in state.json's
// known_camps list and bumps last_camp_id to id. Called from Start
// after a successful announce so the UI dropdown sees the new camp.
func (e *Engine) upsertKnownCamp(id, name string) {
	if e.store == nil {
		return
	}
	st, err := e.store.LoadState()
	if err != nil {
		log.Printf("config: load state.json: %v", err)
		return
	}
	st.LastCampID = id
	found := false
	for i := range st.KnownCamps {
		if st.KnownCamps[i].ID == id {
			if name != "" {
				st.KnownCamps[i].Name = name
			}
			found = true
			break
		}
	}
	if !found {
		st.KnownCamps = append(st.KnownCamps, config.KnownCamp{ID: id, Name: name})
	}
	if err := e.store.SaveState(st); err != nil {
		log.Printf("config: save state.json: %v", err)
	}
}

// mergePeerSnapshotLocked upserts every PeerInfo from the latest camp
// poll into e.camp.PeerCatalog. Existing entries are refreshed in
// place; new peers are appended. Removed peers stay — the catalog is
// historical (we don't yet support node deletion). Called with e.mu
// held by applyPeerList.
func (e *Engine) mergePeerSnapshotLocked(peers []rendezvous.PeerInfo) {
	if e.camp == nil {
		return
	}
	var ourName string
	if e.cfg.Camp != nil {
		ourName = e.cfg.Camp.Name
	}
	byIP := make(map[string]int, len(e.camp.PeerCatalog))
	for i, p := range e.camp.PeerCatalog {
		byIP[p.TunnelIP] = i
	}
	for _, p := range peers {
		if p.TunnelIP == "" || p.Name == ourName {
			continue
		}
		entry := config.Peer{
			Name:        p.Name,
			TunnelIP:    p.TunnelIP,
			PublicIP:    p.PublicIP,
			UDPPort:     p.UDPPort,
			UDPEndpoint: p.UDPEndpoint,
			JoinedAt:    p.JoinedAt,
			LastSeenAt:  p.LastSeenAt,
			Online:      p.Online,
		}
		if idx, ok := byIP[p.TunnelIP]; ok {
			// Preserve previously-known endpoint info if camp now reports
			// the peer as offline (PublicIP/UDPEndpoint go blank when
			// camp marks a peer offline). The catalog is our long-term
			// memory of who's been in the camp.
			if !p.Online {
				prev := e.camp.PeerCatalog[idx]
				if entry.PublicIP == "" {
					entry.PublicIP = prev.PublicIP
				}
				if entry.UDPEndpoint == "" {
					entry.UDPEndpoint = prev.UDPEndpoint
				}
				if entry.UDPPort == 0 {
					entry.UDPPort = prev.UDPPort
				}
				if entry.JoinedAt == 0 {
					entry.JoinedAt = prev.JoinedAt
				}
				if entry.LastSeenAt == 0 {
					entry.LastSeenAt = prev.LastSeenAt
				}
			}
			e.camp.PeerCatalog[idx] = entry
		} else {
			e.camp.PeerCatalog = append(e.camp.PeerCatalog, entry)
		}
	}
}

// pruneSelfFromCatalogLocked drops any catalog entries whose name or
// tunnel_ip match our own — defensive cleanup for files written by
// older builds (before we filtered self in mergePeerSnapshotLocked).
// Called from Start under e.mu. Persists if anything changed.
func (e *Engine) pruneSelfFromCatalogLocked() {
	if e.camp == nil || e.cfg.Camp == nil {
		return
	}
	ourName := e.cfg.Camp.Name
	ourIP := e.cfg.LocalIP
	kept := e.camp.PeerCatalog[:0]
	dropped := 0
	for _, p := range e.camp.PeerCatalog {
		if p.Name == ourName || (ourIP != "" && p.TunnelIP == ourIP) {
			dropped++
			continue
		}
		kept = append(kept, p)
	}
	if dropped > 0 {
		e.camp.PeerCatalog = kept
		e.persistCampLocked()
	}
}

// hydratePeersFromCatalog seeds e.peers from the persisted catalog so
// the UI shows known nodes immediately on engine start (offline ones
// with UDPAddr=nil — holePunchLoop already skips those, tunToPeerLoop
// drops with "no-route"). The next camp poll refreshes Online state
// and re-resolves UDP endpoints for live peers.
func (e *Engine) hydratePeersFromCatalog() {
	if e.camp == nil {
		return
	}
	var ourName string
	if e.cfg.Camp != nil {
		ourName = e.cfg.Camp.Name
	}
	for _, p := range e.camp.PeerCatalog {
		if p.TunnelIP == "" || p.Name == ourName {
			continue
		}
		if _, dup := e.peers[p.TunnelIP]; dup {
			continue
		}
		e.peers[p.TunnelIP] = &peerState{
			Name:        p.Name,
			TunnelIP:    p.TunnelIP,
			PublicIP:    p.PublicIP,
			UDPPort:     p.UDPPort,
			UDPEndpoint: p.UDPEndpoint,
			JoinedAt:    p.JoinedAt,
			LastSeenAt:  p.LastSeenAt,
			// Online=false and UDPAddr=nil. The next camp poll either
			// confirms the peer is back online (and we resolve a fresh
			// UDPAddr) or leaves them dormant.
		}
	}
}

// CampConfig returns a snapshot of the on-disk config for id. nil
// (with nil error) means no config exists yet for that camp_id —
// caller's responsibility to handle (typically: prompt for name on
// the first start). Safe to call when the engine is stopped — uses
// its own Store. Lazily opens the store on first use.
func (e *Engine) CampConfig(id string) (*config.Camp, error) {
	if err := e.ensureStore(); err != nil {
		return nil, err
	}
	return e.store.LoadCamp(id)
}

// ListCamps returns the global state — last selected camp_id + the
// roster of every camp the user has joined. Used by the UI dropdown.
func (e *Engine) ListCamps() (*config.State, error) {
	if err := e.ensureStore(); err != nil {
		return nil, err
	}
	return e.store.LoadState()
}

// RemoveTrustedPeer drops one peer CA — deletes the on-disk PEM, the
// keychain entry, the in-memory cache, and the camp config entry.
// Idempotent: missing pieces are skipped without error. Engine must
// be running so we know which camp file to write.
func (e *Engine) RemoveTrustedPeer(fingerprint string) error {
	if fingerprint == "" {
		return errors.New("empty fingerprint")
	}
	e.trustedPeerCAsMu.Lock()
	entry, ok := e.trustedPeerCAs[fingerprint]
	if ok {
		delete(e.trustedPeerCAs, fingerprint)
	}
	e.trustedPeerCAsMu.Unlock()
	if !ok {
		return nil
	}
	certPath := filepath.Join(trustedPeersDir, entry.PeerName+".crt")
	if err := os.Remove(certPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("ca: remove %s: %v", certPath, err)
	}
	if err := keychain.RemoveByCommonName(entry.CommonName); err != nil {
		log.Printf("ca: keychain remove %s: %v", entry.CommonName, err)
	}
	e.mu.Lock()
	if e.camp != nil {
		kept := e.camp.TrustedPeers[:0]
		for _, t := range e.camp.TrustedPeers {
			if t.Fingerprint == fingerprint {
				continue
			}
			kept = append(kept, t)
		}
		e.camp.TrustedPeers = kept
		e.persistCampLocked()
	}
	e.mu.Unlock()
	return nil
}

// restoreInterceptsFromCamp re-installs every (spec, peer) pair from
// camp config. Called near the end of Start, after utun + routes are
// up and e.peers is hydrated. Entries whose peer is no longer in the
// camp catalog are logged and skipped; they'll be retried by the
// next reconcile (currently driven by the frontend — that goes away
// once the UI reads intercepts straight from camp config).
func (e *Engine) restoreInterceptsFromCamp() {
	if e.camp == nil {
		return
	}
	for _, it := range e.camp.Intercepts {
		if it.Spec == "" || it.Peer == "" {
			continue
		}
		if !e.hasPeerNameLocked(it.Peer) {
			log.Printf("config: intercept %q via %s skipped (peer not in catalog)", it.Spec, it.Peer)
			continue
		}
		if _, err := e.addInterceptLocked(it.Spec, it.Peer); err != nil {
			log.Printf("config: restore intercept %q via %s: %v", it.Spec, it.Peer, err)
		}
	}
}
