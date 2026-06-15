package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/vseplet/f2f/source/helper/db/blocks/message"
)

// Messages API — messages are message blocks in a channel's
// "message:<channelBid>" scope. A conversation is addressed by (kind, key):
// channel → key is the channel bid; dm → key is the peer pub (resolved to the
// DM channel bid server-side). Responses use the messenger UI's message shape.

// GET /api/messages?kind=&key=&limit= → messages (oldest first).
func (s *Server) handleMessagesList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind, key := q.Get("kind"), q.Get("key")
	if kind == "" || key == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("kind and key required"))
		return
	}
	bid, err := s.chanBID(kind, key)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	msgs := s.messages.Messages(bid)
	if limit, _ := strconv.Atoi(q.Get("limit")); limit > 0 && len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, s.webMessage(m, kind, key, ""))
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/messages {kind,key,body,file,reply_to,thread,edit_id} → posts a
// message (or edits one when edit_id is set). 204.
func (s *Server) handleMessagesPost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind    string              `json:"kind"`
		Key     string              `json:"key"`
		Body    string              `json:"body"`
		Type    string              `json:"type"` // legacy call events — ignored
		File    *message.Attachment `json:"file"`
		ReplyTo string              `json:"reply_to"`
		Thread  string              `json:"thread"`
		EditID  string              `json:"edit_id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, message.MaxAttachment*2)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Type != "" && req.Type != "text" {
		w.WriteHeader(http.StatusNoContent) // call lifecycle etc. — no chat line
		return
	}
	file, ok := checkAttachment(w, req.File)
	if !ok {
		return
	}
	req.File = file
	if req.Body == "" && req.File == nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("body or file required"))
		return
	}
	bid, id, ok2 := s.resolveSigner(w, req.Kind, req.Key)
	if !ok2 {
		return
	}
	if req.EditID != "" {
		if err := s.messages.Edit(id, bid, req.EditID, req.Body, req.File); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		s.emitMessage(bid, req.EditID, "update")
	} else {
		msgBID, err := s.messages.Post(id, bid, req.Body, req.File, req.ReplyTo, req.Thread)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		s.emitMessage(bid, msgBID, "create")
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/messages/share — multipart upload shared over torrent, then posted
// as a message carrying the torrent metadata. Form: kind,key,file,body,reply_to,thread.
func (s *Server) handleMessagesShare(w http.ResponseWriter, r *http.Request) {
	t := s.drop.Client()
	if t == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("torrent client not running"))
		return
	}
	if err := r.ParseMultipartForm(8 << 30); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	defer r.MultipartForm.RemoveAll()
	kind, key := r.FormValue("kind"), r.FormValue("key")
	if kind == "" || key == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("kind and key required"))
		return
	}
	bid, id, ok := s.resolveSigner(w, kind, key)
	if !ok {
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
	pf, err := s.drop.ShareToScope(dstPath, bid) // seed pinned to this channel
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	att := &message.Attachment{
		Name: pf.Name, Mime: hdr.Header.Get("Content-Type"), Size: int(pf.Size),
		InfoHash: pf.InfoHash, Magnet: pf.Magnet,
	}
	msgBID, err := s.messages.Post(id, bid, r.FormValue("body"), att, r.FormValue("reply_to"), r.FormValue("thread"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.emitMessage(bid, msgBID, "create")
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/messages/clear {kind,key} — tombstones every message in the
// conversation. Unlike the old messenger "clear", this is a real delete that
// propagates to peers (a replicated log has no local-only copy).
func (s *Server) handleMessagesClear(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind string `json:"kind"`
		Key  string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	bid, id, ok := s.resolveSigner(w, req.Kind, req.Key)
	if !ok {
		return
	}
	for _, m := range s.messages.Messages(bid) {
		_ = s.messages.Delete(id, bid, m.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/db/query {sql} → columns + rows. Read-only SQL console over the
// camp's db.sqlite (the block log) — a debugging aid.
func (s *Server) handleDBQuery(w http.ResponseWriter, r *http.Request) {
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
	res, err := s.db.Query(req.SQL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// resolveSigner resolves the conversation to a channel bid and the local
// signer, writing an error and returning ok=false if not in a camp.
func (s *Server) resolveSigner(w http.ResponseWriter, kind, key string) (bid string, id signer, ok bool) {
	sgn, ok := s.blockSigner(w)
	if !ok {
		return "", nil, false
	}
	bid, err := s.chanBID(kind, key)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return "", nil, false
	}
	return bid, sgn, true
}

// signer is the minimal author interface (satisfied by *identity.Identity).
type signer interface {
	Sign([]byte) []byte
	PubHex() string
}

// checkAttachment validates an inline attachment: drops an empty one, rejects
// an oversized one, and stamps its size. Returns (file, ok).
func checkAttachment(w http.ResponseWriter, f *message.Attachment) (*message.Attachment, bool) {
	if f == nil || len(f.Data) == 0 {
		return nil, true
	}
	if len(f.Data) > message.MaxAttachment {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Errorf("attachment too large (max %d MiB)", message.MaxAttachment>>20))
		return nil, false
	}
	f.Size = len(f.Data)
	return f, true
}
