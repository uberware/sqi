// SPDX-License-Identifier: AGPL-3.0-or-later

package oidc

import (
	"errors"
	"strings"
	"testing"
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := OpenState(tt.key, tt.cookie); !errors.Is(err, ErrStateInvalid) {
				t.Fatalf("err = %v, want ErrStateInvalid", err)
			}
		})
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
