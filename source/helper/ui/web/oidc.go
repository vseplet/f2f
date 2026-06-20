package web

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/vseplet/f2f/source/helper/identity"
	"github.com/vseplet/f2f/source/helper/services/oidc"
)

// handleOIDCInfo returns this peer's OIDC issuer + discovery URL (the
// values to paste into an app's OIDC config) and the registered client
// list. Issuer is the dedicated per-peer host auth-<fp>.<zone>.f2f.
func (s *Server) handleOIDCInfo(w http.ResponseWriter, r *http.Request) {
	label := s.dns.OIDCLabel()
	campID := s.engine.Status().CampID
	var issuer, discovery string
	if label != "" && campID != "" {
		issuer = "https://" + label + "." + identity.CampLabel(campID) + ".f2f"
		discovery = issuer + "/.well-known/openid-configuration"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":    issuer,
		"discovery": discovery,
		"clients":   s.oidc.ListClients(),
		"users":     s.oidc.PasskeyUsers(),
	})
}

// handleOIDCCreateClient registers an application. Confidential clients
// get a generated secret, returned once in the response.
func (s *Server) handleOIDCCreateClient(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string   `json:"name"`
		RedirectURIs []string `json:"redirect_uris"`
		LogoutURIs   []string `json:"logout_uris"`
		Public       bool     `json:"public"`
		PKCE         bool     `json:"pkce"`
		Channels     []string `json:"channels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	info, secret, err := s.oidc.CreateClient(oidc.ClientSpec{
		Name:         req.Name,
		RedirectURIs: req.RedirectURIs,
		LogoutURIs:   req.LogoutURIs,
		Confidential: !req.Public, // UI offers "Public Client"; default confidential
		ForcePKCE:    req.PKCE,
		Channels:     req.Channels,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":     info.ID,
		"client_name":   info.Name,
		"redirect_uris": info.RedirectURIs,
		"confidential":  info.Confidential,
		"client_secret": secret, // plaintext
	})
}

// handleOIDCSetChannels replaces an app's channel allowlist (who may log in).
func (s *Server) handleOIDCSetChannels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string   `json:"id"`
		Channels []string `json:"channels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("id required"))
		return
	}
	if err := s.oidc.SetClientChannels(req.ID, req.Channels); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleOIDCDeleteClient removes a registered application by id.
func (s *Server) handleOIDCDeleteClient(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("client id required"))
		return
	}
	if err := s.oidc.DeleteClient(id); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
