package db

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver (no cgo)
)

// SQLiteStore is a persistent Store backed by one file per camp
// (db.sqlite) — the whole log in a single dumpable file. It lazily opens
// the current camp's file and reopens on a camp switch (dirFn returns the
// active camp dir, "" outside a camp).
type SQLiteStore struct {
	dirFn func() string

	mu  sync.Mutex
	dir string
	db  *sql.DB
}

func NewSQLiteStore(dirFn func() string) *SQLiteStore { return &SQLiteStore{dirFn: dirFn} }

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS entries (
  id      TEXT PRIMARY KEY,
  scope   TEXT NOT NULL,
  author  TEXT NOT NULL,
  seq     INTEGER NOT NULL,
  prev    TEXT,
  lamport INTEGER NOT NULL,
  type    TEXT NOT NULL,
  ts      INTEGER,
  payload BLOB,
  sig     TEXT NOT NULL,
  UNIQUE(scope, author, seq)
);
CREATE INDEX IF NOT EXISTS ix_scope_lamport ON entries(scope, lamport);
CREATE INDEX IF NOT EXISTS ix_scope_type    ON entries(scope, type);`

// connLocked returns the DB handle for the current camp, (re)opening it on
// first use or after a camp switch. Caller holds s.mu.
func (s *SQLiteStore) connLocked() (*sql.DB, error) {
	dir := s.dirFn()
	if dir == "" {
		return nil, errors.New("db: not in a camp")
	}
	if s.db != nil && s.dir == dir {
		return s.db, nil
	}
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
	path := filepath.Join(dir, "db.sqlite")
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("db: open %s: %w", path, err)
	}
	if _, err := d.Exec(sqliteSchema); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("db: schema: %w", err)
	}
	s.db, s.dir = d, dir
	return d, nil
}

func (s *SQLiteStore) Append(e *Entry) error {
	if err := e.verify(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.connLocked()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var dummy string
	switch err := tx.QueryRow(`SELECT id FROM entries WHERE id=?`, e.ID).Scan(&dummy); err {
	case nil:
		return nil // idempotent: already have it
	case sql.ErrNoRows:
		// proceed
	default:
		return err
	}

	var wantSeq uint64 = 1
	var wantPrev string
	row := tx.QueryRow(`SELECT seq, id FROM entries WHERE scope=? AND author=? ORDER BY seq DESC LIMIT 1`, e.Scope, e.Author)
	var lastSeq uint64
	var lastID string
	switch err := row.Scan(&lastSeq, &lastID); err {
	case nil:
		wantSeq, wantPrev = lastSeq+1, lastID
	case sql.ErrNoRows:
		// first in chain
	default:
		return err
	}
	if e.Seq != wantSeq {
		return fmt.Errorf("db: seq gap for %s/%s: have %d, got %d", short(e.Author), e.Scope, wantSeq-1, e.Seq)
	}
	if e.Prev != wantPrev {
		return fmt.Errorf("db: broken chain for %s/%s at seq %d", short(e.Author), e.Scope, e.Seq)
	}
	if _, err := tx.Exec(
		`INSERT INTO entries(id,scope,author,seq,prev,lamport,type,ts,payload,sig) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.Scope, e.Author, e.Seq, e.Prev, e.Lamport, e.Type, e.TS, e.Payload, e.Sig,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) Head(scope, author string) *Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.connLocked()
	if err != nil {
		return nil
	}
	row := d.QueryRow(`SELECT id,scope,author,seq,prev,lamport,type,ts,payload,sig
		FROM entries WHERE scope=? AND author=? ORDER BY seq DESC LIMIT 1`, scope, author)
	e, err := scanEntry(row)
	if err != nil {
		return nil
	}
	return e
}

func (s *SQLiteStore) Entries(scope string) []*Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.connLocked()
	if err != nil {
		return nil
	}
	rows, err := d.Query(`SELECT id,scope,author,seq,prev,lamport,type,ts,payload,sig
		FROM entries WHERE scope=? ORDER BY lamport, id`, scope)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanEntries(rows)
}

func (s *SQLiteStore) Vector(scope string) VersionVector {
	vv := VersionVector{}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.connLocked()
	if err != nil {
		return vv
	}
	rows, err := d.Query(`SELECT author, MAX(seq) FROM entries WHERE scope=? GROUP BY author`, scope)
	if err != nil {
		return vv
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		var seq uint64
		if rows.Scan(&a, &seq) == nil {
			vv[a] = seq
		}
	}
	return vv
}

// Since returns entries beyond `have`, read per-author via the
// UNIQUE(scope,author,seq) index — a cheap delta that doesn't scan (or
// unmarshal) the whole scope, which matters for incremental folding.
func (s *SQLiteStore) Since(scope string, have VersionVector) []*Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.connLocked()
	if err != nil {
		return nil
	}
	ar, err := d.Query(`SELECT DISTINCT author FROM entries WHERE scope=?`, scope)
	if err != nil {
		return nil
	}
	var authors []string
	for ar.Next() {
		var a string
		if ar.Scan(&a) == nil {
			authors = append(authors, a)
		}
	}
	ar.Close()

	var out []*Entry
	for _, a := range authors {
		rows, err := d.Query(`SELECT id,scope,author,seq,prev,lamport,type,ts,payload,sig
			FROM entries WHERE scope=? AND author=? AND seq>? ORDER BY seq`, scope, a, have[a])
		if err != nil {
			continue
		}
		out = append(out, scanEntries(rows)...)
		rows.Close()
	}
	sortEntries(out)
	return out
}

func (s *SQLiteStore) MaxLamport() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.connLocked()
	if err != nil {
		return 0
	}
	var m sql.NullInt64
	if err := d.QueryRow(`SELECT MAX(lamport) FROM entries`).Scan(&m); err != nil || !m.Valid {
		return 0
	}
	return uint64(m.Int64)
}

func (s *SQLiteStore) Scopes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.connLocked()
	if err != nil {
		return nil
	}
	rows, err := d.Query(`SELECT DISTINCT scope FROM entries`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sc string
		if rows.Scan(&sc) == nil {
			out = append(out, sc)
		}
	}
	return out
}

type scannable interface{ Scan(...any) error }

func scanEntry(r scannable) (*Entry, error) {
	var e Entry
	var prev, sig sql.NullString
	var payload []byte
	if err := r.Scan(&e.ID, &e.Scope, &e.Author, &e.Seq, &prev, &e.Lamport, &e.Type, &e.TS, &payload, &sig); err != nil {
		return nil, err
	}
	e.Prev, e.Sig, e.Payload = prev.String, sig.String, payload
	return &e, nil
}

func scanEntries(rows *sql.Rows) []*Entry {
	var out []*Entry
	for rows.Next() {
		if e, err := scanEntry(rows); err == nil {
			out = append(out, e)
		}
	}
	return out
}
