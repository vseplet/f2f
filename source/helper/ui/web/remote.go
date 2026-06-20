package web

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/vseplet/f2f/source/helper/config"
)

// Remote-access exposure: which channels may open MY shell (terminal) and VNC
// (desktop). Stored in the per-camp config and applied live to the services.

type remoteExposure struct {
	Terminal []string `json:"terminal"` // channel bids that may open my shell
	Desktop  []string `json:"desktop"`  // channel bids that may open my desktop
}

// GET /api/remote/exposure → the channels my terminal/desktop are exposed to.
func (s *Server) handleRemoteExposure(w http.ResponseWriter, r *http.Request) {
	out := remoteExposure{Terminal: []string{}, Desktop: []string{}}
	if s.shell != nil {
		out.Terminal = s.shell.Channels()
	}
	if s.vnc != nil {
		out.Desktop = s.vnc.Channels()
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/remote/exposure {terminal:[], desktop:[]} → persist + apply.
func (s *Server) handleRemoteSetExposure(w http.ResponseWriter, r *http.Request) {
	campID := s.engine.Status().CampID
	if campID == "" {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("not in a camp"))
		return
	}
	var req remoteExposure
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Terminal == nil {
		req.Terminal = []string{}
	}
	if req.Desktop == nil {
		req.Desktop = []string{}
	}
	var cmd, addr string
	if err := s.store.UpdateCamp(campID, func(c *config.Camp) {
		c.Shell.Channels = req.Terminal
		c.Vnc.Channels = req.Desktop
		cmd, addr = c.Shell.Command, c.Vnc.Addr
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.shell != nil {
		s.shell.SetChannels(req.Terminal, cmd)
	}
	if s.vnc != nil {
		s.vnc.SetChannels(req.Desktop, addr)
	}
	writeJSON(w, http.StatusOK, req)
}
