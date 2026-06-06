package engine

// Read-only seeding of the in-memory peer map from the camp config the
// caller handed in at Start. The engine never writes the catalog back —
// persistence of roster snapshots is owned by services/camp.

// pruneSelfFromCatalogLocked drops any seed entries whose name or pub
// match our own — defensive cleanup for catalogs written by older builds
// (before self was filtered at the source). In-memory only; the on-disk
// file is reconciled by services/camp on the next poll. Called from Start
// under e.mu.
func (e *Engine) pruneSelfFromCatalogLocked() {
	if e.camp == nil || e.cfg.CampID == "" {
		return
	}
	ourName := e.cfg.CampName
	ourPub := ""
	if e.identity != nil {
		ourPub = e.identity.PubHex()
	}
	kept := e.camp.PeerCatalog[:0]
	for _, p := range e.camp.PeerCatalog {
		if p.Name == ourName || (ourPub != "" && p.Pub == ourPub) {
			continue
		}
		kept = append(kept, p)
	}
	e.camp.PeerCatalog = kept
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
	if e.cfg.CampID != "" {
		ourName = e.cfg.CampName
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
