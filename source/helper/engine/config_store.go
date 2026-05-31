package engine

import (
	"errors"
	"log"
	"os"
	"path/filepath"

	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/engine/rendezvous"
	"github.com/vseplet/f2f/source/helper/platform"
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
		if len(p.Domains) > 0 {
			dup := make([]DomainEntry, len(p.Domains))
			for i, d := range p.Domains {
				dup[i] = DomainEntry{Name: d.Name, Host: d.Host, Port: d.Port, Proto: d.Proto}
			}
			st.Domains = dup
		}
		if len(p.Firewall) > 0 {
			dup := make([]FirewallPort, len(p.Firewall))
			for i, f := range p.Firewall {
				dup[i] = FirewallPort{Port: f.Port, Protocol: f.Protocol, Description: f.Description, Enabled: f.Enabled}
			}
			st.Firewall = dup
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

// RemovePeerDomain drops one (peer, domain) entry from the persisted
// catalog AND from the live peer's Domains slice. If the peer is
// currently online and still publishing that name, the next successful
// poll will re-add it — that's intentional: removal is for stale
// entries where the peer is offline or no longer publishes.
func (e *Engine) RemovePeerDomain(peerName, domainName string) error {
	if peerName == "" || domainName == "" {
		return errors.New("peer_name and domain_name required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// In-memory: peers are keyed by tunnel_ip; find by name.
	for _, p := range e.peers {
		if p.Name != peerName {
			continue
		}
		kept := p.Domains[:0]
		for _, d := range p.Domains {
			if d.Name == domainName {
				continue
			}
			kept = append(kept, d)
		}
		p.Domains = kept
	}
	if e.camp == nil {
		return nil
	}
	changed := false
	for i := range e.camp.PeerCatalog {
		if e.camp.PeerCatalog[i].Name != peerName {
			continue
		}
		kept := e.camp.PeerCatalog[i].Domains[:0]
		for _, d := range e.camp.PeerCatalog[i].Domains {
			if d.Name == domainName {
				changed = true
				continue
			}
			kept = append(kept, d)
		}
		e.camp.PeerCatalog[i].Domains = kept
	}
	if changed {
		e.persistCampLocked()
	}
	return nil
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
	certPath := filepath.Join(e.trustedPeersDir(), entry.PeerName+".crt")
	if err := os.Remove(certPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("ca: remove %s: %v", certPath, err)
	}
	if err := platform.TrustStoreRemove(entry.CommonName); err != nil {
		log.Printf("ca: trust store remove %s: %v", entry.CommonName, err)
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
