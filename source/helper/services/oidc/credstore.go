// Passkey credential storage. One file per camp (passkeys.json in the
// camp dir) mapping a peer pub → its registered WebAuthn credentials.
// This is the local store; camp-wide replication of the public credential
// material (so any peer's IdP can verify a visitor's assertion) is a later
// step — see project_oidc / the messaging membership log.
package oidc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-webauthn/webauthn/webauthn"
)

// CredStore persists passkey credentials keyed by peer pub. dirFn returns
// the current camp's directory (where passkeys.json lives); it may return
// "" when the node isn't in a camp, in which case the store is read-empty.
type CredStore struct {
	dirFn func() string

	mu sync.Mutex
}

// NewCredStore builds a store reading/writing under the camp dir returned
// by dirFn.
func NewCredStore(dirFn func() string) *CredStore {
	return &CredStore{dirFn: dirFn}
}

func (s *CredStore) path() (string, bool) {
	dir := s.dirFn()
	if dir == "" {
		return "", false
	}
	return filepath.Join(dir, "passkeys.json"), true
}

// load reads the whole pub→creds map (empty on missing file). Caller holds
// the lock.
func (s *CredStore) load() (map[string][]webauthn.Credential, error) {
	out := map[string][]webauthn.Credential{}
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
		return nil, fmt.Errorf("oidc: passkeys.json: %w", err)
	}
	return out, nil
}

func (s *CredStore) save(m map[string][]webauthn.Credential) error {
	path, ok := s.path()
	if !ok {
		return fmt.Errorf("oidc: no camp dir for passkey store")
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// pubsWithCreds returns the pubs that have at least one passkey, with
// their credential counts.
func (s *CredStore) pubsWithCreds() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return nil
	}
	out := map[string]int{}
	for pub, creds := range m {
		if len(creds) > 0 {
			out[pub] = len(creds)
		}
	}
	return out
}

// Get returns the credentials registered for pub (nil if none).
func (s *CredStore) Get(pub string) []webauthn.Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return nil
	}
	return m[pub]
}

// Add appends a freshly-registered credential for pub.
func (s *CredStore) Add(pub string, cred webauthn.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	m[pub] = append(m[pub], cred)
	return s.save(m)
}

// Update replaces the stored copy of a credential (matched by ID) for pub,
// e.g. to persist the post-login sign counter. No-op if not found.
func (s *CredStore) Update(pub string, cred *webauthn.Credential) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return
	}
	creds := m[pub]
	for i := range creds {
		if string(creds[i].ID) == string(cred.ID) {
			creds[i] = *cred
			m[pub] = creds
			_ = s.save(m)
			return
		}
	}
}
