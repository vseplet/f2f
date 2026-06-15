package web

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// NotifyBlockChange announces (to connected browsers) that a block scope
// changed because of remote sync, so an open editor can live-refresh. Wired
// from db.Service.OnApply in main; local edits update the UI optimistically
// and don't need this. Delivered over the chat SSE stream (see
// handleChatStream) to avoid a second persistent connection per browser.
func (s *Server) NotifyBlockChange(scope string) {
	data, err := json.Marshal(map[string]string{"type": "blocks", "scope": scope})
	if err != nil {
		return
	}
	s.blockEvents.broadcast(data)
}

// Block API over services/blocks — drives the block (Notion-style) editor.
// Channel = scope; the notes editor uses "note:<conversation>" scopes. All
// writes are authored by the local identity and pushed to peers by db sync.

func (s *Server) blockSigner(w http.ResponseWriter) (signer interface {
	Sign([]byte) []byte
	PubHex() string
}, ok bool) {
	id := s.engine.Identity()
	if id == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("not in a camp"))
		return nil, false
	}
	return id, true
}

// GET /api/notes/scope?kind=&key= → {scope}. Resolves a conversation to its
// note db scope via chanBID — the SAME bid messages use. For a DM that's the
// symmetric "note:dm-<hash(sorted pubs)>" (and EnsureDM creates its membership),
// so both peers share one note scope instead of each keying off the other's pub.
func (s *Server) handleNotesScope(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind, key := q.Get("kind"), q.Get("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("key required"))
		return
	}
	bid, err := s.chanBID(kind, key)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"scope": "note:" + bid})
}

// GET /api/notes?channel=<scope> → folded blocks (with heads/author/version).
func (s *Server) handleNotesList(w http.ResponseWriter, r *http.Request) {
	ch := r.URL.Query().Get("channel")
	if ch == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("channel required"))
		return
	}
	writeJSON(w, http.StatusOK, s.blocks.Blocks(ch))
}

// POST /api/notes {channel,type,content,parent,pos} → {bid}
func (s *Server) handleNotesCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string          `json:"channel"`
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
		Parent  string          `json:"parent"`
		Pos     string          `json:"pos"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Channel == "" || req.Type == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("channel and type required"))
		return
	}
	id, ok := s.blockSigner(w)
	if !ok {
		return
	}
	bid, err := s.blocks.Create(id, req.Channel, req.Type, req.Content, req.Parent, req.Pos)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"bid": bid})
}

// POST /api/notes/update {channel,bid,content}
func (s *Server) handleNotesUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string          `json:"channel"`
		BID     string          `json:"bid"`
		Type    string          `json:"type"` // optional: retype the block
		Content json.RawMessage `json:"content"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id, ok := s.blockSigner(w)
	if !ok {
		return
	}
	var err error
	if req.Type != "" {
		err = s.blocks.UpdateType(id, req.Channel, req.BID, req.Type, req.Content)
	} else {
		err = s.blocks.Update(id, req.Channel, req.BID, req.Content, nil)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/notes/move {channel,bid,pos}
func (s *Server) handleNotesMove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string `json:"channel"`
		BID     string `json:"bid"`
		Pos     string `json:"pos"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id, ok := s.blockSigner(w)
	if !ok {
		return
	}
	if err := s.blocks.Move(id, req.Channel, req.BID, req.Pos); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/notes/delete {channel,bid}
func (s *Server) handleNotesDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string `json:"channel"`
		BID     string `json:"bid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id, ok := s.blockSigner(w)
	if !ok {
		return
	}
	if err := s.blocks.Delete(id, req.Channel, req.BID, nil); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/notes/merge {channel,bid,content}
func (s *Server) handleNotesMerge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string          `json:"channel"`
		BID     string          `json:"bid"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id, ok := s.blockSigner(w)
	if !ok {
		return
	}
	if err := s.blocks.Merge(id, req.Channel, req.BID, req.Content); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
