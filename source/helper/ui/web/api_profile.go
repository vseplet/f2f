package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/vseplet/f2f/source/helper/config"
)

// User profile — the local user's identity in this camp. Stored as a
// block.user in scope "user:<user_id>" (replicated, so a user's devices share
// one profile); the active user_id is recorded per-device in the camp config
// (Camp.Profile). Slice A is name only — passkey/avatar come later. See
// docs/IDENTITY.md.

const profileScopePrefix = "user:"

// profileContent is the block.user payload (slice A: name + nickname). Nick is
// the user's handle and must differ from the device/peer name (Identity.Name) —
// one identifies a person, the other a machine; conflating them is confusing.
type profileContent struct {
	First string `json:"first"`
	Last  string `json:"last"`
	Nick  string `json:"nick"`
}

func profileScope(uid string) string { return profileScopePrefix + uid }

// newUserID mints a random user_id. It's not the passkey/device key — passkey
// will anchor it later; for now it just needs to be unique and stable.
func newUserID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "u-" + hex.EncodeToString(b[:]), nil
}

// currentProfileID returns the active user_id from the camp config, or "".
func (s *Server) currentProfileID() string {
	campID := s.engine.Status().CampID
	if campID == "" {
		return ""
	}
	c, err := s.store.LoadCamp(campID)
	if err != nil || c == nil {
		return ""
	}
	return c.Profile
}

// profileDisplayName is the human label for the passkey (shown in the OS
// authenticator): nickname, else full name, else the device name.
func (s *Server) profileDisplayName() string {
	if uid := s.currentProfileID(); uid != "" {
		if b := s.blocks.Block(profileScope(uid), uid); b != nil && len(b.Heads) > 0 {
			var c profileContent
			_ = json.Unmarshal(b.Heads[len(b.Heads)-1].Content, &c)
			if c.Nick != "" {
				return c.Nick
			}
			if n := strings.TrimSpace(c.First + " " + c.Last); n != "" {
				return n
			}
		}
	}
	if campID := s.engine.Status().CampID; campID != "" {
		if cc, _ := s.store.LoadCamp(campID); cc != nil && cc.Identity.Name != "" {
			return cc.Identity.Name
		}
	}
	return "f2f user"
}

// POST /api/profile/passkey/begin → CredentialCreationOptions for the browser.
func (s *Server) handlePasskeyBegin(w http.ResponseWriter, r *http.Request) {
	pub := s.engine.IdentityPub()
	if pub == "" {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("not in a camp"))
		return
	}
	opts, err := s.oidc.RegisterBegin(r, pub, s.profileDisplayName())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

// POST /api/profile/passkey/finish (attestation response in body) → 204.
func (s *Server) handlePasskeyFinish(w http.ResponseWriter, r *http.Request) {
	pub := s.engine.IdentityPub()
	if pub == "" {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("not in a camp"))
		return
	}
	if err := s.oidc.RegisterFinish(r, pub, s.profileDisplayName()); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/profile/device {name} → renames THIS device (config.Identity.Name),
// pushing the new name into the running engine + camp announce so peers see it
// without a restart. Must be non-empty and differ from the user's nickname.
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
	// Keep the device name distinct from the user's nickname (the mirror of the
	// rule enforced on profile save).
	if uid := s.currentProfileID(); uid != "" {
		if b := s.blocks.Block(profileScope(uid), uid); b != nil && len(b.Heads) > 0 {
			var c profileContent
			_ = json.Unmarshal(b.Heads[len(b.Heads)-1].Content, &c)
			if c.Nick != "" && strings.EqualFold(c.Nick, req.Name) {
				writeError(w, http.StatusBadRequest, fmt.Errorf("device name must differ from your nickname %q", c.Nick))
				return
			}
		}
	}
	if err := s.store.UpdateCamp(campID, func(c *config.Camp) { c.Identity.Name = req.Name }); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.engine.SetName(req.Name) // running engine: self-filter + pair_req/res
	s.camp.SetName(req.Name)   // camp announce: peers see it next tick
	writeJSON(w, http.StatusOK, map[string]any{"name": req.Name})
}

// GET /api/profile → {exists, user_id, first, last}. exists=false forces the
// UI to show the mandatory create-profile modal.
func (s *Server) handleProfileGet(w http.ResponseWriter, r *http.Request) {
	uid := s.currentProfileID()
	if uid == "" {
		writeJSON(w, http.StatusOK, map[string]any{"exists": false})
		return
	}
	b := s.blocks.Block(profileScope(uid), uid)
	if b == nil || b.Deleted {
		writeJSON(w, http.StatusOK, map[string]any{"exists": false})
		return
	}
	var c profileContent
	if n := len(b.Heads); n > 0 {
		_ = json.Unmarshal(b.Heads[n-1].Content, &c)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exists": true, "user_id": uid, "first": c.First, "last": c.Last, "nick": c.Nick,
		"has_passkey": s.engine.IdentityPub() != "" && s.oidc.HasCred(s.engine.IdentityPub()),
	})
}

// POST /api/profile {first,last} → creates (or updates) the profile block and
// records its user_id in the camp config. 200 with the saved profile.
func (s *Server) handleProfileSave(w http.ResponseWriter, r *http.Request) {
	var req profileContent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.First = strings.TrimSpace(req.First)
	req.Last = strings.TrimSpace(req.Last)
	req.Nick = strings.TrimSpace(req.Nick)
	if req.First == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("first name required"))
		return
	}
	if req.Nick == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("nickname required"))
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
	c, _ := s.store.LoadCamp(campID)
	// Nickname must differ from this device's peer name — they identify a person
	// vs a machine; allowing them equal invites confusion in the users view.
	if c != nil && c.Identity.Name != "" && strings.EqualFold(req.Nick, c.Identity.Name) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("nickname must differ from the device name %q", c.Identity.Name))
		return
	}
	uid := ""
	if c != nil {
		uid = c.Profile
	}
	if uid == "" {
		var err error
		if uid, err = newUserID(); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	content, err := json.Marshal(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.blocks.Upsert(id, profileScope(uid), uid, "user", content); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.UpdateCamp(campID, func(c *config.Camp) { c.Profile = uid }); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exists": true, "user_id": uid, "first": req.First, "last": req.Last, "nick": req.Nick,
	})
}
