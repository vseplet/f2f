// Command oidc-demo is a toy relying-party (RP) app — just a web app that
// logs you in with f2f. It has NO identity provider of its own: the IdP
// lives on the SAME origin under /oidc, served by the f2f helper's proxy
// (which routes /oidc/* to the built-in OIDC service). So everything
// stays on the app's own domain — no loopback, no cross-origin bounce.
//
// Run it as a normal local app, then publish a domain to it in the portal
// (e.g. demo.xyz.f2f → 127.0.0.1:7778) and open https://demo.xyz.f2f:
//
//	go run ./services/oidc/demo
//
// Requires a helper build that serves /oidc (proxy route + OIDC service).
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const addr = "127.0.0.1:7778"

func main() {
	app := &rp{
		http:      &http.Client{Timeout: 10 * time.Second},
		clientID:  map[string]string{},
		verifiers: map[string]session{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", app.start)
	mux.HandleFunc("/cb", app.callback)
	fmt.Printf("RP listening on %s — publish a domain to it and open it over https\n", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

type session struct {
	verifier string
	idp      string // issuer this login was started against
	redirect string
}

type rp struct {
	http      *http.Client
	mu        sync.Mutex
	clientID  map[string]string  // issuer → registered client_id
	verifiers map[string]session // state → PKCE/session
}

// origin reconstructs the public base URL the browser used, from the
// proxy's X-Forwarded-* headers (falling back to the raw request for
// direct access). The IdP is this same origin + /oidc.
func origin(r *http.Request) string {
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

// start begins the auth-code+PKCE flow: ensure a client is registered
// with this origin's IdP, then redirect the browser to /oidc/authorize on
// the same origin.
func (a *rp) start(w http.ResponseWriter, r *http.Request) {
	base := origin(r)
	idp := base + "/oidc"
	redirect := base + "/cb"

	clientID, err := a.ensureClient(idp, redirect)
	if err != nil {
		http.Error(w, "register: "+err.Error(), http.StatusBadGateway)
		return
	}

	verifier, challenge := pkce()
	state := token()
	a.mu.Lock()
	a.verifiers[state] = session{verifier: verifier, idp: idp, redirect: redirect}
	a.mu.Unlock()

	q := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"response_type":         {"code"},
		"scope":                 {"openid profile"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"nonce":                 {token()},
	}
	http.Redirect(w, r, idp+"/authorize?"+q.Encode(), http.StatusFound)
}

// callback exchanges the code for tokens (back-channel, same origin) and
// shows the decoded result.
func (a *rp) callback(w http.ResponseWriter, r *http.Request) {
	if e := r.URL.Query().Get("error"); e != "" {
		fmt.Fprintf(w, "<pre>login denied: %s</pre>", e)
		return
	}
	state := r.URL.Query().Get("state")
	a.mu.Lock()
	s, ok := a.verifiers[state]
	delete(a.verifiers, state)
	a.mu.Unlock()
	if !ok {
		http.Error(w, "unknown state", http.StatusBadRequest)
		return
	}
	clientID := a.client(s.idp)

	tok, err := a.exchange(s, clientID, r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	idClaims := decodeJWT(tok.IDToken)
	userinfo := a.userinfo(s.idp, tok.AccessToken)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset=utf-8>
<style>body{font:14px ui-monospace,monospace;background:#15161a;color:#d8dee9;padding:30px;line-height:1.5}
h2{color:#4ec5d6}pre{background:#1d1f25;padding:14px;border-radius:8px;overflow:auto}small{color:#8a90a0}</style>
<h2>logged in ✔</h2>
<small>issuer: %s</small>
<h3>id_token claims</h3><pre>%s</pre>
<h3>/userinfo</h3><pre>%s</pre>
<p><a href="/" style="color:#4ec5d6">↻ log in again</a></p>`,
		s.idp, pretty(idClaims), pretty(userinfo))
}

// ensureClient registers a client with idp once per issuer (DCR), caching
// the client_id. redirect is the callback to register.
func (a *rp) ensureClient(idp, redirect string) (string, error) {
	a.mu.Lock()
	if id := a.clientID[idp]; id != "" {
		a.mu.Unlock()
		return id, nil
	}
	a.mu.Unlock()

	body, _ := json.Marshal(map[string]any{
		"client_name":   "oidc-demo",
		"redirect_uris": []string{redirect},
	})
	resp, err := a.http.Post(idp+"/register", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("register %d: %s", resp.StatusCode, b)
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.ClientID == "" {
		return "", fmt.Errorf("no client_id")
	}
	a.mu.Lock()
	a.clientID[idp] = out.ClientID
	a.mu.Unlock()
	return out.ClientID, nil
}

func (a *rp) client(idp string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.clientID[idp]
}

type tokenResp struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
}

func (a *rp) exchange(s session, clientID, code string) (*tokenResp, error) {
	resp, err := a.http.PostForm(s.idp+"/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {s.redirect},
		"client_id":     {clientID},
		"code_verifier": {s.verifier},
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token %d: %s", resp.StatusCode, b)
	}
	var t tokenResp
	json.NewDecoder(resp.Body).Decode(&t)
	return &t, nil
}

func (a *rp) userinfo(idp, accessToken string) map[string]any {
	req, _ := http.NewRequest("GET", idp+"/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := a.http.Do(req)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return m
}

func pkce() (verifier, challenge string) {
	verifier = token()
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func decodeJWT(t string) map[string]any {
	parts := strings.Split(t, ".")
	if len(parts) != 3 {
		return map[string]any{"error": "malformed jwt"}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var m map[string]any
	json.Unmarshal(payload, &m)
	return m
}

func pretty(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

var ctr int

func token() string {
	ctr++
	return fmt.Sprintf("%x-%d", time.Now().UnixNano(), ctr)
}
