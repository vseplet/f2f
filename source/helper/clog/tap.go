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

// logTap is an io.Writer that taps the log stream: every line written via
// log.Printf (routed through Init's MultiWriter) is broadcast to all active
// subscribers — the UI's diagnostics log stream.
type logTap struct {
	mu   sync.RWMutex
	subs map[chan<- LogEntry]struct{}
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
		t.mu.RLock()
		for ch := range t.subs {
			select {
			case ch <- entry:
			default: // slow subscriber — drop rather than block the log path
			}
		}
		t.mu.RUnlock()
	}
	return len(p), nil
}

func (t *logTap) Subscribe(buf int) (<-chan LogEntry, func()) {
	ch := make(chan LogEntry, buf)
	t.mu.Lock()
	t.subs[ch] = struct{}{}
	t.mu.Unlock()
	return ch, func() {
		t.mu.Lock()
		delete(t.subs, ch)
		t.mu.Unlock()
		close(ch)
	}
}
