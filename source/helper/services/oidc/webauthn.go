// WebAuthn (passkey) glue: the user adapter the library needs, a per-
// request WebAuthn instance (RPID is the camp zone so one passkey works
// at every peer's IdP in the camp), and the in-flight ceremony session.
package oidc

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/vseplet/f2f/source/helper/identity"
)

// waUser adapts a peer (pub + display name + stored credentials) to the
// webauthn.User interface. The user handle is the pub — auth decisions key
// off it, never the name.
type waUser struct {
	pub   string
	name  string
	creds []webauthn.Credential
}

func (u *waUser) WebAuthnID() []byte { return []byte(u.pub) }
func (u *waUser) WebAuthnName() string {
	if u.name != "" {
		return u.name
	}
	return identity.Label("", u.pub)
}
func (u *waUser) WebAuthnDisplayName() string                { return u.WebAuthnName() }
func (u *waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// newWebAuthn builds a WebAuthn instance bound to this request's origin.
// RPID is the camp zone (<zone>.f2f), a registrable parent of every app
// host, so a credential registered once is usable at any peer's IdP.
func newWebAuthn(origin string) (*webauthn.WebAuthn, error) {
	u, err := url.Parse(origin)
	if err != nil {
		return nil, err
	}
	return webauthn.New(&webauthn.Config{
		RPID:          rpIDForHost(u.Hostname()),
		RPDisplayName: "f2f",
		RPOrigins:     []string{origin},
	})
}

// rpIDForHost reduces an app host to the camp zone: the last two labels
// (<zone>.f2f). Browsers treat .f2f as an unknown TLD, so <zone>.f2f is
// the registrable domain and a valid rpId for any <app>.<zone>.f2f origin.
func rpIDForHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	labels := strings.Split(host, ".")
	if len(labels) <= 2 {
		return host
	}
	return strings.Join(labels[len(labels)-2:], ".")
}

// originFor reconstructs the browser-facing origin (scheme://host, no
// path) from the proxy's forwarded headers. WebAuthn needs the bare
// origin, distinct from the issuer (which carries the /oidc prefix).
func originFor(r *http.Request) string {
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

// authSession holds an in-flight /authorize ceremony: the OAuth request
// parameters (resumed when the passkey ceremony finishes) plus the
// WebAuthn challenge/session for the begin→finish round-trip.
type authSession struct {
	clientID    string
	redirectURI string
	state       string
	challenge   string // PKCE code_challenge
	nonce       string
	scope       string
	sub         string // attested peer pub
	name        string
	wa          *webauthn.SessionData
	exp         time.Time
}
