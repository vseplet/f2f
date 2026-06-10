package web

// HTTP surface for the messaging service (services/messenger). All
// loopback-only: the browser talks to its own f2f, which carries messages
// to peers over the QUIC bus. No peer↔peer HTTP.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/vseplet/f2f/source/helper/services/messenger"
)

// handleChatChannels lists the channels we belong to.
func (s *Server) handleChatChannels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.msg.Channels())
}

// handleChatCreateChannel creates a named channel owned by us.
func (s *Server) handleChatCreateChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ch, err := s.msg.CreateChannel(req.Name, req.Members)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

// handleChatMembers adds and/or removes channel members (owner only).
func (s *Server) handleChatMembers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string   `json:"channel"`
		Add     []string `json:"add"`
		Remove  []string `json:"remove"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Add) > 0 {
		if _, err := s.msg.AddMembers(req.Channel, req.Add); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	if len(req.Remove) > 0 {
		if _, err := s.msg.RemoveMembers(req.Channel, req.Remove); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleChatDeleteChannel tears a channel down (owner only).
func (s *Server) handleChatDeleteChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.msg.DeleteChannel(req.Channel); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleChatLeaveChannel removes us from a channel.
func (s *Server) handleChatLeaveChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.msg.LeaveChannel(req.Channel); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleChatMessages returns recent messages for a conversation.
// Query: kind=dm|channel, key=<peer pub | channel id>, limit=<n>.
func (s *Server) handleChatMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind, key := q.Get("kind"), q.Get("key")
	if kind == "" || key == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("kind and key required"))
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	writeJSON(w, http.StatusOK, s.msg.Messages(kind, key, limit))
}

// handleChatSend posts a message. Body: {kind, key, body} — kind "dm"
// (key = peer pub) or "channel" (key = channel id). An optional type turns
// it into a system event (call lifecycle); body is ignored then.
func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind string `json:"kind"`
		Key  string `json:"key"`
		Body string `json:"body"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Kind != "dm" && req.Kind != "channel" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("kind must be dm or channel"))
		return
	}
	if req.Key == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("key required"))
		return
	}
	var err error
	switch req.Type {
	case "": // plain text
		if req.Body == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("body required"))
			return
		}
		if req.Kind == "dm" {
			_, err = s.msg.SendDM(req.Key, req.Body)
		} else {
			_, err = s.msg.Post(req.Key, req.Body)
		}
	case messenger.TypeCallStart, messenger.TypeCallEnd:
		_, err = s.msg.SendEvent(req.Kind, req.Key, req.Type)
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad type %q", req.Type))
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleChatStream is the SSE feed of every new message (local echoes and
// inbound). The browser filters to the open conversation; new traffic in
// other conversations drives unread badges.
func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch, unsubscribe := s.msg.Subscribe(64)
	defer unsubscribe()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case m, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(m)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
