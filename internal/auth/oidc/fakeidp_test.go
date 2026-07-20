// SPDX-License-Identifier: AGPL-3.0-or-later

package oidc

// A minimal OpenID provider for tests: a discovery document, a JWKS, and a
// token endpoint that mints a real RS256-signed ID token.
//
// Every knob breaks exactly ONE thing, so each rejection test isolates the
// single rule it is about. A fake that could only be broken in one lump would
// pass even if the implementation checked just one of the five properties.
//
// Its limit, which no amount of care removes: a fake returns whatever it is
// told to return, so it cannot demonstrate what a REAL provider omits or
// spells differently. That is what the Keycloak integration test is for.

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

type fakeIDP struct {
	t   *testing.T
	srv *httptest.Server

	signKey  *rsa.PrivateKey // published in the JWKS
	rogueKey *rsa.PrivateKey // never published

	// Claim values used when no knob overrides them.
	subject     string
	username    string
	displayName string
	groups      any // []string, or a bare string, or nil to omit the claim
	nonce       string

	// Knobs. Each breaks exactly one property.
	issuerOverride      string        // wrong "iss" claim
	audienceOverride    string        // wrong "aud" claim
	expiryOffset        time.Duration // negative expires the token
	signWithRogueKey    bool          // signature from an unpublished key
	nonceOverride       string        // wrong "nonce" claim
	discoveryFails      bool          // discovery endpoint returns 500
	noEndSessionSupport bool          // discovery omits end_session_endpoint

	// lastTokenForm records the last form posted to /token, so tests can
	// assert PKCE parameters actually reach the provider.
	lastTokenForm url.Values
}

const testClientID = "sqi-test-client"

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()

	signKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	rogueKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rogue key: %v", err)
	}

	f := &fakeIDP{
		t:            t,
		signKey:      signKey,
		rogueKey:     rogueKey,
		subject:      "sub-1",
		username:     "alice",
		displayName:  "Alice Example",
		groups:       []string{"artists"},
		nonce:        "nonce-1",
		expiryOffset: time.Hour,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", f.handleDiscovery)
	mux.HandleFunc("/jwks", f.handleJWKS)
	mux.HandleFunc("/token", f.handleToken)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// URL is the issuer URL to configure a Provider with.
func (f *fakeIDP) URL() string { return f.srv.URL }

func (f *fakeIDP) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	if f.discoveryFails {
		http.Error(w, "provider unavailable", http.StatusInternalServerError)
		return
	}
	doc := map[string]any{
		// Note: this is always the real issuer. go-oidc rejects a discovery
		// document whose issuer disagrees with the URL it was fetched from, so
		// the wrong-issuer knob applies to the ID token's "iss" claim instead
		// — which is the check under test.
		"issuer":                                f.srv.URL,
		"authorization_endpoint":                f.srv.URL + "/authorize",
		"token_endpoint":                        f.srv.URL + "/token",
		"jwks_uri":                              f.srv.URL + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
	if !f.noEndSessionSupport {
		doc["end_session_endpoint"] = f.srv.URL + "/logout"
	}
	writeJSON(f.t, w, doc)
}

func (f *fakeIDP) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	pub := &f.signKey.PublicKey
	writeJSON(f.t, w, map[string]any{"keys": []any{map[string]any{
		"kty": "RSA",
		"kid": "test-key",
		"use": "sig",
		"alg": "RS256",
		"n":   b64(pub.N.Bytes()),
		"e":   b64(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

func (f *fakeIDP) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f.lastTokenForm = r.PostForm

	writeJSON(f.t, w, map[string]any{
		"access_token": "access-token-1",
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     f.idToken(),
	})
}

// idToken mints an ID token honoring whichever knobs are set.
func (f *fakeIDP) idToken() string {
	issuer := f.srv.URL
	if f.issuerOverride != "" {
		issuer = f.issuerOverride
	}
	audience := testClientID
	if f.audienceOverride != "" {
		audience = f.audienceOverride
	}
	nonce := f.nonce
	if f.nonceOverride != "" {
		nonce = f.nonceOverride
	}
	now := time.Now()

	claims := map[string]any{
		"iss":                issuer,
		"aud":                audience,
		"sub":                f.subject,
		"iat":                now.Add(-time.Minute).Unix(),
		"exp":                now.Add(f.expiryOffset).Unix(),
		"nonce":              nonce,
		"preferred_username": f.username,
		"name":               f.displayName,
	}
	if f.groups != nil {
		claims["groups"] = f.groups
	}

	key := f.signKey
	if f.signWithRogueKey {
		key = f.rogueKey
	}
	return signJWT(f.t, key, claims)
}

// signJWT produces a compact RS256 JWS over claims. Hand-rolled rather than
// pulling in a JOSE library: the test needs to sign with a key the server does
// not publish, which is easier to state plainly than to configure.
func signJWT(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "test-key"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	signing := b64(header) + "." + b64(payload)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signing + "." + b64(sig)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("write json: %v", err)
	}
}
