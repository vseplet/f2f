package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// GET /api/secrets/targets → shareable targets for an environment: the camp's
// named channels AND direct messages (1:1), each with a friendly label. DMs
// carry the other peer's name; sharing to one = both peers see the environment.
func (s *Server) handleSecretTargets(w http.ResponseWriter, r *http.Request) {
	self := s.engine.Status().IdentityPub
	seen := map[string]bool{}
	out := []map[string]string{}
	for _, c := range s.channels.List() {
		if seen[c.BID] {
			continue
		}
		seen[c.BID] = true
		if strings.HasPrefix(c.BID, "dm-") {
			other := ""
			for _, m := range c.Members {
				if m != self {
					other = m
					break
				}
			}
			out = append(out, map[string]string{"bid": c.BID, "label": s.peerName(other), "kind": "dm"})
		} else {
			out = append(out, map[string]string{"bid": c.BID, "label": c.Name, "kind": "channel"})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/secrets            → our full vault→env→secret tree.
// GET /api/secrets?channel=<bid> → environments shared to that channel, by every
//                                  reachable member (remote ones read-only).
func (s *Server) handleSecretsList(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("secrets unavailable"))
		return
	}
	if bid := r.URL.Query().Get("channel"); bid != "" {
		writeJSON(w, http.StatusOK, s.secrets.FetchChannel(r.Context(), bid))
		return
	}
	owned, err := s.secrets.Local()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, owned)
}

// POST /api/secrets/vault {id?, name} → create (no id) or rename. Returns {id}.
func (s *Server) handleSecretVault(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("secrets unavailable"))
		return
	}
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name required"))
		return
	}
	if req.ID == "" {
		id, err := s.secrets.CreateVault(req.Name)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
		return
	}
	if err := s.secrets.RenameVault(req.ID, req.Name); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": req.ID})
}

// POST /api/secrets/vault/delete {id}
func (s *Server) handleSecretVaultDelete(w http.ResponseWriter, r *http.Request) {
	s.secretDelete(w, r, func(id string) error { return s.secrets.DeleteVault(id) })
}

// POST /api/secrets/env {id?, vault_id, name?, channels?} → create / rename /
// re-share an environment. No id → create {vault_id, name, channels?}. With id →
// rename (name set) and/or replace the shared-channel set (channels present;
// [] clears, nil leaves untouched). Returns {id}.
func (s *Server) handleSecretEnv(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("secrets unavailable"))
		return
	}
	var req struct {
		ID       string    `json:"id"`
		VaultID  string    `json:"vault_id"`
		Name     string    `json:"name"`
		Channels *[]string `json:"channels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.ID == "" {
		if req.VaultID == "" || req.Name == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("vault_id and name required"))
			return
		}
		var ch []string
		if req.Channels != nil {
			ch = *req.Channels
		}
		id, err := s.secrets.CreateEnv(req.VaultID, req.Name, ch)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
		return
	}
	if req.Name != "" {
		if err := s.secrets.RenameEnv(req.ID, req.Name); err != nil {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
	}
	if req.Channels != nil {
		if err := s.secrets.SetEnvChannels(req.ID, *req.Channels); err != nil {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": req.ID})
}

// POST /api/secrets/env/delete {id}
func (s *Server) handleSecretEnvDelete(w http.ResponseWriter, r *http.Request) {
	s.secretDelete(w, r, func(id string) error { return s.secrets.DeleteEnv(id) })
}

// POST /api/secrets/entry {id?, env_id, name, value} → create or edit a secret.
func (s *Server) handleSecretEntry(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("secrets unavailable"))
		return
	}
	var req struct {
		ID    string `json:"id"`
		EnvID string `json:"env_id"`
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name required"))
		return
	}
	if req.ID == "" {
		if req.EnvID == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("env_id required"))
			return
		}
		id, err := s.secrets.AddSecret(req.EnvID, req.Name, req.Value)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
		return
	}
	if err := s.secrets.SetSecret(req.ID, req.Name, req.Value); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": req.ID})
}

// POST /api/secrets/entry/delete {id}
func (s *Server) handleSecretEntryDelete(w http.ResponseWriter, r *http.Request) {
	s.secretDelete(w, r, func(id string) error { return s.secrets.DeleteSecret(id) })
}

// secretDelete is the shared {id}-body delete handler for vault/env/entry.
func (s *Server) secretDelete(w http.ResponseWriter, r *http.Request, del func(string) error) {
	if s.secrets == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("secrets unavailable"))
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("id required"))
		return
	}
	if err := del(req.ID); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
