// SPDX-License-Identifier: AGPL-3.0-or-later

package password_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/auth/password"
)

func TestHashVerify(t *testing.T) {
	enc, err := password.Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(enc, "$argon2id$") {
		t.Fatalf("encoded hash not argon2id: %q", enc)
	}
	if strings.Contains(enc, "correct horse") {
		t.Fatal("encoded hash leaks the plaintext")
	}

	ok, err := password.Verify(enc, "correct horse battery staple")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Fatal("Verify returned false for the correct password")
	}

	ok, err = password.Verify(enc, "wrong password")
	if err != nil {
		t.Fatalf("Verify (wrong): %v", err)
	}
	if ok {
		t.Fatal("Verify returned true for a wrong password")
	}
}

func TestHashIsSalted(t *testing.T) {
	a, _ := password.Hash("same") //nolint:errcheck // test accepts any valid hash or error
	b, _ := password.Hash("same") //nolint:errcheck // test accepts any valid hash or error
	if a == b {
		t.Fatal("two hashes of the same password are identical (salt missing)")
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	if ok, err := password.Verify("not-a-hash", "x"); err == nil || ok {
		t.Fatalf("Verify on garbage should error, got ok=%v err=%v", ok, err)
	}
}

func TestGenerateTokenAndHash(t *testing.T) {
	tok, err := password.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if len(tok) < 40 {
		t.Fatalf("token too short: %q", tok)
	}
	if tok2, _ := password.GenerateToken(); tok == tok2 { //nolint:errcheck // test accepts any valid token or error
		t.Fatal("GenerateToken returned identical tokens")
	}
	h := password.HashToken(tok)
	if h == tok {
		t.Fatal("HashToken returned the raw token")
	}
	if password.HashToken(tok) != h {
		t.Fatal("HashToken is not deterministic")
	}
}
