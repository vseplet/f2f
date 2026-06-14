package web

// HTTP surface for the messaging service (services/messenger). All
// loopback-only: the browser talks to its own f2f, which carries messages
// to peers over the QUIC bus. No peer↔peer HTTP.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

// handleChatGetNotes returns a conversation's shared notes doc.
// Query: key=<channel id | peer pub>.
// noteDoc mirrors the old messenger.NoteDoc JSON so the notes UI is
// unchanged. Notes are now a block: a single "note" text block per
// conversation scope, in the db log, synced by db anti-entropy.
type noteDoc struct {
	Scope string `json:"scope"`
	Body  string `json:"body"`
	TS    int64  `json:"ts"`
	By    string `json:"by"`
}

const noteBID = "note" // singleton notes block per conversation scope

func (s *Server) noteDocFor(scope string) noteDoc {
	d := noteDoc{Scope: scope}
	b := s.blocks.Block(scope, noteBID)
	if b == nil || len(b.Heads) == 0 {
		return d
	}
	h := b.Heads[len(b.Heads)-1] // latest head = deterministic default of any variants
	var c struct {
		Md string `json:"md"`
	}
	_ = json.Unmarshal(h.Content, &c)
	d.Body, d.TS, d.By = c.Md, h.TS, h.Author
	return d
}

func (s *Server) handleChatGetNotes(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("key required"))
		return
	}
	writeJSON(w, http.StatusOK, s.noteDocFor(key))
}

// handleChatNotes sets a conversation's notes (the "note" block in that
// scope). Any member may edit; concurrent edits become block variants and
// converge via the block engine. The commit is pushed to peers by db sync.
func (s *Server) handleChatNotes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key  string `json:"key"`
		Body string `json:"body"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // notes are text — 1 MiB is plenty
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Key == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("key required"))
		return
	}
	id := s.engine.Identity()
	if id == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("not in a camp"))
		return
	}
	content, _ := json.Marshal(struct {
		Md string `json:"md"`
	}{req.Body})
	if err := s.blocks.Upsert(id, req.Key, noteBID, "text", content); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.noteDocFor(req.Key))
}

// handleChatQuery runs a read-only SQL query against the camp's messenger.db
// and returns columns + rows. Loopback UI only; read-only is enforced by the
// store (SQLite mode=ro). A SQL error is returned as a 400 with its text.
func (s *Server) handleChatQuery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SQL string `json:"sql"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.SQL == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("sql required"))
		return
	}
	res, err := s.msg.Query(req.SQL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleChatClear wipes a conversation's messages locally (memory + SQLite).
// Body: {kind, key}. Peers keep their copies — nothing is sent.
func (s *Server) handleChatClear(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind string `json:"kind"`
		Key  string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Key == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("key required"))
		return
	}
	if err := s.msg.ClearConversation(req.Kind, req.Key); err != nil {
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
		Kind    string                `json:"kind"`
		Key     string                `json:"key"`
		Body    string                `json:"body"`
		Type    string                `json:"type"`
		ReplyTo string                `json:"reply_to"`
		Thread  string                `json:"thread"`
		EditID  string                `json:"edit_id"`
		File    *messenger.Attachment `json:"file"`
	}
	// Cap the body generously above MaxAttachment (base64 + JSON overhead) so
	// an oversized upload is rejected cleanly rather than read into memory.
	r.Body = http.MaxBytesReader(w, r.Body, messenger.MaxAttachment*2)
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
	if req.File != nil {
		if len(req.File.Data) == 0 {
			req.File = nil // empty attachment — treat as a plain message
		} else if len(req.File.Data) > messenger.MaxAttachment {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Errorf("attachment too large (max %d MiB)", messenger.MaxAttachment>>20))
			return
		} else {
			req.File.Size = len(req.File.Data)
		}
	}
	var err error
	switch req.Type {
	case "": // plain text and/or attachment
		if req.Body == "" && req.File == nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("body or file required"))
			return
		}
		if req.Kind == "dm" {
			_, err = s.msg.SendDM(req.Key, req.Body, req.File, req.ReplyTo, req.Thread, req.EditID)
		} else {
			_, err = s.msg.Post(req.Key, req.Body, req.File, req.ReplyTo, req.Thread, req.EditID)
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

// handleChatShare shares a large file into a conversation over torrent.
// multipart/form-data: file + kind (dm|channel) + key + optional body caption.
// The file is seeded (scoped to this conversation so it stays out of the
// public catalog) and a message carrying its torrent metadata is posted; the
// recipient downloads it over the drop transport from the chat.
func (s *Server) handleChatShare(w http.ResponseWriter, r *http.Request) {
	t := s.drop.Client()
	if t == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("torrent client not running"))
		return
	}
	if err := r.ParseMultipartForm(8 << 30); err != nil { // 8 GiB cap on one share
		writeError(w, http.StatusBadRequest, err)
		return
	}
	defer r.MultipartForm.RemoveAll()
	kind, key := r.FormValue("kind"), r.FormValue("key")
	if kind != "dm" && kind != "channel" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("kind must be dm or channel"))
		return
	}
	if key == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("key required"))
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	defer file.Close()
	dstPath := filepath.Join(t.SharedDir(), filepath.Base(hdr.Filename))
	dst, err := os.Create(dstPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := io.Copy(dst, file); err != nil {
		dst.Close()
		os.Remove(dstPath)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	dst.Close()
	// Seed it pinned to this conversation, then announce it as a message.
	pf, err := s.drop.ShareToScope(dstPath, key)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	att := &messenger.Attachment{
		Name:     pf.Name,
		Mime:     hdr.Header.Get("Content-Type"),
		Size:     int(pf.Size),
		InfoHash: pf.InfoHash,
		Magnet:   pf.Magnet,
	}
	body := r.FormValue("body")
	replyTo := r.FormValue("reply_to")
	thread := r.FormValue("thread")
	editID := r.FormValue("edit_id")
	if kind == "dm" {
		_, err = s.msg.SendDM(key, body, att, replyTo, thread, editID)
	} else {
		_, err = s.msg.Post(key, body, att, replyTo, thread, editID)
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
