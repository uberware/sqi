// SPDX-License-Identifier: AGPL-3.0-or-later

package oidc

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSealOpenState(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	s, err := NewFlowState()
	if err != nil {
		t.Fatalf("NewFlowState: %v", err)
	}
	sealed, err := SealState(key, s)
	if err != nil {
		t.Fatalf("SealState: %v", err)
	}

	got, err := OpenState(key, sealed)
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	if got != s {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, s)
	}

	tests := []struct {
		name   string
		key    []byte
		cookie string
	}{
		{name: "wrong key", key: []byte("ffffffffffffffffffffffffffffffff"), cookie: sealed},
		{name: "tampered payload", key: key, cookie: "x" + sealed[1:]},
		{name: "truncated", key: key, cookie: strings.Split(sealed, ".")[0]},
		{name: "empty", key: key, cookie: ""},
		// An empty key must not verify anything: HMAC with no key is a MAC an
		// attacker can compute, so accepting it would make the sole CSRF
		// defense on the public callback forgeable.
		{name: "nil key", key: nil, cookie: sealed},
		{name: "zero-length key", key: []byte{}, cookie: sealed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := OpenState(tt.key, tt.cookie); !errors.Is(err, ErrStateInvalid) {
				t.Fatalf("err = %v, want ErrStateInvalid", err)
			}
		})
	}
}

// TestState_EmptyKeyIsRefused pins that neither half of the seal/open pair will
// operate with no key. HMAC keyed with nothing is a signature an attacker can
// produce, and these routes are public, so a "valid" state cookie under an
// empty key is a forged callback that mints a real session. Refusing here means
// the security of the callback does not depend on the caller having remembered
// to supply a key.
func TestState_EmptyKeyIsRefused(t *testing.T) {
	fs, err := NewFlowState()
	if err != nil {
		t.Fatalf("NewFlowState: %v", err)
	}

	for _, key := range [][]byte{nil, {}} {
		if _, err := SealState(key, fs); !errors.Is(err, ErrNoSigningKey) {
			t.Errorf("SealState(%v) err = %v, want ErrNoSigningKey", key, err)
		}
	}

	// What an attacker would actually send: a payload they sealed themselves
	// under the empty key. Built by hand because SealState now refuses to.
	raw, err := json.Marshal(fs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	forged := payload + "." + mac(nil, payload)

	for _, key := range [][]byte{nil, {}} {
		if _, err := OpenState(key, forged); !errors.Is(err, ErrStateInvalid) {
			t.Errorf("OpenState(%v) err = %v, want ErrStateInvalid — an empty key must verify nothing", key, err)
		}
	}
}

// TestOpenState_RejectsExpiredState pins the server-side TTL. The cookie's own
// MaxAge binds only a cooperating browser; a captured cookie value replayed
// directly is bounded by nothing but this check.
func TestOpenState_RejectsExpiredState(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")

	tests := []struct {
		name     string
		issuedAt int64
		wantErr  bool
	}{
		{name: "fresh", issuedAt: time.Now().Unix()},
		{name: "just inside the TTL", issuedAt: time.Now().Add(-StateTTL + time.Minute).Unix()},
		{name: "just past the TTL", issuedAt: time.Now().Add(-StateTTL - time.Second).Unix(), wantErr: true},
		{name: "long expired", issuedAt: time.Now().Add(-24 * time.Hour).Unix(), wantErr: true},
		{name: "no issued-at at all", issuedAt: 0, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs, err := NewFlowState()
			if err != nil {
				t.Fatalf("NewFlowState: %v", err)
			}
			fs.IssuedAt = tt.issuedAt
			sealed, err := SealState(key, fs)
			if err != nil {
				t.Fatalf("SealState: %v", err)
			}

			got, err := OpenState(key, sealed)
			switch {
			case tt.wantErr:
				// Deliberately the same bare sentinel as a forged cookie: an
				// expired state must not be distinguishable from a fake one.
				if !errors.Is(err, ErrStateInvalid) {
					t.Fatalf("err = %v, want ErrStateInvalid", err)
				}
			case err != nil:
				t.Fatalf("OpenState: %v", err)
			case got != fs:
				t.Fatalf("round-trip mismatch: %+v vs %+v", got, fs)
			}
		})
	}
}

func TestNewFlowState_StampsIssuedAt(t *testing.T) {
	before := time.Now().Unix()
	fs, err := NewFlowState()
	if err != nil {
		t.Fatalf("NewFlowState: %v", err)
	}
	if fs.IssuedAt < before || fs.IssuedAt > time.Now().Unix() {
		t.Fatalf("IssuedAt = %d, want a stamp within [%d, now]", fs.IssuedAt, before)
	}
}

func TestNewFlowState_ValuesAreDistinctAndSized(t *testing.T) {
	a, err := NewFlowState()
	if err != nil {
		t.Fatalf("NewFlowState: %v", err)
	}
	b, err := NewFlowState()
	if err != nil {
		t.Fatalf("NewFlowState: %v", err)
	}
	if a.State == b.State || a.Nonce == b.Nonce || a.Verifier == b.Verifier {
		t.Fatal("two flow states share a value; they must be independently random")
	}
	// RFC 7636 §4.1 requires a verifier of 43-128 characters.
	if len(a.Verifier) < 43 || len(a.Verifier) > 128 {
		t.Fatalf("verifier length %d, want 43..128", len(a.Verifier))
	}
	if a.Challenge() == a.Verifier {
		t.Fatal("challenge must be the SHA-256 of the verifier, not the verifier itself")
	}
}

func TestNewSigningKey_IsRandomAnd256Bit(t *testing.T) {
	a, err := NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey: %v", err)
	}
	b, err := NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey: %v", err)
	}
	if len(a) != 32 {
		t.Fatalf("key length %d, want 32", len(a))
	}
	if string(a) == string(b) {
		t.Fatal("two boots produced the same signing key")
	}
}
