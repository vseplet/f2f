package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/vseplet/f2f/source/helper/db/blocks/channels"
)

// Channels API — channels are channel blocks (db/blocks/channels). DMs are
// channels too (deterministic "dm-…" bid) but aren't listed as rooms.

// GET /api/channels → the camp's rooms (excludes DMs). Shaped with id=bid for
// the messenger UI.
func (s *Server) handleChannelsList(w http.ResponseWriter, r *http.Request) {
	var out []map[string]any
	for _, c := range s.channels.List() {
		if strings.HasPrefix(c.BID, "dm-") {
			continue
		}
		out = append(out, channelJSON(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// channelJSON shapes a channel block into the messenger UI's Channel JSON
// (id == the channel block bid).
func channelJSON(c *channels.Channel) map[string]any {
	return map[string]any{
		"id": c.BID, "name": c.Name, "owner": c.Owner,
		"members": c.Members, "created_at": c.CreatedAt, "parent": c.Parent,
	}
}

// POST /api/channels {name,members,parent,pos} → the created channel.
func (s *Server) handleChannelsCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
		Parent  string   `json:"parent"`
		Pos     string   `json:"pos"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name required"))
		return
	}
	id, ok := s.blockSigner(w)
	if !ok {
		return
	}
	bid, err := s.channels.Create(id, req.Name, req.Parent, req.Pos)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	for _, p := range req.Members {
		_ = s.channels.AddMember(id, bid, p)
	}
	writeJSON(w, http.StatusOK, channelJSON(s.channels.Get(bid)))
}

// POST /api/channels/rename {bid,name}
func (s *Server) handleChannelsRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BID  string `json:"bid"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id, ok := s.blockSigner(w)
	if !ok {
		return
	}
	if err := s.channels.Rename(id, req.BID, req.Name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/channels/members {bid,add,remove}
func (s *Server) handleChannelsMembers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BID    string   `json:"bid"`
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id, ok := s.blockSigner(w)
	if !ok {
		return
	}
	for _, p := range req.Add {
		if err := s.channels.AddMember(id, req.BID, p); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	for _, p := range req.Remove {
		if err := s.channels.RemoveMember(id, req.BID, p); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/channels/delete {bid}
func (s *Server) handleChannelsDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BID string `json:"bid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id, ok := s.blockSigner(w)
	if !ok {
		return
	}
	if err := s.channels.Delete(id, req.BID); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/channels/dm {peer} → {bid} — ensure (idempotent) the DM channel
// with a peer and return its deterministic bid.
func (s *Server) handleChannelsDM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Peer string `json:"peer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Peer == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("peer required"))
		return
	}
	id, ok := s.blockSigner(w)
	if !ok {
		return
	}
	bid, err := s.channels.EnsureDM(id, id.PubHex(), req.Peer)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"bid": bid})
}
