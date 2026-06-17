package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/vseplet/f2f/source/helper/config"
)

// User profile — the peer's identity in this camp. Stored as a block.profile in
// the well-known "profiles" scope, keyed by peer_pub (self-authored, replicated
// to all camp members). Model: peer = user — no user_id indirection. The
// passkey's PUBLIC credential lives here too (see profile_passkey.go), NOT in
// OIDC's passkeys.json. See docs/IDENTITY.md.

const profileScope = "profiles"

// profileContent is the block.profile payload. first/last is the display name;
// the username/handle is the peer name (Identity.Name) — peer = user, so no
// separate nickname. passkeys holds PUBLIC WebAuthn credentials only.
type profileContent struct {
	First    string                `json:"first"`
	Last     string                `json:"last"`
	Passkeys []webauthn.Credential `json:"passkeys,omitempty"`
}

// profilePub returns this peer's identity pub (the profile block's key), or "".
func (s *Server) profilePub() string { return s.engine.IdentityPub() }

// profileDisplayName is the human label for the passkey (shown in the OS
// authenticator): full name, else the device/peer name.
func (s *Server) profileDisplayName() string {
	if pub := s.profilePub(); pub != "" {
		c := s.loadProfileContent(pub)
		if n := strings.TrimSpace(c.First + " " + c.Last); n != "" {
			return n
		}
	}
	if campID := s.engine.Status().CampID; campID != "" {
		if cc, _ := s.store.LoadCamp(campID); cc != nil && cc.Identity.Name != "" {
			return cc.Identity.Name
		}
	}
	return "f2f user"
}

// POST /api/profile/device {name} → renames THIS device (config.Identity.Name),
// pushing the new name into the running engine + camp announce so peers see it
// without a restart. The device/peer name is the username. Must be non-empty.
func (s *Server) handleDeviceRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("device name required"))
		return
	}
	campID := s.engine.Status().CampID
	if campID == "" {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("not in a camp"))
		return
	}
	if err := s.store.UpdateCamp(campID, func(c *config.Camp) { c.Identity.Name = req.Name }); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.engine.SetName(req.Name) // running engine: self-filter + pair_req/res
	s.camp.SetName(req.Name)   // camp announce: peers see it next tick
	writeJSON(w, http.StatusOK, map[string]any{"name": req.Name})
}

// GET /api/profile → {exists, first, last, has_passkey}. exists means the
// profile is filled (has a first name); has_passkey is independent (a passkey
// may exist before the name is saved).
func (s *Server) handleProfileGet(w http.ResponseWriter, r *http.Request) {
	pub := s.profilePub()
	if pub == "" {
		writeJSON(w, http.StatusOK, map[string]any{"exists": false})
		return
	}
	c := s.loadProfileContent(pub)
	writeJSON(w, http.StatusOK, map[string]any{
		"exists":      c.First != "",
		"first":       c.First,
		"last":        c.Last,
		"has_passkey": len(c.Passkeys) > 0,
	})
}

// POST /api/profile {first,last} → creates (or updates) the profile block keyed
// by this peer's pub, preserving any registered passkeys. 200 with the profile.
func (s *Server) handleProfileSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		First string `json:"first"`
		Last  string `json:"last"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.First = strings.TrimSpace(req.First)
	req.Last = strings.TrimSpace(req.Last)
	if req.First == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("first name required"))
		return
	}
	id, ok := s.blockSigner(w)
	if !ok {
		return
	}
	campID := s.engine.Status().CampID
	if campID == "" {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("not in a camp"))
		return
	}
	pub := s.profilePub()
	if pub == "" {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("identity not ready"))
		return
	}
	// Preserve passkeys already registered into the profile block.
	out := s.loadProfileContent(pub)
	out.First, out.Last = req.First, req.Last
	content, err := json.Marshal(out)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.blocks.Upsert(id, profileScope, pub, "profile", content); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exists": true, "first": req.First, "last": req.Last,
	})
}
