// SPDX-License-Identifier: AGPL-3.0-or-later

// Package password hashes and verifies user passwords with argon2id and mints
// opaque session tokens. The encoded hash embeds the algorithm parameters and
// salt so parameters can be raised later without invalidating existing hashes.
package password

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// OWASP-baseline argon2id parameters.
const (
	argonMemory  = 19456 // KiB (19 MiB)
	argonTime    = 2
	argonThreads = 1
	argonSaltLen = 16
	argonKeyLen  = 32
)

// ErrInvalidHash is returned when an encoded hash cannot be parsed.
var ErrInvalidHash = errors.New("password: invalid encoded hash")

// Hash returns an argon2id encoded hash of plaintext:
// $argon2id$v=19$m=19456,t=2,p=1$<b64salt>$<b64hash>.
func Hash(plaintext string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(plaintext), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify reports whether plaintext matches the argon2id encoded hash.
func Verify(encoded, plaintext string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrInvalidHash
	}
	var memory, time, threads int
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, ErrInvalidHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrInvalidHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrInvalidHash
	}
	got := argon2.IDKey(
		[]byte(plaintext), salt,
		uint32(time), uint32(memory), uint8(threads), uint32(len(want)), //nolint:gosec // hash format controlled by us
	)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// GenerateToken returns a 256-bit URL-safe random token (the raw session
// secret handed to the client; only its HashToken digest is stored).
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("password: read token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the hex SHA-256 of a session token, for at-rest storage and
// constant-length lookup.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
