// OIDC client (relying-party) registry, persisted per camp in
// clients.json (next to passkeys.json). Survives helper restarts — unlike
// the old in-memory map, which lost every registration on restart.
//
// Public clients (native/SPA, PKCE, no secret) and confidential clients
// (server apps like Gitea/Affine, with a client_secret) both live here.
// Only the secret's SHA-256 is stored; the plaintext is shown once at
// creation and never again.
package oidc

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type client struct {
	ID           string   `json:"client_id"`
	Name         string   `json:"client_name,omitempty"`
	RedirectURIs []string `json:"redirect_uris"`
	LogoutURIs   []string `json:"logout_uris,omitempty"` // post-logout redirect URIs
	// Confidential clients authenticate to /token with a secret; public
	// clients rely on PKCE alone (token_endpoint_auth_method=none). The
	// secret is stored in plaintext (so the admin UI can show it next to
	// the client_id) — acceptable here because the registry lives only on
	// the peer's own disk, behind the overlay.
	Confidential bool   `json:"confidential,omitempty"`
	Secret       string `json:"secret,omitempty"` // empty for public clients
	// ForcePKCE requires PKCE even for a confidential client. Public
	// clients always require PKCE regardless.
	ForcePKCE bool `json:"pkce,omitempty"`
}

// needsPKCE reports whether /authorize must demand a code_challenge for
// this client: always for public clients, opt-in for confidential.
func (c *client) needsPKCE() bool { return !c.Confidential || c.ForcePKCE }

// checkSecret reports whether plain matches the client's secret, in
// constant time. Always false for public clients.
func (c *client) checkSecret(plain string) bool {
	if c.Secret == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(plain), []byte(c.Secret)) == 1
}

// ClientStore persists the client registry under the current camp dir.
type ClientStore struct {
	dirFn func() string
	mu    sync.Mutex
}

func NewClientStore(dirFn func() string) *ClientStore {
	return &ClientStore{dirFn: dirFn}
}

func (s *ClientStore) path() (string, bool) {
	dir := s.dirFn()
	if dir == "" {
		return "", false
	}
	return filepath.Join(dir, "clients.json"), true
}

func (s *ClientStore) load() (map[string]*client, error) {
	out := map[string]*client{}
	path, ok := s.path()
	if !ok {
		return out, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("oidc: clients.json: %w", err)
	}
	return out, nil
}

func (s *ClientStore) save(m map[string]*client) error {
	path, ok := s.path()
	if !ok {
		return fmt.Errorf("oidc: no camp dir for client store")
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// Get returns the client by id (nil if unknown).
func (s *ClientStore) Get(id string) *client {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return nil
	}
	return m[id]
}

// List returns all registered clients (secrets never included — hashes
// only). Order is unspecified.
func (s *ClientStore) List() []*client {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return nil
	}
	out := make([]*client, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	return out
}

// ClientSpec is the input to register a client.
type ClientSpec struct {
	Name         string
	RedirectURIs []string
	LogoutURIs   []string
	Confidential bool
	ForcePKCE    bool
}

// Create registers a new client from spec. Confidential clients get a
// generated secret, returned in plaintext (empty for public clients).
func (s *ClientStore) Create(spec ClientSpec) (*client, string, error) {
	c := &client{
		ID: randToken(16), Name: spec.Name,
		RedirectURIs: spec.RedirectURIs, LogoutURIs: spec.LogoutURIs,
		Confidential: spec.Confidential, ForcePKCE: spec.ForcePKCE,
	}
	var secret string
	if spec.Confidential {
		secret = randToken(24)
		c.Secret = secret
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return nil, "", err
	}
	m[c.ID] = c
	if err := s.save(m); err != nil {
		return nil, "", err
	}
	return c, secret, nil
}

// ClientInfo is the public (secret-free) view of a registered client,
// returned to the admin UI.
type ClientInfo struct {
	ID           string   `json:"client_id"`
	Name         string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
	LogoutURIs   []string `json:"logout_uris,omitempty"`
	Confidential bool     `json:"confidential"`
	PKCE         bool     `json:"pkce"`
	Secret       string   `json:"client_secret,omitempty"` // shown in the admin UI by request
}

func (c *client) info() ClientInfo {
	return ClientInfo{
		ID: c.ID, Name: c.Name, RedirectURIs: c.RedirectURIs, LogoutURIs: c.LogoutURIs,
		Confidential: c.Confidential, PKCE: c.ForcePKCE, Secret: c.Secret,
	}
}

// ListClients returns the registry for the admin UI (no secrets).
func (s *Service) ListClients() []ClientInfo {
	cs := s.clients.List()
	out := make([]ClientInfo, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.info())
	}
	return out
}

// CreateClient registers a client from the admin UI, validating the
// redirect URIs. Returns the plaintext secret once (empty for public).
func (s *Service) CreateClient(spec ClientSpec) (ClientInfo, string, error) {
	if len(spec.RedirectURIs) == 0 {
		return ClientInfo{}, "", fmt.Errorf("at least one redirect_uri required")
	}
	for _, u := range append(append([]string{}, spec.RedirectURIs...), spec.LogoutURIs...) {
		if !validRedirect(u) {
			return ClientInfo{}, "", fmt.Errorf("redirect_uri must be https within .f2f: %s", u)
		}
	}
	c, secret, err := s.clients.Create(spec)
	if err != nil {
		return ClientInfo{}, "", err
	}
	return c.info(), secret, nil
}

// DeleteClient removes a client by id.
func (s *Service) DeleteClient(id string) error { return s.clients.Delete(id) }

// PasskeyUser is a peer that has enrolled at least one passkey — the
// closest thing to a "registered user" in this IdP (there is no user
// table; identity is the camp pub).
type PasskeyUser struct {
	Pub         string `json:"pub"`
	Name        string `json:"name"`
	Credentials int    `json:"credentials"`
}

// PasskeyUsers lists the peers with enrolled passkeys, named via the
// backend roster.
func (s *Service) PasskeyUsers() []PasskeyUser {
	if s.profile == nil {
		return nil
	}
	counts := s.profile.WithCreds()
	out := make([]PasskeyUser, 0, len(counts))
	for pub, n := range counts {
		out = append(out, PasskeyUser{Pub: pub, Name: s.be.PeerName(pub), Credentials: n})
	}
	return out
}

// Delete removes a client by id (no-op if absent).
func (s *ClientStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	delete(m, id)
	return s.save(m)
}
