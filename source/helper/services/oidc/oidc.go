// Package oidc is a minimal OpenID Connect provider that turns f2f's
// overlay identity into standard OIDC tokens, co-located with the apps it
// fronts. The novelty isn't the protocol — it's how the user is
// authenticated: the IdP runs on the peer that hosts the relying app, and
// the visiting peer is identified by the AmneziaWG-attested overlay
// connection (surfaced as the X-F2F-Peer header the proxy injects), not a
// password. The token's subject is the visitor's Ed25519 pub; the token
// is signed (EdDSA) with the host peer's identity, so any relying app
// validates it against this node's one-key JWKS.
//
// This is a prototype: it implements discovery, JWKS, dynamic client
// registration (RFC 7591), the authorization-code flow with PKCE
// (RFC 7636) and userinfo. The /authorize consent screen is the seam
// where the passkey (WebAuthn) ceremony will live — today it's a plain
// approve button. Client and code stores are in-memory.
package oidc

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/vseplet/f2f/source/helper/clog"
	"github.com/vseplet/f2f/source/helper/identity"
)

// attestHeader is the header the reverse proxy sets to the
// AmneziaWG-attested pub of the calling peer. The proxy MUST strip any
// inbound copy before setting it — it's the whole basis of auth, so a
// spoofed value would be an identity-forgery hole.
const attestHeader = "X-F2F-Peer"

const (
	codeTTL  = 60 * time.Second
	tokenTTL = 60 * time.Minute
)

// Backend is the node-state the provider reads: the current camp's
// signing identity and a pub→display-name lookup for the visiting peer.
// The issuer is NOT here — it's derived per-request from the host the app
// was reached on (see issuerFor), because the IdP is co-located with the
// app on this peer and addressed via the app's own routable domain.
// Implemented over the engine in main; faked in tests.
type Backend interface {
	// Identity is the running camp's Ed25519 key, or nil outside a camp.
	Identity() *identity.Identity
	// PeerName resolves an attested pub to a display name ("" if unknown).
	PeerName(pubHex string) string
}

type authCode struct {
	clientID    string
	redirectURI string
	sub         string // attested pub of the authenticated peer
	name        string
	nonce       string
	challenge   string // PKCE S256 code_challenge
	scope       string
	exp         time.Time
}

// Service is the provider. Construct with New, mount with Handler.
type Service struct {
	be      Backend
	creds   *CredStore
	clients *ClientStore
	keys    *SignKeys

	mu       sync.Mutex
	codes    map[string]*authCode
	sessions map[string]*authSession // ceremony id (cookie) → in-flight /authorize
}

// New builds the provider over backend be, persisting passkeys via creds,
// the client registry via clients, and RS256 signing via keys.
func New(be Backend, creds *CredStore, clients *ClientStore, keys *SignKeys) *Service {
	return &Service{
		be:       be,
		creds:    creds,
		clients:  clients,
		keys:     keys,
		codes:    map[string]*authCode{},
		sessions: map[string]*authSession{},
	}
}

// Handler returns the mux serving the provider's endpoints.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", s.handleDiscovery)
	mux.HandleFunc("/jwks", s.handleJWKS)
	mux.HandleFunc("/register", s.handleRegister)
	mux.HandleFunc("/authorize", s.handleAuthorize)
	mux.HandleFunc("/authorize/passkey/begin", s.handlePasskeyBegin)
	mux.HandleFunc("/authorize/passkey/finish", s.handlePasskeyFinish)
	mux.HandleFunc("/authorize/cancel", s.handleCancel)
	mux.HandleFunc("/token", s.handleToken)
	mux.HandleFunc("/userinfo", s.handleUserinfo)
	mux.HandleFunc("/end_session", s.handleEndSession)
	return mux
}

const sessionCookie = "f2f_oidc_sess"

// active returns the current camp identity, or false if the node isn't in
// a camp (no key to sign with).
func (s *Service) active() (*identity.Identity, bool) {
	id := s.be.Identity()
	if id == nil {
		return nil, false
	}
	return id, true
}

