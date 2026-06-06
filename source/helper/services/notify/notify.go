// Package notify is the notification hub. Any source (the QUIC bus, calls,
// pki, drop, …) pushes a Notification; the service buffers the recent ones
// and fans every new one out to subscribed UI streams (SSE).
//
// It deliberately knows nothing about transport: the bus registers
// Service.FromBus as its "notify" handler, local subsystems call Push
// directly. The web layer exposes Recent()/Subscribe() over HTTP.
package notify

import (
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const maxRecent = 200

// Notification is one UI-facing event.
type Notification struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`            // "message" | "call" | "cert" | "peer" | "system" …
	Title string `json:"title"`           // short headline
	Body  string `json:"body,omitempty"`  // optional detail
	From  string `json:"from,omitempty"`  // peer pub, if it originated from a peer
	Route string `json:"route,omitempty"` // UI hash route to open on click
	TS    int64  `json:"ts"`              // unix ms
}

// Service buffers recent notifications and fans new ones out to subscribers.
type Service struct {
	seq atomic.Int64

	mu     sync.Mutex
	recent []Notification
	subs   map[chan Notification]struct{}
}

// New constructs an empty hub.
func New() *Service {
	return &Service{subs: make(map[chan Notification]struct{})}
}

// Push records a notification (assigning id + ts) and delivers it to every
// current subscriber. Returns the stored value.
func (s *Service) Push(n Notification) Notification {
	if n.TS == 0 {
		n.TS = time.Now().UnixMilli()
	}
	n.ID = strconv.FormatInt(s.seq.Add(1), 10)

	s.mu.Lock()
	s.recent = append(s.recent, n)
	if len(s.recent) > maxRecent {
		s.recent = s.recent[len(s.recent)-maxRecent:]
	}
	for ch := range s.subs {
		select {
		case ch <- n:
		default: // drop for a slow subscriber rather than block the pusher
		}
	}
	s.mu.Unlock()
	return n
}

// Recent returns the buffered notifications, oldest-first.
func (s *Service) Recent() []Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Notification, len(s.recent))
	copy(out, s.recent)
	return out
}

// Subscribe returns a channel of new notifications and an unsubscribe func.
// The UI's SSE handler ranges over the channel until the client disconnects.
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
// pushed a notification to us. Its signature matches bus.HandlerFunc, so
// main wires it with busSvc.Handle("notify", notifySvc.FromBus) without
// this package importing the bus.
func (s *Service) FromBus(fromPub string, payload []byte) ([]byte, error) {
	var n Notification
	if err := json.Unmarshal(payload, &n); err != nil {
		return nil, err
	}
	n.From = fromPub // trust the bus-attested sender, not the payload
	s.Push(n)
	return nil, nil
}
