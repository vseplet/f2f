package db

import (
	"fmt"
	"sort"
	"sync"
)

// VersionVector maps author → highest seq this replica holds, for one
// scope. It's the compact "what do I have" summary anti-entropy compares.
type VersionVector map[string]uint64

// Store persists entries and answers replication queries. Append
// validates the per-author chain and signature; reads are per scope.
// In-memory now; a SQLite-backed Store (one dumpable file) comes next
// behind the same interface.
type Store interface {
	// Append validates and stores e. Idempotent on a known ID — returns
	// applied=false (no error) when the frame was already present, so callers
	// can skip side effects (hooks/notifications) on duplicates. Errors on a
	// bad signature, a seq gap, or a prev-hash that breaks the chain.
	Append(e *Frame) (applied bool, err error)
	// Head returns the author's latest entry in scope (nil if none).
	Head(scope, author string) *Frame
	// Frames returns every entry in scope in a deterministic total order
	// (Lamport, then ID) — the order apps fold over.
	Frames(scope string) []*Frame
	// Vector returns the per-author max seq for scope.
	Vector(scope string) VersionVector
	// Since returns entries in scope beyond what `have` covers.
	Since(scope string, have VersionVector) []*Frame
	// Scopes lists every scope with at least one entry.
	Scopes() []string
	// MaxLamport returns the highest Lamport across all stored entries (0 if
	// empty) — used to reseed the logical clock after a restart so new local
	// writes never collide with persisted ones.
	MaxLamport() uint64
}

// MemStore is an in-memory Store. Safe for concurrent use.
type MemStore struct {
	mu     sync.Mutex
	scopes map[string]*scopeFrames
}

type scopeFrames struct {
	byID     map[string]*Frame   // ID → entry (idempotency)
	byAuthor map[string][]*Frame // author → entries in seq order
}

func NewMemStore() *MemStore {
	return &MemStore{scopes: map[string]*scopeFrames{}}
}

func (m *MemStore) Append(e *Frame) (bool, error) {
	if err := e.verify(); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sl := m.scopes[e.Scope]
	if sl == nil {
		sl = &scopeFrames{byID: map[string]*Frame{}, byAuthor: map[string][]*Frame{}}
		m.scopes[e.Scope] = sl
	}
	if _, dup := sl.byID[e.ID]; dup {
		return false, nil // idempotent: already have it
	}
	chain := sl.byAuthor[e.Author]
	var wantSeq uint64 = 1
	var wantPrev string
	if n := len(chain); n > 0 {
		wantSeq = chain[n-1].Seq + 1
		wantPrev = chain[n-1].ID
	}
	if e.Seq != wantSeq {
		return false, fmt.Errorf("db: seq gap for %s/%s: have %d, got %d", short(e.Author), e.Scope, wantSeq-1, e.Seq)
	}
	if e.Prev != wantPrev {
		return false, fmt.Errorf("db: broken chain for %s/%s at seq %d", short(e.Author), e.Scope, e.Seq)
	}
	sl.byID[e.ID] = e
	sl.byAuthor[e.Author] = append(chain, e)
	return true, nil
}

func (m *MemStore) Head(scope, author string) *Frame {
	m.mu.Lock()
	defer m.mu.Unlock()
	sl := m.scopes[scope]
	if sl == nil {
		return nil
	}
	c := sl.byAuthor[author]
	if len(c) == 0 {
		return nil
	}
	return c[len(c)-1]
}

func (m *MemStore) Frames(scope string) []*Frame {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedFrames(m.scopes[scope])
}

func (m *MemStore) Vector(scope string) VersionVector {
	m.mu.Lock()
	defer m.mu.Unlock()
	vv := VersionVector{}
	sl := m.scopes[scope]
	if sl == nil {
		return vv
	}
	for author, c := range sl.byAuthor {
		if n := len(c); n > 0 {
			vv[author] = c[n-1].Seq
		}
	}
	return vv
}

func (m *MemStore) Since(scope string, have VersionVector) []*Frame {
	m.mu.Lock()
	defer m.mu.Unlock()
	sl := m.scopes[scope]
	if sl == nil {
		return nil
	}
	var out []*Frame
	for author, c := range sl.byAuthor {
		from := have[author]
		for _, e := range c {
			if e.Seq > from {
				out = append(out, e)
			}
		}
	}
	sortFrames(out)
	return out
}

func (m *MemStore) MaxLamport() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var max uint64
	for _, sl := range m.scopes {
		for _, e := range sl.byID {
			if e.Lamport > max {
				max = e.Lamport
			}
		}
	}
	return max
}

func (m *MemStore) Scopes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.scopes))
	for s := range m.scopes {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// sortedFrames flattens a scope's per-author chains into one
// deterministic order.
func sortedFrames(sl *scopeFrames) []*Frame {
	if sl == nil {
		return nil
	}
	out := make([]*Frame, 0, len(sl.byID))
	for _, e := range sl.byID {
		out = append(out, e)
	}
	sortFrames(out)
	return out
}

// sortFrames imposes the canonical total order: Lamport asc, then ID.
// Causal order (app-level parent refs) is a refinement apps apply on top.
func sortFrames(es []*Frame) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].Lamport != es[j].Lamport {
			return es[i].Lamport < es[j].Lamport
		}
		return es[i].ID < es[j].ID
	})
}

func short(pub string) string {
	if len(pub) > 12 {
		return pub[:12]
	}
	return pub
}