// issuerFor reconstructs this provider's issuer from the request as the
// caller's browser sees it: the proxy terminates TLS and forwards the
// original scheme/host/path-prefix in X-Forwarded-* headers. The IdP
// lives under a /oidc prefix on the app's host, so the issuer is e.g.
// https://grafana.<zone>.f2f/oidc. Falls back to the raw request for
// direct (test) access.
func issuerFor(r *http.Request) string {
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
	return proto + "://" + host + r.Header.Get("X-Forwarded-Prefix")
}

func (s *Service) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.active(); !ok {
		http.Error(w, "not in a camp", http.StatusServiceUnavailable)
		return
	}
	iss := issuerFor(r)
	writeJSON(w, map[string]any{
		"issuer":                                iss,
		"authorization_endpoint":                iss + "/authorize",
		"token_endpoint":                        iss + "/token",
		"userinfo_endpoint":                     iss + "/userinfo",
		"jwks_uri":                              iss + "/jwks",
		"registration_endpoint":                 iss + "/register",
		"end_session_endpoint":                  iss + "/end_session",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
		"claims_supported":                      []string{"sub", "name", "preferred_username", "email", "email_verified"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_basic", "client_secret_post"},
	})
}

func (s *Service) handleJWKS(w http.ResponseWriter, r *http.Request) {
	key, kid, err := s.keys.get()
	if err != nil {
		http.Error(w, "not in a camp", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, jwks{Keys: []jwk{rsaJWK(&key.PublicKey, kid)}})
}

// handleRegister is dynamic client registration (RFC 7591). The request
// itself arrives over the overlay, so it's already attested as a camp
// member — that's the gate. We constrain redirect_uris to https within
// the .f2f zone so a registration can never aim tokens at an off-overlay
// endpoint.
func (s *Service) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if len(req.RedirectURIs) == 0 {
		oauthErr(w, http.StatusBadRequest, "invalid_redirect_uri", "at least one redirect_uri required")
		return
	}
	for _, u := range req.RedirectURIs {
		if !validRedirect(u) {
			oauthErr(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri must be https within .f2f: "+u)
			return
		}
	}
	// Dynamically-registered clients are public (PKCE only). Confidential
	// clients with a secret are created through the admin API/UI.
	c, _, err := s.clients.Create(ClientSpec{Name: req.ClientName, RedirectURIs: req.RedirectURIs})
	if err != nil {
		http.Error(w, "register: "+err.Error(), http.StatusInternalServerError)
		return
	}
	clog.Info("oidc", "registered client %s (%s)", c.ID, c.Name)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{
		"client_id":                  c.ID,
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"token_endpoint_auth_method": "none",
	})
}

