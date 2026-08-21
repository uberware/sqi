// SPDX-License-Identifier: AGPL-3.0-or-later

package brokerauth_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/brokerauth"
)

func TestGenerateJoinToken(t *testing.T) {
	tok, hash, prefix, err := brokerauth.GenerateJoinToken()
	if err != nil {
		t.Fatalf("GenerateJoinToken: %v", err)
	}
	if !strings.HasPrefix(tok, "sqiw_") {
		t.Errorf("token %q lacks the sqiw_ prefix", tok)
	}
	if strings.Contains(hash, tok) {
		t.Error("hash contains the raw token")
	}
	if !strings.HasPrefix(tok, prefix) {
		t.Errorf("prefix %q is not a prefix of token %q", prefix, tok)
	}
	if got := brokerauth.HashJoinToken(tok); got != hash {
		t.Errorf("HashJoinToken = %q, want %q", got, hash)
	}
}

func TestGenerateJoinToken_Unique(t *testing.T) {
	seen := make(map[string]bool, 100)
	for range 100 {
		tok, _, _, err := brokerauth.GenerateJoinToken()
		if err != nil {
			t.Fatalf("GenerateJoinToken: %v", err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token %q", tok)
		}
		seen[tok] = true
	}
}
