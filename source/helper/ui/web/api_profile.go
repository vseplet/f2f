package web

import (
	"bytes"
	"encoding/base64"
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
		"passkeys":    passkeyList(c.Passkeys),
	})
}

// passkeyList renders the enrolled credentials for the UI: the credential id
// (base64url, the stable handle the delete endpoint takes) and nothing secret.
// Only the id is exposed; the credential itself is a public key already synced
// to every peer, but the UI needs no more than an id to list + remove.
func passkeyList(creds []webauthn.Credential) []map[string]any {
	out := make([]map[string]any, 0, len(creds))
	for _, cr := range creds {
		out = append(out, map[string]any{"id": base64.RawURLEncoding.EncodeToString(cr.ID)})
	}
	return out
}

// POST /api/profile/passkey/delete {id} → removes the credential with that id
// (base64url) from block.profile. When the last one goes, has_passkey flips back
// to false and the UI re-offers "create passkey" — this is the recovery path
// after losing an authenticator: from any device still in the camp, drop the
// lost passkey and enrol a fresh one. Gated by the block signer (device
// identity), not by a passkey, so a lost passkey doesn't lock you out of this.
func (s *Server) handlePasskeyDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("passkey id required"))
		return
	}
	want, err := base64.RawURLEncoding.DecodeString(req.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad passkey id: %w", err))
		return
	}
	pub := s.profilePub()
	if pub == "" {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("identity not ready"))
		return
	}
	id, ok := s.blockSigner(w)
	if !ok {
		return
	}
	c := s.loadProfileContent(pub)
	kept := c.Passkeys[:0]
	removed := false
	for _, cr := range c.Passkeys {
		if bytes.Equal(cr.ID, want) {
			removed = true
			continue
		}
		kept = append(kept, cr)
	}
	if !removed {
		writeError(w, http.StatusNotFound, fmt.Errorf("no such passkey"))
		return
	}
	c.Passkeys = kept
	content, err := json.Marshal(c)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.blocks.Upsert(id, profileScope, pub, "profile", content); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"has_passkey": len(c.Passkeys) > 0})
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