// handleAuthorize starts the authorization-code flow. It validates the
// request, pins the attested subject, opens a ceremony session, and
// renders the passkey page. The actual approval happens via the WebAuthn
// begin/finish round-trip below — there is no password and no plain
// "approve" button. The subject is the attested peer, never a form field.
func (s *Service) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.active(); !ok {
		http.Error(w, "not in a camp", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")
	state := q.Get("state")

	c := s.clients.Get(clientID)
	if c == nil {
		oauthErr(w, http.StatusBadRequest, "unauthorized_client", "unknown client_id")
		return
	}
	if !redirectMatches(c.RedirectURIs, redirectURI) {
		// Per spec: a bad redirect_uri must NOT redirect — show an error.
		oauthErr(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri not registered")
		return
	}
	// From here errors go back to the client via the redirect_uri.
	if q.Get("response_type") != "code" {
		redirectErr(w, r, redirectURI, state, "unsupported_response_type", "only code is supported")
		return
	}
	// PKCE is mandatory for public clients and opt-in for confidential
	// ones (which authenticate with a secret instead) — many server apps
	// like Gitea don't send PKCE. When a challenge is supplied it must be
	// S256; the value is carried through and verified at /token.
	challenge := q.Get("code_challenge")
	if c.needsPKCE() {
		if challenge == "" || q.Get("code_challenge_method") != "S256" {
			redirectErr(w, r, redirectURI, state, "invalid_request", "PKCE S256 required")
			return
		}
	} else if challenge != "" && q.Get("code_challenge_method") != "S256" {
		redirectErr(w, r, redirectURI, state, "invalid_request", "code_challenge_method must be S256")
		return
	}

	sub := s.attestedPeer(r)
	if sub == "" {
		http.Error(w, "no attested overlay identity", http.StatusUnauthorized)
		return
	}

	sid := randToken(18)
	s.mu.Lock()
	s.sessions[sid] = &authSession{
		clientID:    clientID,
		redirectURI: redirectURI,
		state:       state,
		challenge:   challenge,
		nonce:       q.Get("nonce"),
		scope:       q.Get("scope"),
		sub:         sub,
		name:        s.be.PeerName(sub),
		register:    len(s.creds.Get(sub)) == 0,
		exp:         time.Now().Add(5 * time.Minute),
	}
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(originFor(r), "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   300,
	})
	s.renderPasskeyPage(w, c, s.sessions[sid])
}

// session resolves the ceremony session from the request cookie (nil if
// missing/expired).
func (s *Service) session(r *http.Request) (string, *authSession) {
	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[ck.Value]
	if sess == nil || time.Now().After(sess.exp) {
		delete(s.sessions, ck.Value)
		return "", nil
	}
	return ck.Value, sess
}

// handlePasskeyBegin returns WebAuthn options: a registration challenge on
// first use (no credential yet), otherwise a login assertion challenge.
func (s *Service) handlePasskeyBegin(w http.ResponseWriter, r *http.Request) {
	_, sess := s.session(r)
	if sess == nil {
		http.Error(w, "no session", http.StatusBadRequest)
		return
	}
	wa, err := newWebAuthn(originFor(r))
	if err != nil {
		http.Error(w, "webauthn init: "+err.Error(), http.StatusInternalServerError)
		return
	}
	user := &waUser{pub: sess.sub, name: sess.name, creds: s.creds.Get(sess.sub)}

	if sess.register {
		opts, sd, err := wa.BeginRegistration(user)
		if err != nil {
			http.Error(w, "begin register: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.mu.Lock()
		sess.wa = sd
		s.mu.Unlock()
		writeJSON(w, map[string]any{"mode": "register", "options": opts})
		return
	}
	opts, sd, err := wa.BeginLogin(user)
	if err != nil {
		http.Error(w, "begin login: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	sess.wa = sd
	s.mu.Unlock()
	writeJSON(w, map[string]any{"mode": "login", "options": opts})
}

// handlePasskeyFinish verifies the WebAuthn response (storing the new
// credential on registration, bumping the sign counter on login), then
// mints the authorization code and returns the redirect URL.
func (s *Service) handlePasskeyFinish(w http.ResponseWriter, r *http.Request) {
	sid, sess := s.session(r)
	if sess == nil || sess.wa == nil {
		http.Error(w, "no session", http.StatusBadRequest)
		return
	}
	wa, err := newWebAuthn(originFor(r))
	if err != nil {
		http.Error(w, "webauthn init: "+err.Error(), http.StatusInternalServerError)
		return
	}
	user := &waUser{pub: sess.sub, name: sess.name, creds: s.creds.Get(sess.sub)}

	if sess.register {
		cred, err := wa.FinishRegistration(user, *sess.wa, r)
		if err != nil {
			http.Error(w, "finish register: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.creds.Add(sess.sub, *cred); err != nil {
			http.Error(w, "store credential: "+err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		cred, err := wa.FinishLogin(user, *sess.wa, r)
		if err != nil {
			http.Error(w, "finish login: "+err.Error(), http.StatusBadRequest)
			return
		}
		s.creds.Update(sess.sub, cred)
	}

	code := s.issueCode(sess)
	s.mu.Lock()
	delete(s.sessions, sid)
	s.mu.Unlock()
	writeJSON(w, map[string]string{"redirect": buildRedirect(sess.redirectURI, code, sess.state)})
}

// handleCancel aborts the ceremony and bounces back to the client with an
// access_denied error.
func (s *Service) handleCancel(w http.ResponseWriter, r *http.Request) {
	sid, sess := s.session(r)
	if sess == nil {
		http.Error(w, "no session", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	delete(s.sessions, sid)
	s.mu.Unlock()
	redirectErr(w, r, sess.redirectURI, sess.state, "access_denied", "user cancelled")
}

// issueCode mints a single-use authorization code bound to the session.
func (s *Service) issueCode(sess *authSession) string {
	code := randToken(24)
	s.mu.Lock()
	s.codes[code] = &authCode{
		clientID:    sess.clientID,
		redirectURI: sess.redirectURI,
		sub:         sess.sub,
		name:        sess.name,
		nonce:       sess.nonce,
		challenge:   sess.challenge,
		scope:       sess.scope,
		exp:         time.Now().Add(codeTTL),
	}
	s.mu.Unlock()
	return code
}

func buildRedirect(redirectURI, code, state string) string {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return redirectURI
	}
	q := u.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Service) handleToken(w http.ResponseWriter, r *http.Request) {
	key, kid, err := s.keys.get()
	if err != nil {
		http.Error(w, "not in a camp", http.StatusServiceUnavailable)
		return
	}
	iss := issuerFor(r)
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthErr(w, http.StatusBadRequest, "invalid_request", "bad form")
		return
	}
	if r.Form.Get("grant_type") != "authorization_code" {
		oauthErr(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code")
		return
	}
	code := r.Form.Get("code")
	s.mu.Lock()
	ac := s.codes[code]
	delete(s.codes, code) // single use, even on failure
	s.mu.Unlock()
	if ac == nil || time.Now().After(ac.exp) {
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "code unknown or expired")
		return
	}
	// client_id can arrive in the form or via HTTP Basic (confidential).
	clientID, clientSecret := clientAuth(r)
	if clientID == "" {
		clientID = ac.clientID
	}
	if clientID != ac.clientID || r.Form.Get("redirect_uri") != ac.redirectURI {
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "client/redirect mismatch")
		return
	}
	c := s.clients.Get(ac.clientID)
	if c == nil {
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "client gone")
		return
	}
	// Confidential clients must present their secret; public clients lean
	// on PKCE.
	if c.Confidential && !c.checkSecret(clientSecret) {
		w.Header().Set("WWW-Authenticate", "Basic")
		oauthErr(w, http.StatusUnauthorized, "invalid_client", "bad client_secret")
		return
	}
	// PKCE is verified only when a challenge was issued (always for public
	// clients; optional for confidential ones).
	if ac.challenge != "" && !verifyPKCE(r.Form.Get("code_verifier"), ac.challenge) {
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}

	now := time.Now()
	idClaims := map[string]any{
		"iss": iss,
		"sub": ac.sub,
		"aud": ac.clientID,
		"iat": now.Unix(),
		"exp": now.Add(tokenTTL).Unix(),
	}
	if ac.nonce != "" {
		idClaims["nonce"] = ac.nonce
	}
	for k, v := range identityClaims(ac.sub, ac.name, ac.scope, r) {
		idClaims[k] = v
	}
	idToken, err := mintRS256(key, kid, idClaims)
	if err != nil {
		http.Error(w, "sign id_token", http.StatusInternalServerError)
		return
	}
	// Access token is also an RS256 JWT so /userinfo can validate it
	// statelessly against the same JWKS.
	accessToken, err := mintRS256(key, kid, map[string]any{
		"iss":   iss,
		"sub":   ac.sub,
		"aud":   iss + "/userinfo",
		"iat":   now.Unix(),
		"exp":   now.Add(tokenTTL).Unix(),
		"scope": ac.scope,
		"name":  ac.name,
	})
	if err != nil {
		http.Error(w, "sign access_token", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{
		"access_token": accessToken,
		"id_token":     idToken,
		"token_type":   "Bearer",
		"expires_in":   int(tokenTTL.Seconds()),
		"scope":        ac.scope,
	})
}

func (s *Service) handleUserinfo(w http.ResponseWriter, r *http.Request) {
	key, _, err := s.keys.get()
	if err != nil {
		http.Error(w, "not in a camp", http.StatusServiceUnavailable)
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	claims, err := verifyRS256(strings.TrimPrefix(auth, "Bearer "), &key.PublicKey)
	if err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	sub, _ := claims["sub"].(string)
	name, _ := claims["name"].(string)
	scope, _ := claims["scope"].(string)
	out := map[string]any{"sub": sub}
	for k, v := range identityClaims(sub, name, scope, r) {
		out[k] = v
	}
	writeJSON(w, out)
}

// handleEndSession is RP-initiated logout (OIDC RP-Initiated Logout). We
// keep no persistent IdP login session (the passkey ceremony runs every
// time), so there's nothing to clear — we just validate the
// post_logout_redirect_uri against the client's registered logout URIs
// and bounce the browser back.
func (s *Service) handleEndSession(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	post := q.Get("post_logout_redirect_uri")
	if post == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><meta charset=utf-8><p>Signed out."))
		return
	}
	if !s.logoutAllowed(post) {
		http.Error(w, "post_logout_redirect_uri not registered", http.StatusBadRequest)
		return
	}
	if st := q.Get("state"); st != "" {
		if u, err := url.Parse(post); err == nil {
			rq := u.Query()
			rq.Set("state", st)
			u.RawQuery = rq.Encode()
			post = u.String()
		}
	}
	http.Redirect(w, r, post, http.StatusFound)
}

// logoutAllowed reports whether post is a safe post-logout redirect: it
// matches a client's explicitly-registered logout URI, OR shares an
// origin with one of a client's registered redirect URIs (so logging back
// "to the app" works without separately registering a logout URI — apps
// like Gitea just send their own base URL).
func (s *Service) logoutAllowed(post string) bool {
	postOrigin := originOf(post)
	for _, c := range s.clients.List() {
		if redirectMatches(c.LogoutURIs, post) {
			return true
		}
		if postOrigin == "" {
			continue
		}
		for _, ru := range c.RedirectURIs {
			if o := originOf(ru); o != "" && o == postOrigin {
				return true
			}
		}
	}
	return false
}

// originOf returns scheme://host for raw, or "" if it can't be parsed.
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// attestedPeer returns the pub of the calling peer. The reverse proxy
// sets X-F2F-Peer from the AmneziaWG-attested overlay address. As a
// single-node fallback (loopback, "log in to my own service") we use the
// local identity — there is no remote peer to attest in that case.
func (s *Service) attestedPeer(r *http.Request) string {
	if p := strings.TrimSpace(r.Header.Get(attestHeader)); p != "" {
		return p
	}
	if id := s.be.Identity(); id != nil {
		return id.PubHex()
	}
	return ""
}

// renderPasskeyPage shows the sign-in screen and drives the WebAuthn
// ceremony in the browser. On a button press it asks /authorize/passkey/
// begin for options, runs navigator.credentials.create (first use) or .get
// (returning user), POSTs the result to .../finish, and follows the
// returned redirect back to the app. The base path (/oidc) comes from the
// issuer so fetches resolve through the proxy prefix.
func (s *Service) renderPasskeyPage(w http.ResponseWriter, c *client, sess *authSession) {
	who := sess.name
	if who == "" {
		who = identity.Label("", sess.sub)
	}
	verb := "Continue with passkey"
	if sess.register {
		verb = "Set up a passkey"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, passkeyHTML,
		html.EscapeString(clientLabel(c)),
		html.EscapeString(who),
		scopeNote(sess.scope),
		html.EscapeString(verb),
	)
}

// passkeyHTML is the sign-in page template. %s: app name, user, scope
// note, button verb. The JS converts the base64url fields WebAuthn options
// carry to/from ArrayBuffers and talks to the begin/finish endpoints
// relative to the current path (which already includes the /oidc prefix).
const passkeyHTML = `<!doctype html><meta charset="utf-8"><title>f2f sign-in</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>body{font:15px system-ui;background:#15161a;color:#e6e6e6;display:grid;place-items:center;height:100vh;margin:0}
.card{background:#1d1f25;border:1px solid #2a2d36;border-radius:14px;padding:28px 32px;max-width:380px;text-align:center}
h1{font-size:17px;margin:0 0 6px}.muted{color:#8a90a0;font-size:13px}.app{color:#4ec5d6;font-weight:600}
button{margin-top:18px;width:100%%;padding:11px;border:0;border-radius:9px;background:#4ec5d6;color:#06121a;font-weight:600;font-size:15px;cursor:pointer}
.deny{background:transparent;color:#8a90a0;margin-top:8px}.err{color:#e0686e;font-size:13px;margin-top:12px;min-height:1em}</style>
<div class="card">
<h1>Sign in to <span class="app">%s</span></h1>
<div class="muted">as <b>%s</b></div>
<div class="muted" style="margin-top:10px">It will receive your f2f identity%s.</div>
<button id="go">%s</button>
<button id="cancel" class="deny">Cancel</button>
<div class="err" id="err"></div>
</div>
<script>
const b64uToBuf = s => Uint8Array.from(atob(s.replace(/-/g,'+').replace(/_/g,'/').padEnd(Math.ceil(s.length/4)*4,'=')), c=>c.charCodeAt(0)).buffer;
const bufToB64u = b => btoa(String.fromCharCode(...new Uint8Array(b))).replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'');
const err = m => document.getElementById('err').textContent = m;
// begin/finish/cancel live under the same /oidc base as this page.
const base = location.pathname.replace(/\/authorize$/, '');
function decodeCreate(o){o.challenge=b64uToBuf(o.challenge);o.user.id=b64uToBuf(o.user.id);
  (o.excludeCredentials||[]).forEach(c=>c.id=b64uToBuf(c.id));return o;}
function decodeGet(o){o.challenge=b64uToBuf(o.challenge);
  (o.allowCredentials||[]).forEach(c=>c.id=b64uToBuf(c.id));return o;}
function encodeCred(cred){
  const r=cred.response, out={id:cred.id,type:cred.type,rawId:bufToB64u(cred.rawId),
    response:{clientDataJSON:bufToB64u(r.clientDataJSON)}};
  if(r.attestationObject) out.response.attestationObject=bufToB64u(r.attestationObject);
  if(r.authenticatorData){out.response.authenticatorData=bufToB64u(r.authenticatorData);
    out.response.signature=bufToB64u(r.signature);
    out.response.userHandle=r.userHandle?bufToB64u(r.userHandle):null;}
  return out;}
async function run(){
  err('');
  document.getElementById('go').disabled=true;
  try{
    const b=await fetch(base+'/authorize/passkey/begin',{method:'POST'});
    if(!b.ok) throw new Error(await b.text());
    const {mode,options}=await b.json();
    let cred;
    if(mode==='register') cred=await navigator.credentials.create({publicKey:decodeCreate(options.publicKey)});
    else cred=await navigator.credentials.get({publicKey:decodeGet(options.publicKey)});
    const f=await fetch(base+'/authorize/passkey/finish',{method:'POST',
      headers:{'content-type':'application/json'},body:JSON.stringify(encodeCred(cred))});
    if(!f.ok) throw new Error(await f.text());
    location.href=(await f.json()).redirect;
  }catch(e){err(e.message||String(e));document.getElementById('go').disabled=false;}
}
document.getElementById('go').onclick=run;
document.getElementById('cancel').onclick=()=>location.href=base+'/authorize/cancel';
</script>`

// --- helpers ---

func validRedirect(raw string) bool {
	// Wildcard patterns (Pocket-ID style, e.g. https://*.gitea.xyz.f2f/*)
	// don't url.Parse cleanly; validate them by shape: https + .f2f host.
	if strings.Contains(raw, "*") {
		rest, ok := strings.CutPrefix(raw, "https://")
		if !ok {
			return false
		}
		host := rest
		if i := strings.IndexAny(rest, "/?"); i >= 0 {
			host = rest[:i]
		}
		return strings.HasSuffix(host, ".f2f")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	// Loopback redirects are allowed over http for native/local clients
	// (RFC 8252) — used by the local demo RP.
	if host == "127.0.0.1" || host == "::1" || host == "localhost" {
		return u.Scheme == "http" || u.Scheme == "https"
	}
	// Everything else must be https within the overlay's .f2f zone, so a
	// registration can never aim tokens off-overlay.
	return u.Scheme == "https" && strings.HasSuffix(host, ".f2f")
}

// redirectMatches reports whether actual matches one of the registered
// redirect URIs, honouring '*' wildcards (each '*' matches any run of
// characters).
func redirectMatches(registered []string, actual string) bool {
	for _, r := range registered {
		if r == actual {
			return true
		}
		if strings.Contains(r, "*") && wildcardMatch(r, actual) {
			return true
		}
	}
	return false
}

// wildcardMatch matches s against a glob pattern where '*' is any run of
// characters. Anchored at both ends.
func wildcardMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	// Must start with the first literal and end with the last.
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	if !strings.HasSuffix(s, parts[len(parts)-1]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return true
}

// identityClaims builds the profile/email claims apps like Gitea/Affine
// expect. f2f has no real email, so we synthesize a stable one from the
// peer's fingerprint within the camp zone (email_verified=false — it's an
// identifier, not a reachable address). preferred_username is the peer's
// display name. zone is the camp domain (e.g. xyz.f2f) taken from the
// request host. Honours the requested scope.
func identityClaims(sub, name, scope string, r *http.Request) map[string]any {
	out := map[string]any{}
	if strings.Contains(scope, "profile") {
		if name != "" {
			out["name"] = name
			out["preferred_username"] = name
		}
	}
	if strings.Contains(scope, "email") {
		zone := rpIDForHost(hostOf(r)) // <zone>.f2f
		fp := identity.FingerprintHex(sub)
		if fp != "" && zone != "" {
			out["email"] = fp + "@" + zone
			out["email_verified"] = false
		}
	}
	return out
}

// hostOf returns the external host the request was made to (forwarded host
// or raw Host).
func hostOf(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		return h
	}
	return r.Host
}

// clientAuth extracts client credentials from the token request: HTTP
// Basic (client_secret_basic) takes precedence, else the form fields
// (client_secret_post). Returns empty strings when absent.
func clientAuth(r *http.Request) (id, secret string) {
	if u, p, ok := r.BasicAuth(); ok {
		return u, p
	}
	return r.Form.Get("client_id"), r.Form.Get("client_secret")
}

func verifyPKCE(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]) == challenge
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("oidc: rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func clientLabel(c *client) string {
	if c.Name != "" {
		return c.Name
	}
	return c.ID
}

func scopeNote(scope string) string {
	if strings.Contains(scope, "profile") {
		return " (name)"
	}
	return ""
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func oauthErr(w http.ResponseWriter, code int, errCode, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": errCode, "error_description": desc})
}

// redirectErr sends an OAuth error back to the client via redirect_uri,
// per RFC 6749 §4.1.2.1 (used once redirect_uri is validated).
func redirectErr(w http.ResponseWriter, r *http.Request, redirectURI, state, errCode, desc string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		oauthErr(w, http.StatusBadRequest, errCode, desc)
		return
	}
	q := u.Query()
	q.Set("error", errCode)
	q.Set("error_description", desc)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}
