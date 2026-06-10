package messenger

// SQLite persistence, one database per camp. NOT wired into the live flow
// yet — the Service keeps state in memory for now; this is here so flipping
// on persistence later is a small change, not a redesign. Pure-Go
// modernc.org/sqlite driver (no cgo) keeps the binary cross-platform.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// Store manages one SQLite database per camp under a base directory.
// Construct once with NewStore; safe for concurrent use.
type Store struct {
	campDir func(campID string) string // ~/.f2f/<camp_id>/ (creates it)

	mu  sync.Mutex
	dbs map[string]*sql.DB // camp_id → handle (lazily opened)
}

// NewStore returns a Store. campDir resolves (and creates) a camp's
// directory — typically config.Store.CampDir. No file is touched until a
// camp's database is first used.
func NewStore(campDir func(campID string) string) *Store {
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
  id        TEXT    NOT NULL PRIMARY KEY,
  kind      TEXT    NOT NULL,            -- 'dm' | 'channel'
  peer      TEXT    NOT NULL,            -- dm: peer pub; channel: channel id
  mtype     TEXT    NOT NULL DEFAULT 'text',
  sender    TEXT    NOT NULL DEFAULT '', -- author pub (Message.From)
  recipient TEXT    NOT NULL DEFAULT '', -- dm recipient pub (Message.To)
  body      TEXT    NOT NULL DEFAULT '',
  members   TEXT    NOT NULL DEFAULT '', -- JSON array of member pubs
  ts        INTEGER NOT NULL,            -- unix ms
  mine      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_messages_conv ON messages(kind, peer, ts);

CREATE TABLE IF NOT EXISTS channels (
  id         TEXT    NOT NULL PRIMARY KEY,
  name       TEXT    NOT NULL DEFAULT '',
  owner      TEXT    NOT NULL DEFAULT '',
  members    TEXT    NOT NULL DEFAULT '', -- JSON array of member pubs
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS outbox (
  msg_id    TEXT    NOT NULL,            -- message ID (dedup key on the peer)
  recipient TEXT    NOT NULL,            -- peer pub still owed this message
  payload   TEXT    NOT NULL,            -- full wire JSON, resent verbatim
  ts        INTEGER NOT NULL,            -- unix ms enqueued, for pruning
  PRIMARY KEY (msg_id, recipient)
);`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("messenger: migrate: %w", err)
	}
	return nil
}

// AddMessage inserts a message into campID's database (stamping ts if
// unset). The caller supplies the id.
func (s *Store) AddMessage(campID string, m Message) error {
	db, err := s.dbFor(campID)
	if err != nil {
		return err
	}
	if m.TS == 0 {
		m.TS = time.Now().UnixMilli()
	}
	_, err = db.Exec(
		`INSERT INTO messages(id, kind, peer, mtype, sender, recipient, body, members, ts, mine)
		 VALUES(?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO NOTHING`,
		m.ID, m.Kind, m.Peer, m.Type, m.From, m.To, m.Body, jsonArr(m.Members), m.TS, b2i(m.Mine))
	if err != nil {
		return fmt.Errorf("messenger: add message: %w", err)
	}
	return nil
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
		`SELECT id, kind, peer, mtype, sender, recipient, body, members, ts, mine
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
		var members string
		var mine int
		if err := rows.Scan(&m.ID, &m.Kind, &m.Peer, &m.Type, &m.From, &m.To, &m.Body, &members, &m.TS, &mine); err != nil {
			return nil, err
		}
		m.Members = parseArr(members)
		m.Mine = mine != 0
		out = append(out, m)
	}
	// oldest-first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// AllMessages returns every message in campID's database, oldest-first —
// used to hydrate the in-memory store at startup.
func (s *Store) AllMessages(campID string) ([]Message, error) {
	db, err := s.dbFor(campID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(
		`SELECT id, kind, peer, mtype, sender, recipient, body, members, ts, mine
		   FROM messages ORDER BY ts ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("messenger: all messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var members string
		var mine int
		if err := rows.Scan(&m.ID, &m.Kind, &m.Peer, &m.Type, &m.From, &m.To, &m.Body, &members, &m.TS, &mine); err != nil {
			return nil, err
		}
		m.Members = parseArr(members)
		m.Mine = mine != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteChannel removes a channel row and all its messages from campID.
func (s *Store) DeleteChannel(campID, id string) error {
	db, err := s.dbFor(campID)
	if err != nil {
		return err
	}
	if _, err := db.Exec(`DELETE FROM channels WHERE id=?`, id); err != nil {
		return fmt.Errorf("messenger: delete channel: %w", err)
	}
	_, err = db.Exec(`DELETE FROM messages WHERE kind='channel' AND peer=?`, id)
	return err
}

// UpsertChannel creates or updates a channel in campID's database.
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
		`INSERT INTO channels(id, name, owner, members, created_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, owner=excluded.owner, members=excluded.members`,
		c.ID, c.Name, c.Owner, jsonArr(c.Members), c.CreatedAt)
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
	rows, err := db.Query(`SELECT id, name, owner, members, created_at FROM channels ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("messenger: channels: %w", err)
	}
	defer rows.Close()

	var out []Channel
	for rows.Next() {
		var c Channel
		var members string
		if err := rows.Scan(&c.ID, &c.Name, &c.Owner, &members, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.Members = parseArr(members)
		out = append(out, c)
	}
	return out, rows.Err()
}

// OutboxItem is one undelivered (message, recipient) pair.
type OutboxItem struct {
	MsgID     string
	Recipient string
	Payload   []byte
	TS        int64
}

// AddOutbox records an undelivered message for a recipient (idempotent).
func (s *Store) AddOutbox(campID string, it OutboxItem) error {
	db, err := s.dbFor(campID)
	if err != nil {
		return err
	}
	_, err = db.Exec(
		`INSERT INTO outbox(msg_id, recipient, payload, ts) VALUES(?,?,?,?)
		 ON CONFLICT(msg_id, recipient) DO NOTHING`,
		it.MsgID, it.Recipient, string(it.Payload), it.TS)
	return err
}

// Outbox returns every undelivered item, oldest-first.
func (s *Store) Outbox(campID string) ([]OutboxItem, error) {
	db, err := s.dbFor(campID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT msg_id, recipient, payload, ts FROM outbox ORDER BY ts ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxItem
	for rows.Next() {
		var it OutboxItem
		var payload string
		if err := rows.Scan(&it.MsgID, &it.Recipient, &payload, &it.TS); err != nil {
			return nil, err
		}
		it.Payload = []byte(payload)
		out = append(out, it)
	}
	return out, rows.Err()
}

// DeleteOutbox drops one delivered (message, recipient) pair.
func (s *Store) DeleteOutbox(campID, msgID, recipient string) error {
	db, err := s.dbFor(campID)
	if err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM outbox WHERE msg_id=? AND recipient=?`, msgID, recipient)
	return err
}

// PruneOutbox drops items enqueued before cutoff (unix ms) — a recipient
// that hasn't been seen for that long forfeits the backlog.
func (s *Store) PruneOutbox(campID string, cutoff int64) error {
	db, err := s.dbFor(campID)
	if err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM outbox WHERE ts < ?`, cutoff)
	return err
}

func jsonArr(v []string) string {
	if len(v) == 0 {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func parseArr(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
