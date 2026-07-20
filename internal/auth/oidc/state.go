// SPDX-License-Identifier: AGPL-3.0-or-later

package oidc

// Per-login flow state: the CSRF state parameter, the ID-token nonce, and the
// PKCE verifier.
//
// Held in a signed cookie rather than a server-side table. The browser that
// starts the flow is the browser that finishes it, seconds later, so a table
// would need a cleanup job and would still lose in-flight logins on restart.
// The cookie is HttpOnly, so page script cannot read or forge it; the HMAC
// additionally defeats a cookie-tossing sibling subdomain, which HttpOnly
// alone does not.
//
// The signing key is generated per boot and never persisted: a restart
// invalidating in-flight logins is an acceptable cost for having no key to
// configure, distribute, or rotate.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// ErrStateInvalid means the state cookie was missing, malformed, or not signed
// by this server. Callers must not distinguish these in what they show a user.
var ErrStateInvalid = errors.New("oidc: state cookie invalid or expired")

// FlowState is the set of one-time values bound to a single login attempt.
type FlowState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
}

// NewFlowState mints independently-random values for one login attempt.
func NewFlowState() (FlowState, error) {
	var out FlowState
	for _, dst := range []*string{&out.State, &out.Nonce, &out.Verifier} {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return FlowState{}, err
		}
		// Unpadded base64url of 32 bytes is 43 chars — exactly the RFC 7636
		// minimum verifier length, and URL-safe without escaping.
		*dst = base64.RawURLEncoding.EncodeToString(b)
	}
	return out, nil
}

// Challenge returns the PKCE S256 code challenge for this state's verifier.
func (s FlowState) Challenge() string {
	sum := sha256.Sum256([]byte(s.Verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// NewSigningKey returns a random per-boot HMAC key for SealState/OpenState.
func NewSigningKey() ([]byte, error) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, nil
}

// SealState encodes s as "<payload>.<mac>" for use as a cookie value.
func SealState(key []byte, s FlowState) (string, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + mac(key, payload), nil
}

// OpenState verifies and decodes a cookie produced by SealState.
func OpenState(key []byte, cookie string) (FlowState, error) {
	payload, sig, ok := strings.Cut(cookie, ".")
	if !ok || payload == "" {
		return FlowState{}, ErrStateInvalid
	}
	// Constant-time: a byte-by-byte comparison would leak how much of a forged
	// MAC is correct, letting an attacker construct one a byte at a time.
	if !hmac.Equal([]byte(sig), []byte(mac(key, payload))) {
		return FlowState{}, ErrStateInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return FlowState{}, ErrStateInvalid
	}
	var out FlowState
	if err := json.Unmarshal(raw, &out); err != nil {
		return FlowState{}, ErrStateInvalid
	}
	if out.State == "" || out.Nonce == "" || out.Verifier == "" {
		return FlowState{}, ErrStateInvalid
	}
	return out, nil
}

func mac(key []byte, payload string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
