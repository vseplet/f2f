package oidc

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Standalone passkey registration — used by the profile UI to enroll a passkey
// outside the /authorize login flow. Keyed by peer pub (the same store OIDC
// login reads), so a passkey registered here works for login too. (Slice B
// quick path; a user_id-anchored passkey in block.user comes later.)

// RegisterBegin starts a passkey registration for pub and returns the
// CredentialCreationOptions the browser feeds to navigator.credentials.create.
func (s *Service) RegisterBegin(r *http.Request, pub, name string) (json.RawMessage, error) {
	wa, err := newWebAuthn(originFor(r))
	if err != nil {
		return nil, err
	}
	user := &waUser{pub: pub, name: name, creds: s.creds.Get(pub)}
	opts, sd, err := wa.BeginRegistration(user)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.regSess[pub] = sd
	s.mu.Unlock()
	return json.Marshal(opts)
}

// RegisterFinish completes the registration from the browser's attestation
// response (read off r) and persists the new credential under pub.
func (s *Service) RegisterFinish(r *http.Request, pub, name string) error {
	s.mu.Lock()
	sd := s.regSess[pub]
	delete(s.regSess, pub)
	s.mu.Unlock()
	if sd == nil {
		return fmt.Errorf("no pending passkey registration")
	}
	wa, err := newWebAuthn(originFor(r))
	if err != nil {
		return err
	}
	user := &waUser{pub: pub, name: name, creds: s.creds.Get(pub)}
	cred, err := wa.FinishRegistration(user, *sd, r)
	if err != nil {
		return err
	}
	return s.creds.Add(pub, *cred)
}

// HasCred reports whether pub has at least one registered passkey.
func (s *Service) HasCred(pub string) bool { return len(s.creds.Get(pub)) > 0 }
