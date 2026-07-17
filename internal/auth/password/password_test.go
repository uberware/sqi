// SPDX-License-Identifier: AGPL-3.0-or-later

package password_test

import (
	"encoding/base64"
	"errors"
	"fmt"
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
	a, err := password.Hash("same")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	b, err := password.Hash("same")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
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
	tok2, err := password.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if tok == tok2 {
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

// TestVerifyRejectsMalformedHashes covers hand-crafted and tampered encoded
// hashes that must be rejected with ErrInvalidHash rather than panicking,
// hanging, or (worse) silently reporting a password mismatch as the reason.
func TestVerifyRejectsMalformedHashes(t *testing.T) {
	valid, err := password.Hash("reference")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	parts := strings.Split(valid, "$")
	if len(parts) != 6 {
		t.Fatalf("unexpected valid hash shape: %q", valid)
	}
	salt, key := parts[4], parts[5]

	tests := []struct {
		name    string
		encoded string
	}{
		{
			name:    "wrong part count",
			encoded: "$argon2id$v=19$m=19456,t=2,p=1$" + salt,
		},
		{
			name:    "wrong algorithm segment",
			encoded: fmt.Sprintf("$argon2i$v=19$m=19456,t=2,p=1$%s$%s", salt, key),
		},
		{
			name:    "non-numeric params",
			encoded: fmt.Sprintf("$argon2id$v=19$m=abc,t=2,p=1$%s$%s", salt, key),
		},
		{
			name:    "bad version",
			encoded: fmt.Sprintf("$argon2id$v=1$m=19456,t=2,p=1$%s$%s", salt, key),
		},
		{
			name:    "mismatched version",
			encoded: fmt.Sprintf("$argon2id$v=99$m=19456,t=2,p=1$%s$%s", salt, key),
		},
		{
			name:    "negative memory",
			encoded: fmt.Sprintf("$argon2id$v=19$m=-1,t=2,p=1$%s$%s", salt, key),
		},
		{
			name:    "negative time",
			encoded: fmt.Sprintf("$argon2id$v=19$m=19456,t=-1,p=1$%s$%s", salt, key),
		},
		{
			name:    "negative threads",
			encoded: fmt.Sprintf("$argon2id$v=19$m=19456,t=2,p=-1$%s$%s", salt, key),
		},
		{
			name:    "oversized memory",
			encoded: fmt.Sprintf("$argon2id$v=19$m=99999999999,t=2,p=1$%s$%s", salt, key),
		},
		{
			name:    "oversized iterations",
			encoded: fmt.Sprintf("$argon2id$v=19$m=19456,t=2000000000,p=1$%s$%s", salt, key),
		},
		{
			name:    "truncated/invalid base64 salt",
			encoded: "$argon2id$v=19$m=19456,t=2,p=1$not-valid-b64!!!$" + key,
		},
		{
			name:    "truncated/invalid base64 key",
			encoded: fmt.Sprintf("$argon2id$v=19$m=19456,t=2,p=1$%s$not-valid-b64!!!", salt),
		},
		{
			name:    "empty salt",
			encoded: "$argon2id$v=19$m=19456,t=2,p=1$$" + key,
		},
		{
			name:    "empty key",
			encoded: fmt.Sprintf("$argon2id$v=19$m=19456,t=2,p=1$%s$", salt),
		},
		{
			// One salt byte short of the required 16. Must be rejected
			// outright rather than accepted with a weaker salt.
			name:    "wrong-length salt (too short)",
			encoded: fmt.Sprintf("$argon2id$v=19$m=19456,t=2,p=1$%s$%s", salt[:len(salt)-2], key),
		},
		{
			// A 1-byte (base64 "AA") key field. Before the fix, Verify
			// derived a comparison key of length len(want), so this would
			// make ConstantTimeCompare guard only ~8 bits instead of 256.
			name:    "wrong-length key (1 byte)",
			encoded: fmt.Sprintf("$argon2id$v=19$m=19456,t=2,p=1$%s$AA", salt),
		},
		{
			// One key byte short of the required 32.
			name:    "wrong-length key (too short)",
			encoded: fmt.Sprintf("$argon2id$v=19$m=19456,t=2,p=1$%s$%s", salt, key[:len(key)-2]),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, err := password.Verify(tt.encoded, "anything")
			if !errors.Is(err, password.ErrInvalidHash) {
				t.Fatalf("Verify(%q): err = %v, want ErrInvalidHash", tt.encoded, err)
			}
			if ok {
				t.Fatalf("Verify(%q): ok = true, want false", tt.encoded)
			}
		})
	}
}

// TestVerifyBitFlippedHashIsWrongPasswordNotError covers a well-formed
// encoded hash (correct part count, valid version, sane parameters, correct
// salt/key lengths) whose key bytes have been corrupted by a single bit
// flip. This must be reported as a plain verification failure — (false,
// nil) — not as ErrInvalidHash: the encoding is structurally valid, so a
// caller can't distinguish "wrong password" from "corrupted database row"
// and must not leak that distinction either.
func TestVerifyBitFlippedHashIsWrongPasswordNotError(t *testing.T) {
	valid, err := password.Hash("reference")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	parts := strings.Split(valid, "$")
	if len(parts) != 6 {
		t.Fatalf("unexpected valid hash shape: %q", valid)
	}
	keyBytes, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	keyBytes[0] ^= 0x01 // flip one bit, keep the length identical
	parts[5] = base64.RawStdEncoding.EncodeToString(keyBytes)
	tampered := strings.Join(parts, "$")

	ok, err := password.Verify(tampered, "reference")
	if err != nil {
		t.Fatalf("Verify(bit-flipped): err = %v, want nil", err)
	}
	if ok {
		t.Fatal("Verify(bit-flipped): ok = true, want false")
	}
}
