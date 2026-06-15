package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/vseplet/f2f/source/helper/db/blocks/attach"
)

// Note attachments — a note carries a file as a standalone block.file in its
// "note:<conv>" scope (Notion-style: the file is its own block among the text
// blocks, not a field on a text block). Transport mirrors messages: small files
// inline (base64 Data ≤ attach.Max), large ones seeded over torrent via the
// drop. Both produce a block.file the editor renders inline.

// fileBlock is the block.file content: the shared attachment plus an optional
// caption carried on the block itself (covers the "image + caption as one
// atom" case without a separate text block).
type fileBlock struct {
	attach.Attachment
	Caption string `json:"caption,omitempty"`
}

// POST /api/notes/attach {channel,parent,pos,caption,file} → {bid}. Inline
// attachment (base64 bytes in file.data); creates a block.file in the note.
func (s *Server) handleNotesAttach(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string             `json:"channel"`
		Parent  string             `json:"parent"`
		Pos     string             `json:"pos"`
		Caption string             `json:"caption"`
		File    *attach.Attachment `json:"file"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, attach.Max*2)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Channel == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("channel required"))
		return
	}
	file, err := attach.Check(req.File)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	if file == nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("file required"))
		return
	}
	id, ok := s.blockSigner(w)
	if !ok {
		return
	}
	bid, err := s.createFileBlock(id, req.Channel, req.Parent, req.Pos, fileBlock{Attachment: *file, Caption: req.Caption})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"bid": bid})
}

// POST /api/notes/share — multipart upload seeded over torrent, then recorded
// as a block.file carrying the magnet. Form: channel,parent,pos,caption,file.
func (s *Server) handleNotesShare(w http.ResponseWriter, r *http.Request) {
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
	channel := r.FormValue("channel")
	if channel == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("channel required"))
		return
	}
	id, ok := s.blockSigner(w)
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
	pf, err := s.drop.ShareToScope(dstPath, channel) // seed pinned to the note scope
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	fb := fileBlock{
		Attachment: attach.Attachment{
			Name: pf.Name, Mime: hdr.Header.Get("Content-Type"), Size: int(pf.Size),
			InfoHash: pf.InfoHash, Magnet: pf.Magnet,
		},
		Caption: r.FormValue("caption"),
	}
	bid, err := s.createFileBlock(id, channel, r.FormValue("parent"), r.FormValue("pos"), fb)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"bid": bid})
}

// createFileBlock marshals a fileBlock and creates a block.file in the scope.
func (s *Server) createFileBlock(id signer, channel, parent, pos string, fb fileBlock) (string, error) {
	c, err := json.Marshal(fb)
	if err != nil {
		return "", err
	}
	return s.blocks.Create(id, channel, "file", c, parent, pos)
}
