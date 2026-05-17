package engine

import (
	"strings"
	"sync"
	"time"
)

// LogEntry is one line of output. Time is when the entry was captured;
// Message is the formatted text already produced by the standard logger
// (it still contains the time prefix the log package added). UI consumers
// typically just display Message verbatim.
type LogEntry struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

// logTap is an io.Writer that taps the global log output: every line the
// program writes via log.Printf goes through here, gets broadcast to all
// active subscribers, and is also passed through to whatever upstream
// writer (usually os.Stderr) the caller wires up via io.MultiWriter.
type logTap struct {
	mu   sync.RWMutex
	subs map[chan<- LogEntry]struct{}
}

func newLogTap() *logTap {
	return &logTap{subs: map[chan<- LogEntry]struct{}{}}
}

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
			default:
				// Subscriber is slow — drop rather than block the entire
				// log path.
			}
		}
		t.mu.RUnlock()
	}
	return len(p), nil
}

// Subscribe returns a channel of log entries plus a function to unsubscribe.
// The buffer controls how many entries can queue before drops start.
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
