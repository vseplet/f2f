package engine

import (
	"errors"
	"log"

	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/engine/rendezvous"
)

// ensureStore returns nil when the engine was constructed with a
// Store via engine.New, or an error otherwise — every code path that
// touches persisted state goes through this so the engine never
// silently no-ops a save.
func (e *Engine) ensureStore() error {
	if e.store == nil {
		return errors.New("engine: no config store (use engine.New(store))")
	}
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
		c.Identity.Name = name
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
	byPub := make(map[string]int, len(e.camp.PeerCatalog))
	for i, p := range e.camp.PeerCatalog {
		if p.Pub != "" {
			byPub[p.Pub] = i
		}
	}
	for _, p := range peers {
		if p.Pub == "" || p.Name == ourName {
			continue
		}
		entry := config.Peer{
			Name:        p.Name,
			Pub:         p.Pub,
			PublicIP:    p.PublicIP,
			UDPPort:     p.UDPPort,
			UDPEndpoint: p.UDPEndpoint,
			JoinedAt:    p.JoinedAt,
			LastSeenAt:  p.LastSeenAt,
			Online:      p.Online,
		}
		if idx, ok := byPub[p.Pub]; ok {
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
	ourPub := ""
	if e.identity != nil {
		ourPub = e.identity.PubHex()
	}
	kept := e.camp.PeerCatalog[:0]
	dropped := 0
	for _, p := range e.camp.PeerCatalog {
		if p.Name == ourName || (ourPub != "" && p.Pub == ourPub) {
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
		if p.Name == ourName {
			continue
		}
		if p.Pub == "" {
			continue
		}
		if _, dup := e.peers[p.Pub]; dup {
			continue
		}
		st := &peerState{
			Name:        p.Name,
			Pub:         p.Pub,
			PublicIP:    p.PublicIP,
			UDPPort:     p.UDPPort,
			UDPEndpoint: p.UDPEndpoint,
			JoinedAt:    p.JoinedAt,
			LastSeenAt:  p.LastSeenAt,
			// Online=false and UDPAddr=nil. The next camp poll either
			// confirms the peer is back online (and we resolve a fresh
			// UDPAddr) or leaves them dormant.
		}
		e.peers[p.Pub] = st
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

// StartLastCamp loads state.json, picks last_camp_id, reads that
// camp's config for the name, and calls Start with the same defaults
// /api/start would use. Used by main.go to auto-start the engine as
// the binary boots — no UI interaction needed.
//
// Returns nil silently when:
//   - state.json doesn't exist yet (fresh install)
//   - last_camp_id is empty
//   - no per-camp config exists for the last id (config wiped)
//   - the camp config has no name (would require user input to start)
//
// Real errors (filesystem failures, Start errors) bubble up.
func (e *Engine) StartLastCamp() error {
	if err := e.ensureStore(); err != nil {
		return err
	}
	st, err := e.store.LoadState()
	if err != nil {
		return err
	}
	if st == nil || st.LastCampID == "" {
		return nil
	}
	camp, err := e.store.LoadCamp(st.LastCampID)
	if err != nil {
		return err
	}
	if camp == nil || camp.Identity.Name == "" {
		log.Printf("autostart: skipping camp %s (no config or missing name)", st.LastCampID)
		return nil
	}
	listen := e.defaultListen
	if listen == "" {
		listen = ":0"
	}
	cfg := Config{
		LocalIP: "10.99.0.1", // placeholder; camp announce overrides
		Listen:  listen,
		Camp: &CampConfig{
			URL:      "wss://f2f-camp.fly.dev/ws",
			StunAddr: "f2f-camp.fly.dev:3478",
			Name:     camp.Identity.Name,
			ID:       camp.CampID,
		},
	}
	log.Printf("autostart: starting last camp %s as %s", camp.CampID, camp.Identity.Name)
	return e.Start(cfg)
}

// Intercept restoration moved to services/tunnel.
