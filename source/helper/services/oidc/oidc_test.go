package oidc

import (
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vseplet/f2f/source/helper/identity"
)

// fakeBackend stands in for the engine: a fixed signing identity and a
// canned peer-name table. (The issuer is derived per-request, not here.)
type fakeBackend struct {
	id    *identity.Identity
	names map[string]string
}

func (f *fakeBackend) Identity() *identity.Identity { return f.id }
func (f *fakeBackend) PeerName(pub string) string   { return f.names[pub] }

func newTestService(t *testing.T) (*Service, *identity.Identity) {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	be := &fakeBackend{id: id, names: map[string]string{id.PubHex(): "alice"}}
	dir := t.TempDir()
	dirFn := func() string { return dir }
	return New(be, NewClientStore(dirFn), NewSignKeys(dirFn)), id
}

// rsaPub returns the service's RSA public key for verifying minted tokens.
func rsaPub(t *testing.T, svc *Service) *rsa.PublicKey {
	t.Helper()
	key, _, err := svc.keys.get()
	if err != nil {
		t.Fatalf("sign key: %v", err)
	}
	return &key.PublicKey
}

func pkce() (verifier, challenge string) {
	verifier = randToken(32)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// TestRegisterAndMetadata covers dynamic client registration plus the
// discovery and JWKS documents.
func TestRegisterAndMetadata(t *testing.T) {
	svc, id := newTestService(t)
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"client_name":   "gitea",
		"redirect_uris": []string{"https://gitea.bob.f2f/callback"},
	})
	resp, err := http.Post(srv.URL+"/register", "application/json", strings.NewReader(string(body)))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: err=%v status=%v", err, resp.StatusCode)
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()
	if reg.ClientID == "" {
		t.Fatal("no client_id")
	}

	resp, _ = http.Get(srv.URL + "/.well-known/openid-configuration")
	var meta map[string]any
	json.NewDecoder(resp.Body).Decode(&meta)
	resp.Body.Close()
	if meta["issuer"] == nil || meta["authorization_endpoint"] == nil {
		t.Fatalf("discovery incomplete: %v", meta)
	}

	resp, _ = http.Get(srv.URL + "/jwks")
	var set jwks
	json.NewDecoder(resp.Body).Decode(&set)
	resp.Body.Close()
	if len(set.Keys) != 1 || set.Keys[0].Kty != "RSA" || set.Keys[0].Alg != "RS256" || set.Keys[0].N == "" {
		t.Fatalf("jwks wrong: %+v", set)
	}
	_ = id
}

// TestTokenAndUserinfo seeds an authorization code directly (the WebAuthn
// ceremony that normally mints it can't run headless) and exercises the
// token + userinfo endpoints, asserting the EdDSA id_token is valid and
// carries the attested subject.
func TestTokenAndUserinfo(t *testing.T) {
	svc, id := newTestService(t)
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()
	wantIss := srv.URL // httptest is plain http; issuer mirrors the request scheme

	verifier, challenge := pkce()
	c, _, _ := svc.clients.Create(ClientSpec{Name: "gitea", RedirectURIs: []string{"https://gitea.bob.f2f/cb"}})
	svc.mu.Lock()
	svc.codes["thecode"] = &authCode{
		clientID: c.ID, redirectURI: "https://gitea.bob.f2f/cb",
		sub: id.PubHex(), name: "alice", nonce: "n1", scope: "openid profile email",
		challenge: challenge, exp: time.Now().Add(codeTTL),
	}
	svc.mu.Unlock()

	resp, err := http.PostForm(srv.URL+"/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"thecode"},
		"redirect_uri":  {"https://gitea.bob.f2f/cb"},
		"client_id":     {c.ID},
		"code_verifier": {verifier},
	})
	if err != nil || resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("token: err=%v status=%v body=%s", err, resp.StatusCode, b)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		TokenType   string `json:"token_type"`
	}
	json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if tok.TokenType != "Bearer" || tok.IDToken == "" {
		t.Fatalf("bad token resp: %+v", tok)
	}
	claims, err := verifyRS256(tok.IDToken, rsaPub(t, svc))
	if err != nil {
		t.Fatalf("verify id_token: %v", err)
	}
	if claims["sub"] != id.PubHex() || claims["iss"] != wantIss || claims["aud"] != c.ID {
		t.Fatalf("claims wrong: %v (want iss %s)", claims, wantIss)
	}
	if claims["nonce"] != "n1" || claims["name"] != "alice" || claims["preferred_username"] != "alice" {
		t.Fatalf("profile claims wrong: %v", claims)
	}
	if email, _ := claims["email"].(string); !strings.HasSuffix(email, "@"+rpIDForHost(strings.TrimPrefix(srv.URL, "http://"))) {
		t.Fatalf("synthetic email wrong: %v", claims["email"])
	}

	req, _ := http.NewRequest("GET", srv.URL+"/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("userinfo: err=%v status=%v", err, resp.StatusCode)
	}
	var ui map[string]any
	json.NewDecoder(resp.Body).Decode(&ui)
	resp.Body.Close()
	if ui["sub"] != id.PubHex() || ui["name"] != "alice" {
		t.Fatalf("userinfo wrong: %v", ui)
	}
}

