// RS256 (RSA-SHA256) JWT minting/verification and the JWKS document. We
// sign with RS256 rather than EdDSA: real-world OIDC clients (Gitea via
// coreos/go-oidc, and many others) only accept RS256/ES256/PS256 and
// reject EdDSA at verification. A JWS is base64url(header).base64url(
// payload).base64url(sig); the signature is RSASSA-PKCS1-v1_5 over
// SHA-256 of "header.payload". kid identifies the signing key in the JWKS.
package oidc

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// b64 is base64url without padding, per JOSE (RFC 7515 §2).
var b64 = base64.RawURLEncoding

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// mintRS256 signs claims as an RS256 JWS compact serialization.
func mintRS256(priv *rsa.PrivateKey, kid string, claims any) (string, error) {
	hb, err := json.Marshal(jwtHeader{Alg: "RS256", Typ: "JWT", Kid: kid})
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := b64.EncodeToString(hb) + "." + b64.EncodeToString(cb)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64.EncodeToString(sig), nil
}

// verifyRS256 checks the RSA signature against pub and returns the raw
// claims. It does NOT enforce exp/aud/iss — the caller applies policy.
func verifyRS256(token string, pub *rsa.PublicKey) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("oidc: malformed jwt")
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("oidc: bad signature encoding")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return nil, errors.New("oidc: signature mismatch")
	}
	cb, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("oidc: bad payload encoding")
	}
	var claims map[string]any
	if err := json.Unmarshal(cb, &claims); err != nil {
		return nil, fmt.Errorf("oidc: claims: %w", err)
	}
	return claims, nil
}

// jwk is one key in the JWKS. For RSA: kty=RSA with modulus n and
// exponent e (both base64url big-endian).
type jwk struct {
	Kty string `json:"kty"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
	Kid string `json:"kid,omitempty"`
	N   string `json:"n,omitempty"`
	E   string `json:"e,omitempty"`
}

type jwks struct {
	Keys []jwk `json:"keys"`
}

// rsaJWK renders an RSA public key as a JWKS entry.
func rsaJWK(pub *rsa.PublicKey, kid string) jwk {
	return jwk{
		Kty: "RSA",
		Use: "sig",
		Alg: "RS256",
		Kid: kid,
		N:   b64.EncodeToString(pub.N.Bytes()),
		E:   b64.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}
