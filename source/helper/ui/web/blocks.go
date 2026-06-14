package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// NotifyBlockChange announces (to connected browsers) that a block scope
// changed because of remote sync, so an open editor can live-refresh. Wired
// from db.Service.OnApply in main; local edits update the UI optimistically
// and don't need this.
func (s *Server) NotifyBlockChange(scope string) {
	data, err := json.Marshal(map[string]string{"scope": scope})
	if err != nil {
		return
	}
	s.blockEvents.broadcast(data)
}

// GET /api/blocks/stream — SSE of {scope} events on remote block changes.
func (s *Server) handleBlocksStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, unsubscribe := s.blockEvents.subscribe()
	defer unsubscribe()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case data, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
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

// GET /api/blocks?channel=<scope> → folded blocks (with heads/author/version).
func (s *Server) handleBlocksList(w http.ResponseWriter, r *http.Request) {
	ch := r.URL.Query().Get("channel")
	if ch == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("channel required"))
		return
	}
	writeJSON(w, http.StatusOK, s.blocks.Blocks(ch))
}

// POST /api/blocks {channel,type,content,parent,pos} → {bid}
func (s *Server) handleBlocksCreate(w http.ResponseWriter, r *http.Request) {
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

// POST /api/blocks/update {channel,bid,content}
func (s *Server) handleBlocksUpdate(w http.ResponseWriter, r *http.Request) {
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

// POST /api/blocks/move {channel,bid,pos}
func (s *Server) handleBlocksMove(w http.ResponseWriter, r *http.Request) {
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

// POST /api/blocks/delete {channel,bid}
func (s *Server) handleBlocksDelete(w http.ResponseWriter, r *http.Request) {
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

// POST /api/blocks/merge {channel,bid,content}
func (s *Server) handleBlocksMerge(w http.ResponseWriter, r *http.Request) {
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
