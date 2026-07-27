package clog

import (
	"strings"
	"sync"
	"time"
)

// LogEntry is one line of output. Time is when the entry was captured;
// Message is the formatted text already produced by the standard logger
// (it still contains the time prefix log added). UI consumers display
// Message verbatim.
type LogEntry struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

// maxBacklog is how many recent log lines the tap retains so a fresh subscriber
// (a just-opened log view) gets immediate history instead of an empty screen
// until the next line is emitted.
const maxBacklog = 500

// logTap is an io.Writer that taps the log stream: every line written via
// log.Printf (routed through Init's MultiWriter) is broadcast to all active
// subscribers — the UI's diagnostics log stream. It also keeps a ring of the
// last maxBacklog entries, replayed to each new subscriber.
type logTap struct {
	mu   sync.RWMutex
	subs map[chan<- LogEntry]struct{}
	ring []LogEntry
}

func newLogTap() *logTap {
	return &logTap{subs: map[chan<- LogEntry]struct{}{}}
}

// tap is the process-wide log tap. It exists before Init so Subscribe never
// nil-panics; Init wires it into the log output.
var tap = newLogTap()

// Subscribe returns a channel of log entries plus an unsubscribe func. buf
// bounds the queue before slow-subscriber drops start. Used by the web layer
// for GET /api/log/stream.
func Subscribe(buf int) (<-chan LogEntry, func()) { return tap.Subscribe(buf) }

// Write parses one or more lines from p and broadcasts each as a LogEntry.
// Slow subscribers drop messages rather than block the writer.
func (t *logTap) Write(p []byte) (int, error) {
	now := time.Now()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		entry := LogEntry{Time: now, Message: line}
		t.mu.Lock()
		t.ring = append(t.ring, entry)
		if len(t.ring) > maxBacklog {
			// Reslice with a fresh backing array so old entries can be GC'd
			// rather than pinned forever behind a growing slice.
			t.ring = append(t.ring[:0:0], t.ring[len(t.ring)-maxBacklog:]...)
		}
		for ch := range t.subs {
			select {
			case ch <- entry:
			default: // slow subscriber — drop rather than block the log path
			}
		}
		t.mu.Unlock()
	}
	return len(p), nil
}

func (t *logTap) Subscribe(buf int) (<-chan LogEntry, func()) {
	t.mu.Lock()
	// Prefill with the retained backlog so the subscriber sees recent history
	// immediately. Size the channel to fit backlog + the caller's live headroom
	// so the prefill never blocks.
	backlog := t.ring
	ch := make(chan LogEntry, len(backlog)+buf)
	for _, e := range backlog {
		ch <- e
	}
	t.subs[ch] = struct{}{}
	t.mu.Unlock()
	return ch, func() {
		t.mu.Lock()
		delete(t.subs, ch)
		t.mu.Unlock()
		close(ch)
	}
}
