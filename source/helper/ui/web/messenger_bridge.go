package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/vseplet/f2f/source/helper/db"
	"github.com/vseplet/f2f/source/helper/db/blocks/message"
)

var errNotInCamp = fmt.Errorf("not in a camp")

// Bridge between the block engine and the browser for the messenger feature:
// channels are channel blocks, messages are message blocks. Local writes echo
// to the browser from the API handlers; remotely-synced frames echo via
// OnFrameApplied (the db OnApply hook).

func (s *Server) selfPub() string { return s.engine.IdentityPub() }

// chanBID resolves a (kind, key) conversation to its channel block bid. For a
// channel, the key already IS the bid; for a DM, ensure the DM channel between
// us and the peer and return its deterministic bid.
func (s *Server) chanBID(kind, key string) (string, error) {
	if kind == "dm" {
		id := s.engine.Identity()
		if id == nil {
			return "", errNotInCamp
		}
		return s.channels.EnsureDM(id, id.PubHex(), key)
	}
	return key, nil
}

// dmPeer returns the other member of a DM channel (from our point of view).
func (s *Server) dmPeer(bid string) string {
	c := s.channels.Get(bid)
	if c == nil {
		return ""
	}
	for _, p := range c.Members {
		if p != s.selfPub() {
			return p
		}
	}
	return ""
}

// webMessage shapes a message into the JSON the messenger UI expects. editID is
// set (= the message id) for an edit so the UI patches in place instead of
// appending a duplicate.
func (s *Server) webMessage(m message.Message, kind, key, editID string) map[string]any {
	return map[string]any{
		"id": m.ID, "kind": kind, "peer": key, "type": "text",
		"from": m.From, "from_name": s.authorName(m.From), "to": "", "body": m.Body, "file": m.File,
		"reply_to": m.ReplyTo, "thread": m.Thread, "edit_id": editID,
		"ts": m.TS, "edited": m.Edited, "mine": m.From == s.selfPub(),
	}
}

// emitMessage folds a message and broadcasts it to connected browsers over the
// unified stream. op "update" marks an edit (UI patches in place).
func (s *Server) emitMessage(channelBID, msgBID, op string) {
	m := s.messages.Get(channelBID, msgBID)
	if m == nil {
		return // deleted/unknown
	}
	kind, key := "channel", channelBID
	if strings.HasPrefix(channelBID, "dm-") {
		kind, key = "dm", s.dmPeer(channelBID)
	}
	editID := ""
	if op == "update" {
		editID = m.ID
	}
	if b, err := json.Marshal(s.webMessage(*m, kind, key, editID)); err == nil {
		s.msgEvents.broadcast(b)
	}
}

// OnFrameApplied is the db OnApply hook: a frame just synced in from a peer.
// It live-refreshes open block views and pushes a message to the messenger UI
// (local writes echo from the API handlers instead).
func (s *Server) OnFrameApplied(e *db.Frame) {
	s.NotifyBlockChange(e.Scope)
	if !strings.HasPrefix(e.Scope, message.ScopePrefix) {
		return
	}
	var fo struct {
		BID string `json:"bid"`
		Op  string `json:"op"`
	}
	if json.Unmarshal(e.Payload, &fo) != nil || fo.BID == "" {
		return
	}
	bid := strings.TrimPrefix(e.Scope, message.ScopePrefix)
	s.emitMessage(bid, fo.BID, fo.Op)
}

// handleEventStream is the single always-on server→browser push stream:
// messages (msgEvents) and block-scope changes (blockEvents). One long-lived
// SSE per browser (extra persistent connections starve the HTTP/1.1 per-host
// budget). Call signalling and the diag log stay separate.
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	mch, munsub := s.msgEvents.subscribe()
	defer munsub()
	bch, bunsub := s.blockEvents.subscribe()
	defer bunsub()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case data, ok := <-mch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data) // a message (kind/peer/…)
			flusher.Flush()
		case data, ok := <-bch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data) // {"type":"blocks","scope":…}
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
