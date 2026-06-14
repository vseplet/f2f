package db

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Service is the single distributed-database instance every other
// service builds on. It owns one Store, assigns Seq/Prev/Lamport on local
// writes, accepts replicated entries from peers, and can dump/import the
// whole DB for sharing.
type Service struct {
	mu       sync.Mutex
	store    Store
	lamport  uint64         // highest Lamport seen across all scopes
	now      func() int64   // wall-clock ms; injectable for tests
	onCommit func(e *Entry) // optional hook fired after a local Commit (sync push)
	onApply  func(e *Entry) // optional hook fired after a remote entry is applied
}

// OnCommit registers a hook called after each successful local Commit —
// the sync layer uses it to eagerly push new entries to peers.
func (svc *Service) OnCommit(fn func(*Entry)) { svc.onCommit = fn }

// OnApply registers a hook called after each remote entry is successfully
// applied (anti-entropy) — the UI uses it to live-refresh open views, since
// OnCommit only covers local writes.
func (svc *Service) OnApply(fn func(*Entry)) { svc.onApply = fn }

// New builds the service over store (use NewMemStore for now).
func New(store Store) *Service {
	return &Service{store: store, now: func() int64 { return time.Now().UnixMilli() }}
}

// Commit appends a local entry authored by s into scope. It assigns the
// next Seq, links Prev to the author's head, stamps a fresh Lamport, and
// signs. Returns the stored entry.
func (svc *Service) Commit(s signer, scope, typ string, payload []byte) (*Entry, error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	author := s.PubHex()
	var seq uint64 = 1
	var prev string
	if head := svc.store.Head(scope, author); head != nil {
		seq = head.Seq + 1
		prev = head.ID
	}
	// Reseed the logical clock from the store so a restart (lamport resets to
	// 0 in memory) never stamps a new write below persisted ones — otherwise
	// the fresh edit sorts as "older" and the fold shows stale content.
	if m := svc.store.MaxLamport(); m > svc.lamport {
		svc.lamport = m
	}
	svc.lamport++
	e := &Entry{
		Scope:   scope,
		Seq:     seq,
		Prev:    prev,
		Lamport: svc.lamport,
		Type:    typ,
		Payload: payload,
		TS:      svc.now(),
	}
	e.sign(s)
	if err := svc.store.Append(e); err != nil {
		return nil, err
	}
	if svc.onCommit != nil {
		svc.onCommit(e)
	}
	return e, nil
}

// Apply ingests an entry received from a peer (anti-entropy). It advances
// the local Lamport clock and validates+stores via the Store.
func (svc *Service) Apply(e *Entry) error {
	svc.mu.Lock()
	if e.Lamport > svc.lamport {
		svc.lamport = e.Lamport
	}
	err := svc.store.Append(e)
	hook := svc.onApply
	svc.mu.Unlock()
	if err == nil && hook != nil {
		hook(e) // outside the lock: the hook must not call back into svc
	}
	return err
}

// Entries returns scope's entries in canonical order — apps fold over it.
func (svc *Service) Entries(scope string) []*Entry { return svc.store.Entries(scope) }

// Vector returns scope's version vector (for anti-entropy).
func (svc *Service) Vector(scope string) VersionVector { return svc.store.Vector(scope) }

// Since returns entries in scope beyond what `have` covers (the bytes a
// peer is missing).
func (svc *Service) Since(scope string, have VersionVector) []*Entry {
	return svc.store.Since(scope, have)
}

// Scopes lists all scopes with entries.
func (svc *Service) Scopes() []string { return svc.store.Scopes() }

// Dump serializes the entire database (every scope's entries) to JSON —
// for sharing or backup. Entries carry their own signatures, so a dump is
// self-verifying on Import.
func (svc *Service) Dump() ([]byte, error) {
	svc.mu.Lock()
	scopes := svc.store.Scopes()
	var all []*Entry
	for _, sc := range scopes {
		all = append(all, svc.store.Entries(sc)...)
	}
	svc.mu.Unlock()
	return json.Marshal(all)
}

// Import merges a dumped database into this one. Entries are applied in
// per-author chain order so prev-links resolve; bad or duplicate entries
// are skipped. Returns how many new entries were stored.
func (svc *Service) Import(blob []byte) (int, error) {
	var all []*Entry
	if err := json.Unmarshal(blob, &all); err != nil {
		return 0, fmt.Errorf("db: import decode: %w", err)
	}
	// Apply oldest-first per (scope,author) so chains link up.
	sort.Slice(all, func(i, j int) bool {
		if all[i].Scope != all[j].Scope {
			return all[i].Scope < all[j].Scope
		}
		if all[i].Author != all[j].Author {
			return all[i].Author < all[j].Author
		}
		return all[i].Seq < all[j].Seq
	})
	n := 0
	for _, e := range all {
		before := svc.store.Head(e.Scope, e.Author)
		if err := svc.Apply(e); err != nil {
			continue // skip bad/out-of-order; idempotent dups are no-ops
		}
		if after := svc.store.Head(e.Scope, e.Author); after != nil && (before == nil || after.ID != before.ID) {
			n++
		}
	}
	return n, nil
}
