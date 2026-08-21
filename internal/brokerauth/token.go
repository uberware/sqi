// SPDX-License-Identifier: AGPL-3.0-or-later

package brokerauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// joinTokenPrefix marks a raw token as a worker join token, distinguishing it
// at a glance from an API key (which carries the "sqi_" prefix) in logs and
// operator tooling.
const joinTokenPrefix = "sqiw_"

// joinTokenPrefixLen is how many leading characters of the raw token are
// returned for list identification only. It is never used to look up or
// authenticate a token (that goes through the full-token hash), so storing a
// short, non-secret slice of the raw token leaks nothing usable.
const joinTokenPrefixLen = 12

// GenerateJoinToken creates a new random worker join token. It returns the raw
// token (shown to the operator exactly once), its SHA-256 hash (the only form
// stored at rest), and a short display prefix for list identification.
func GenerateJoinToken() (token, hash, prefix string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", "", fmt.Errorf("brokerauth: read token: %w", err)
	}
	token = joinTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, HashJoinToken(token), token[:joinTokenPrefixLen], nil
}

// HashJoinToken returns the hex SHA-256 of a join token, for at-rest storage
// and constant-length lookup.
func HashJoinToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
