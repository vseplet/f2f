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
	// Append validates and stores e. Idempotent on a known ID. Errors on
	// a bad signature, a seq gap, or a prev-hash that breaks the chain.
	Append(e *Entry) error
	// Head returns the author's latest entry in scope (nil if none).
	Head(scope, author string) *Entry
	// Entries returns every entry in scope in a deterministic total order
	// (Lamport, then ID) — the order apps fold over.
	Entries(scope string) []*Entry
	// Vector returns the per-author max seq for scope.
	Vector(scope string) VersionVector
	// Since returns entries in scope beyond what `have` covers.
	Since(scope string, have VersionVector) []*Entry
	// Scopes lists every scope with at least one entry.
	Scopes() []string
}

// MemStore is an in-memory Store. Safe for concurrent use.
type MemStore struct {
	mu     sync.Mutex
	scopes map[string]*scopeLog
}

type scopeLog struct {
	byID     map[string]*Entry   // ID → entry (idempotency)
	byAuthor map[string][]*Entry // author → entries in seq order
}

func NewMemStore() *MemStore {
	return &MemStore{scopes: map[string]*scopeLog{}}
}

func (m *MemStore) Append(e *Entry) error {
	if err := e.verify(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sl := m.scopes[e.Scope]
	if sl == nil {
		sl = &scopeLog{byID: map[string]*Entry{}, byAuthor: map[string][]*Entry{}}
		m.scopes[e.Scope] = sl
	}
	if _, dup := sl.byID[e.ID]; dup {
		return nil // idempotent
	}
	chain := sl.byAuthor[e.Author]
	var wantSeq uint64 = 1
	var wantPrev string
	if n := len(chain); n > 0 {
		wantSeq = chain[n-1].Seq + 1
		wantPrev = chain[n-1].ID
	}
	if e.Seq != wantSeq {
		return fmt.Errorf("db: seq gap for %s/%s: have %d, got %d", short(e.Author), e.Scope, wantSeq-1, e.Seq)
	}
	if e.Prev != wantPrev {
		return fmt.Errorf("db: broken chain for %s/%s at seq %d", short(e.Author), e.Scope, e.Seq)
	}
	sl.byID[e.ID] = e
	sl.byAuthor[e.Author] = append(chain, e)
	return nil
}

func (m *MemStore) Head(scope, author string) *Entry {
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

func (m *MemStore) Entries(scope string) []*Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedEntries(m.scopes[scope])
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

func (m *MemStore) Since(scope string, have VersionVector) []*Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	sl := m.scopes[scope]
	if sl == nil {
		return nil
	}
	var out []*Entry
	for author, c := range sl.byAuthor {
		from := have[author]
		for _, e := range c {
			if e.Seq > from {
				out = append(out, e)
			}
		}
	}
	sortEntries(out)
	return out
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

// sortedEntries flattens a scope's per-author chains into one
// deterministic order.
func sortedEntries(sl *scopeLog) []*Entry {
	if sl == nil {
		return nil
	}
	out := make([]*Entry, 0, len(sl.byID))
	for _, e := range sl.byID {
		out = append(out, e)
	}
	sortEntries(out)
	return out
}

// sortEntries imposes the canonical total order: Lamport asc, then ID.
// Causal order (app-level parent refs) is a refinement apps apply on top.
func sortEntries(es []*Entry) {
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
