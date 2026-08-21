// SPDX-License-Identifier: AGPL-3.0-or-later

package jointoken_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/auth/jointoken"
)

func TestGenerate(t *testing.T) {
	tok, hash, prefix, err := jointoken.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
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
	if got := jointoken.Hash(tok); got != hash {
		t.Errorf("Hash = %q, want %q", got, hash)
	}
}

func TestGenerate_Unique(t *testing.T) {
	seen := make(map[string]bool, 100)
	for range 100 {
		tok, _, _, err := jointoken.Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token %q", tok)
		}
		seen[tok] = true
	}
}
