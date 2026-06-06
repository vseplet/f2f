// Package messenger is the local persistence layer for messages, channels
// and direct conversations. Each camp gets its own SQLite file under ~/.f2f
// (<camp_id>/messenger.db), mirroring the per-camp config files, opened
// lazily and cached. Pure-Go modernc.org/sqlite driver (no cgo) keeps the
// binary cross-platform.
//
// This is the storage foundation only — the engine/web layers wire it to
// the wire protocol and the UI on top.
package messenger

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// Message is one line in a DM or channel conversation. The owning camp is
// the database file, so it isn't stored on the row.
type Message struct {
	ID         int64  `json:"id"`
	Kind       string `json:"kind"` // "dm" | "channel"
	Peer       string `json:"peer"` // dm: peer key/pub; channel: channel id
	AuthorPub  string `json:"author_pub"`
	AuthorName string `json:"author_name"`
	Body       string `json:"body"`
	TS         int64  `json:"ts"` // unix ms
	Mine       bool   `json:"mine"`
}

// Channel is a named room within a camp.
type Channel struct {
	ID        string `json:"id"` // slug, unique within the camp
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"` // unix ms
}

// Store manages one SQLite database per camp under a base directory.
// Construct once with New; safe for concurrent use.
type Store struct {
	campDir func(campID string) string // ~/.f2f/<camp_id>/ (creates it)

	mu  sync.Mutex
	dbs map[string]*sql.DB // camp_id → handle (lazily opened)
}

// New returns a Store. campDir resolves (and creates) a camp's directory —
// typically config.Store.CampDir. No file is touched until a camp's
// database is first used.
func New(campDir func(campID string) string) *Store {
	return &Store{campDir: campDir, dbs: make(map[string]*sql.DB)}
}

// Close closes every open camp database.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for id, db := range s.dbs {
		if err := db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(s.dbs, id)
	}
	return firstErr
}

// dbFor returns the (lazily opened, migrated, cached) handle for campID.
func (s *Store) dbFor(campID string) (*sql.DB, error) {
	campID = strings.TrimSpace(campID)
	if campID == "" {
		campID = "_root"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if db := s.dbs[campID]; db != nil {
		return db, nil
	}
	path := filepath.Join(s.campDir(campID), "messenger.db")
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("messenger: open %q: %w", path, err)
	}
	db.SetMaxOpenConns(1) // SQLite: serialise writers, avoid "database is locked"
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	s.dbs[campID] = db
	return db, nil
}

func migrate(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS messages (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  kind        TEXT    NOT NULL,            -- 'dm' | 'channel'
  peer        TEXT    NOT NULL,            -- dm: peer key/pub; channel: id
  author_pub  TEXT    NOT NULL DEFAULT '',
  author_name TEXT    NOT NULL DEFAULT '',
  body        TEXT    NOT NULL,
  ts          INTEGER NOT NULL,            -- unix ms
  mine        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_messages_conv ON messages(kind, peer, ts);

CREATE TABLE IF NOT EXISTS channels (
  id         TEXT    NOT NULL PRIMARY KEY,
  name       TEXT    NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("messenger: migrate: %w", err)
	}
	return nil
}

// AddMessage inserts a message into campID's database (stamping ts if
// unset) and returns its id.
func (s *Store) AddMessage(campID string, m Message) (int64, error) {
	db, err := s.dbFor(campID)
	if err != nil {
		return 0, err
	}
	if m.TS == 0 {
		m.TS = time.Now().UnixMilli()
	}
	res, err := db.Exec(
		`INSERT INTO messages(kind, peer, author_pub, author_name, body, ts, mine)
		 VALUES(?,?,?,?,?,?,?)`,
		m.Kind, m.Peer, m.AuthorPub, m.AuthorName, m.Body, m.TS, b2i(m.Mine))
	if err != nil {
		return 0, fmt.Errorf("messenger: add message: %w", err)
	}
	return res.LastInsertId()
}

// Messages returns up to limit recent messages for a conversation in
// campID, oldest-first (ready to render). limit <= 0 defaults to 200.
func (s *Store) Messages(campID, kind, peer string, limit int) ([]Message, error) {
	db, err := s.dbFor(campID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := db.Query(
		`SELECT id, kind, peer, author_pub, author_name, body, ts, mine
		   FROM messages
		  WHERE kind=? AND peer=?
		  ORDER BY ts DESC, id DESC
		  LIMIT ?`,
		kind, peer, limit)
	if err != nil {
		return nil, fmt.Errorf("messenger: messages: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		var mine int
		if err := rows.Scan(&m.ID, &m.Kind, &m.Peer, &m.AuthorPub, &m.AuthorName, &m.Body, &m.TS, &mine); err != nil {
			return nil, err
		}
		m.Mine = mine != 0
		out = append(out, m)
	}
	// oldest-first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// UpsertChannel creates or renames a channel in campID's database.
func (s *Store) UpsertChannel(campID string, c Channel) error {
	db, err := s.dbFor(campID)
	if err != nil {
		return err
	}
	if c.ID == "" {
		return errors.New("messenger: channel id required")
	}
	if c.CreatedAt == 0 {
		c.CreatedAt = time.Now().UnixMilli()
	}
	_, err = db.Exec(
		`INSERT INTO channels(id, name, created_at) VALUES(?,?,?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name`,
		c.ID, c.Name, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("messenger: upsert channel: %w", err)
	}
	return nil
}

// Channels lists campID's channels, newest-first.
func (s *Store) Channels(campID string) ([]Channel, error) {
	db, err := s.dbFor(campID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, name, created_at FROM channels ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("messenger: channels: %w", err)
	}
	defer rows.Close()

	var out []Channel
	for rows.Next() {
		var c Channel
		if err := rows.Scan(&c.ID, &c.Name, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
