package web

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/vseplet/f2f/source/helper/identity"
)

// Profile passkey — a WebAuthn ceremony owned by the web/profile layer (NOT the
// OIDC credstore). The resulting PUBLIC credential is stored in block.profile
// (scope "profiles", synced to all camp members), so the profile owns its
// passkey instead of it sitting in OIDC's local passkeys.json. RPID is the camp
// zone (<zone>.f2f) so one passkey works at every peer.

// profileWAUser adapts a peer to webauthn.User. The handle is the pub.
type profileWAUser struct {
	pub   string
	name  string
	creds []webauthn.Credential
}

func (u *profileWAUser) WebAuthnID() []byte { return []byte(u.pub) }
func (u *profileWAUser) WebAuthnName() string {
	if u.name != "" {
		return u.name
	}
	return identity.Label("", u.pub)
}
func (u *profileWAUser) WebAuthnDisplayName() string                { return u.WebAuthnName() }
func (u *profileWAUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// profileWebAuthn builds a WebAuthn instance bound to this request's origin,
// with RPID = the camp zone (registrable parent of every app host).
func profileWebAuthn(origin string) (*webauthn.WebAuthn, error) {
	u, err := url.Parse(origin)
	if err != nil {
		return nil, err
	}
	return webauthn.New(&webauthn.Config{
		RPID:          profileRPID(u.Hostname()),
		RPDisplayName: "f2f",
		RPOrigins:     []string{origin},
	})
}

// profileRPID reduces an app host to the camp zone (last two labels,
// <zone>.f2f) — a valid rpId for any <app>.<zone>.f2f origin.
func profileRPID(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	labels := strings.Split(host, ".")
	if len(labels) <= 2 {
		return host
	}
	return strings.Join(labels[len(labels)-2:], ".")
}

// profileOrigin reconstructs the browser-facing origin (scheme://host) from the
// proxy's forwarded headers — what WebAuthn needs.
func profileOrigin(r *http.Request) string {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return proto + "://" + host
}

// loadProfileContent returns the current block.profile content for pub (zero
// value if there's no block yet).
func (s *Server) loadProfileContent(pub string) profileContent {
	var c profileContent
	if b := s.blocks.Block(profileScope, pub); b != nil && len(b.Heads) > 0 {
		_ = json.Unmarshal(b.Heads[len(b.Heads)-1].Content, &c)
	}
	return c
}

// POST /api/profile/passkey/begin → CredentialCreationOptions for the browser.
func (s *Server) handlePasskeyBegin(w http.ResponseWriter, r *http.Request) {
	pub := s.profilePub()
	if pub == "" {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("not in a camp"))
		return
	}
	wa, err := profileWebAuthn(profileOrigin(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	c := s.loadProfileContent(pub)
	user := &profileWAUser{pub: pub, name: s.profileDisplayName(), creds: c.Passkeys}
	opts, sd, err := wa.BeginRegistration(user)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.profRegMu.Lock()
	s.profRegSess[pub] = sd
	s.profRegMu.Unlock()
	writeJSON(w, http.StatusOK, opts)
}

// POST /api/profile/passkey/finish (attestation in body) → stores the new
// public credential in block.profile and returns 204.
func (s *Server) handlePasskeyFinish(w http.ResponseWriter, r *http.Request) {
	pub := s.profilePub()
	if pub == "" {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("not in a camp"))
		return
	}
	s.profRegMu.Lock()
	sd := s.profRegSess[pub]
	delete(s.profRegSess, pub)
	s.profRegMu.Unlock()
	if sd == nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("no pending passkey registration"))
		return
	}
	wa, err := profileWebAuthn(profileOrigin(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	c := s.loadProfileContent(pub)
	user := &profileWAUser{pub: pub, name: s.profileDisplayName(), creds: c.Passkeys}
	cred, err := wa.FinishRegistration(user, *sd, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id, ok := s.blockSigner(w)
	if !ok {
		return
	}
	// Store only the PUBLIC credential — it's synced to every peer in the
	// profile block; the private key never leaves the authenticator.
	c.Passkeys = append(c.Passkeys, *cred)
	content, err := json.Marshal(c)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.blocks.Upsert(id, profileScope, pub, "profile", content); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
