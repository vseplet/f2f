// Package notify is the notification hub. Any source (the QUIC bus, calls,
// pki, drop, …) pushes a Notification; it's persisted in the active camp's
// SQLite database (~/.f2f/<camp_id>/notifications.db) and fanned out to
// subscribed UI streams (SSE).
//
// Storage is per-camp (lazily opened, like messenger): notifications belong
// to a camp. The SSE subscribers are process-global — the browser refetches
// the recent list when the camp changes.
package notify

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

const keepRecent = 500 // prune older notifications past this per camp

// Notification is one UI-facing event.
type Notification struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`            // "message" | "call" | "cert" | "peer" | "ping" | "system" …
	Title string `json:"title"`           // short headline
	Body  string `json:"body,omitempty"`  // optional detail
	From  string `json:"from,omitempty"`  // peer pub, if it originated from a peer
	Route string `json:"route,omitempty"` // UI hash route to open on click
	TS    int64  `json:"ts"`              // unix ms
}

// Service persists notifications per camp and fans new ones out to UI SSE.
type Service struct {
	campDir func(campID string) string // ~/.f2f/<camp_id>/ (creates it)
	current func() string              // active camp id

	mu   sync.Mutex
	dbs  map[string]*sql.DB // camp_id → notifications.db
	subs map[chan Notification]struct{}
}

// New constructs the hub. campDir resolves a camp's directory
// (config.Store.CampDir); current returns the active camp id.
func New(campDir func(campID string) string, current func() string) *Service {
	return &Service{
		campDir: campDir,
		current: current,
		dbs:     make(map[string]*sql.DB),
		subs:    make(map[chan Notification]struct{}),
	}
}

// Close closes every open camp database.
func (s *Service) Close() error {
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

func (s *Service) dbFor(campID string) (*sql.DB, error) {
	if campID == "" {
		campID = "_root"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if db := s.dbs[campID]; db != nil {
		return db, nil
	}
	path := s.campDir(campID) + "/notifications.db"
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("notify: open %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS notifications (
  id    INTEGER PRIMARY KEY AUTOINCREMENT,
  kind  TEXT    NOT NULL DEFAULT '',
  title TEXT    NOT NULL DEFAULT '',
  body  TEXT    NOT NULL DEFAULT '',
  from_pub TEXT NOT NULL DEFAULT '',
  route TEXT    NOT NULL DEFAULT '',
  ts    INTEGER NOT NULL
);`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("notify: migrate: %w", err)
	}
	s.dbs[campID] = db
	return db, nil
}

// Push records a notification in the active camp's DB (assigning id + ts)
// and delivers it to every current subscriber. Returns the stored value.
func (s *Service) Push(n Notification) Notification {
	if n.TS == 0 {
		n.TS = time.Now().UnixMilli()
	}
	camp := ""
	if s.current != nil {
		camp = s.current()
	}
	if db, err := s.dbFor(camp); err == nil {
		res, err := db.Exec(
			`INSERT INTO notifications(kind, title, body, from_pub, route, ts) VALUES(?,?,?,?,?,?)`,
			n.Kind, n.Title, n.Body, n.From, n.Route, n.TS)
		if err == nil {
			if id, e := res.LastInsertId(); e == nil {
				n.ID = strconv.FormatInt(id, 10)
				_, _ = db.Exec(`DELETE FROM notifications WHERE id <= ? - ?`, id, keepRecent)
			}
		}
	}

	s.mu.Lock()
	for ch := range s.subs {
		select {
		case ch <- n:
		default: // drop for a slow subscriber rather than block the pusher
		}
	}
	s.mu.Unlock()
	return n
}

// Recent returns the active camp's recent notifications, oldest-first.
func (s *Service) Recent() []Notification {
	camp := ""
	if s.current != nil {
		camp = s.current()
	}
	db, err := s.dbFor(camp)
	if err != nil {
		return nil
	}
	rows, err := db.Query(
		`SELECT id, kind, title, body, from_pub, route, ts FROM notifications ORDER BY id DESC LIMIT ?`,
		keepRecent)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		var n Notification
		var id int64
		if err := rows.Scan(&id, &n.Kind, &n.Title, &n.Body, &n.From, &n.Route, &n.TS); err != nil {
			return out
		}
		n.ID = strconv.FormatInt(id, 10)
		out = append(out, n)
	}
	// oldest-first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Subscribe returns a channel of new notifications and an unsubscribe func.
func (s *Service) Subscribe() (<-chan Notification, func()) {
	ch := make(chan Notification, 16)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, ch)
			close(ch)
			s.mu.Unlock()
		})
	}
}

// FromBus is the QUIC-bus handler for the "notify" message type: a peer
// pushed a notification to us. Signature matches bus.HandlerFunc.
func (s *Service) FromBus(fromPub string, payload []byte) ([]byte, error) {
	var n Notification
	if err := json.Unmarshal(payload, &n); err != nil {
		return nil, err
	}
	n.From = fromPub // trust the bus-attested sender, not the payload
	s.Push(n)
	return nil, nil
}
