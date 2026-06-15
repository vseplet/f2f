package db

import (
	"context"
	"encoding/json"
	"sort"
)

// Bus is the minimal transport the sync layer needs — satisfied by
// mesh/bus.Service (via a tiny adapter in main, since its Handle takes the
// named bus.HandlerFunc). Kept as a plain-func interface so db has no
// dependency on mesh/bus and tests can fake it.
type Bus interface {
	Handle(typ string, fn func(fromPub string, payload []byte) ([]byte, error))
	Request(ctx context.Context, pub, typ string, payload []byte) ([]byte, error)
	Notify(pub, typ string, payload []byte) error
	Peers() []string
}

// Bus message types.
const (
	typePull   = "db.pull"   // Request: {scope, have} → reply: []*Frame the asker lacks
	typePush   = "db.push"   // Notify: one *Frame, sent eagerly on local commit
	typeScopes = "db.scopes" // Request: nil → reply: []string of the peer's scopes
)

// Sync replicates the log between peers by anti-entropy: pull (ask a peer
// for entries beyond our version vector) + eager push (fan a freshly
// committed entry to peers). One mechanism covers catch-up, redelivery
// and convergence. Relay / e2e / membership-gating layer on top later.
type Sync struct {
	svc *Service
	bus Bus
}

func NewSync(svc *Service, bus Bus) *Sync { return &Sync{svc: svc, bus: bus} }

// Register wires the bus handlers. Call once after construction.
func (s *Sync) Register() {
	s.bus.Handle(typePull, s.onPull)
	s.bus.Handle(typePush, s.onPush)
	s.bus.Handle(typeScopes, s.onScopes)
}

// onScopes answers a peer's "which scopes do you have" — lets a peer that
// joined late discover scopes it has never seen and pull them.
func (s *Sync) onScopes(_ string, _ []byte) ([]byte, error) {
	return json.Marshal(s.svc.Scopes())
}

// remoteScopes asks a peer for its scope list (empty on an un-upgraded peer
// that lacks the handler — caller falls back to its own scopes).
func (s *Sync) remoteScopes(ctx context.Context, pub string) []string {
	resp, err := s.bus.Request(ctx, pub, typeScopes, nil)
	if err != nil {
		return nil
	}
	var scopes []string
	if json.Unmarshal(resp, &scopes) != nil {
		return nil
	}
	return scopes
}

type pullReq struct {
	Scope string        `json:"scope"`
	Have  VersionVector `json:"have"`
}

// onPull answers a peer's "what am I missing in this scope".
func (s *Sync) onPull(_ string, payload []byte) ([]byte, error) {
	var req pullReq
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err
	}
	return json.Marshal(s.svc.Since(req.Scope, req.Have))
}

// onPush ingests an eagerly-pushed entry. On a gap (Apply fails because an
// earlier seq is missing) it pulls the scope from the sender to fill in.
func (s *Sync) onPush(from string, payload []byte) ([]byte, error) {
	var e Frame
	if err := json.Unmarshal(payload, &e); err != nil {
		return nil, err
	}
	if err := s.svc.Apply(&e); err != nil {
		go func() { _ = s.PullScope(context.Background(), from, e.Scope) }()
	}
	return nil, nil
}

// Push fans a freshly-committed entry to all reachable peers (best-effort;
// anyone offline catches up later via pull).
func (s *Sync) Push(e *Frame) {
	payload, err := json.Marshal(e)
	if err != nil {
		return
	}
	for _, pub := range s.bus.Peers() {
		_ = s.bus.Notify(pub, typePush, payload)
	}
}

// PullScope asks one peer for everything we lack in scope and applies it.
func (s *Sync) PullScope(ctx context.Context, pub, scope string) error {
	req, err := json.Marshal(pullReq{Scope: scope, Have: s.svc.Vector(scope)})
	if err != nil {
		return err
	}
	resp, err := s.bus.Request(ctx, pub, typePull, req)
	if err != nil {
		return err
	}
	var entries []*Frame
	if err := json.Unmarshal(resp, &entries); err != nil {
		return err
	}
	applyInOrder(s.svc, entries)
	return nil
}

// PullAll pulls from every reachable peer. For each peer it unions our scopes
// with that peer's (discovered via db.scopes), so a scope we've never seen —
// e.g. a channel's notes created while we were offline — gets discovered and
// pulled rather than staying invisible forever.
func (s *Sync) PullAll(ctx context.Context) {
	local := s.svc.Scopes()
	for _, pub := range s.bus.Peers() {
		scopes := map[string]bool{}
		for _, sc := range local {
			scopes[sc] = true
		}
		for _, sc := range s.remoteScopes(ctx, pub) {
			scopes[sc] = true
		}
		for sc := range scopes {
			_ = s.PullScope(ctx, pub, sc)
		}
	}
}

// applyInOrder applies entries respecting each author's seq chain (sort by
// author then seq) so prev-links resolve; gaps just skip that author's tail.
func applyInOrder(svc *Service, entries []*Frame) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Author != entries[j].Author {
			return entries[i].Author < entries[j].Author
		}
		return entries[i].Seq < entries[j].Seq
	})
	for _, e := range entries {
		if err := svc.Apply(e); err != nil {
			break // chain gap for this author; rest are out of order
		}
	}
}