// TestAuthorizeRendersPasskeyPage checks the /authorize GET opens a
// ceremony (sets the session cookie) and serves the passkey page rather
// than a password prompt. With no proxy attestation header, the subject
// falls back to the local identity.
func TestAuthorizeRendersPasskeyPage(t *testing.T) {
	svc, _ := newTestService(t)
	c, _, _ := svc.clients.Create(ClientSpec{Name: "myapp", RedirectURIs: []string{"https://app.bob.f2f/cb"}})
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	_, challenge := pkce()
	q := url.Values{
		"client_id": {c.ID}, "redirect_uri": {"https://app.bob.f2f/cb"},
		"response_type": {"code"}, "scope": {"openid"}, "state": {"s"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}
	resp, err := http.Get(srv.URL + "/authorize?" + q.Encode())
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize: err=%v status=%v", err, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "passkey") || !strings.Contains(string(body), "myapp") {
		t.Fatalf("page missing passkey/app: %s", body)
	}
	var hasCookie bool
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie && ck.Value != "" {
			hasCookie = true
		}
	}
	if !hasCookie {
		t.Fatal("no session cookie set")
	}
}

// TestPKCEMismatchRejected ensures a wrong verifier can't redeem a code.
func TestPKCEMismatchRejected(t *testing.T) {
	svc, _ := newTestService(t)
	_, challenge := pkce()
	c, _, _ := svc.clients.Create(ClientSpec{Name: "cid", RedirectURIs: []string{"https://app.bob.f2f/cb"}})
	svc.mu.Lock()
	svc.codes["thecode"] = &authCode{
		clientID: c.ID, redirectURI: "https://app.bob.f2f/cb",
		sub: "deadbeef", challenge: challenge, exp: time.Now().Add(codeTTL),
	}
	svc.mu.Unlock()

	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()
	resp, _ := http.PostForm(srv.URL+"/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"thecode"},
		"redirect_uri":  {"https://app.bob.f2f/cb"},
		"client_id":     {c.ID},
		"code_verifier": {"the-wrong-verifier"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on PKCE mismatch, got %d", resp.StatusCode)
	}
}

// TestConfidentialClientSecret checks a confidential client must present
// its secret at /token (right secret passes, wrong/absent fails).
func TestConfidentialClientSecret(t *testing.T) {
	svc, id := newTestService(t)
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	c, secret, _ := svc.clients.Create(ClientSpec{Name: "affine", RedirectURIs: []string{"https://affine.bob.f2f/cb"}, Confidential: true})
	if secret == "" {
		t.Fatal("confidential client got no secret")
	}
	verifier, challenge := pkce()
	seed := func() {
		svc.mu.Lock()
		svc.codes["cc"] = &authCode{
			clientID: c.ID, redirectURI: "https://affine.bob.f2f/cb",
			sub: id.PubHex(), scope: "openid", challenge: challenge,
			exp: time.Now().Add(codeTTL),
		}
		svc.mu.Unlock()
	}
	form := func(sec string) url.Values {
		return url.Values{
			"grant_type": {"authorization_code"}, "code": {"cc"},
			"redirect_uri": {"https://affine.bob.f2f/cb"}, "client_id": {c.ID},
			"client_secret": {sec}, "code_verifier": {verifier},
		}
	}

	seed()
	resp, _ := http.PostForm(srv.URL+"/token", form("wrong"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong secret: want 401, got %d", resp.StatusCode)
	}
	seed() // code was consumed
	resp, _ = http.PostForm(srv.URL+"/token", form(secret))
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("right secret: want 200, got %d (%s)", resp.StatusCode, b)
	}
}

// TestRegisterRejectsNonF2FRedirect guards the redirect_uri constraint.
func TestRegisterRejectsNonF2FRedirect(t *testing.T) {
	svc, _ := newTestService(t)
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()
	body, _ := json.Marshal(map[string]any{"redirect_uris": []string{"https://evil.example.com/cb"}})
	resp, _ := http.Post(srv.URL+"/register", "application/json", strings.NewReader(string(body)))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for off-overlay redirect, got %d", resp.StatusCode)
	}
}
