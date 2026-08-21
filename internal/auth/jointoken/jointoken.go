// SPDX-License-Identifier: AGPL-3.0-or-later

// Package jointoken mints and hashes worker join tokens: the one-time or
// reusable secrets an operator issues (via the CLI or the REST API) and a
// worker redeems to enroll and receive its broker credential. It is
// server-side only — issuance and redemption both happen on sqi-server — and
// reuses the same random-token and hashing primitives as session tokens and
// API keys (internal/auth/password) rather than re-deriving them.
package jointoken

import (
	"fmt"

	"github.com/uberware/sqi/internal/auth/password"
)

// prefix marks a raw token as a worker join token, distinguishing it at a
// glance from an API key (which carries the "sqi_" prefix) in logs and
// operator tooling.
const prefix = "sqiw_"

// prefixLen is how many leading characters of the raw token are returned for
// list identification only. It is never used to look up or authenticate a
// token (that goes through the full-token hash), so storing a short,
// non-secret slice of the raw token leaks nothing usable.
const prefixLen = 12

// Generate creates a new random worker join token. It returns the raw token
// (shown to the operator exactly once), its SHA-256 hash (the only form
// stored at rest), and a short display prefix for list identification.
func Generate() (token, hash, displayPrefix string, err error) {
	raw, err := password.GenerateToken()
	if err != nil {
		return "", "", "", fmt.Errorf("jointoken: generate: %w", err)
	}
	token = prefix + raw
	return token, Hash(token), token[:prefixLen], nil
}

// Hash returns the hex SHA-256 of a join token, for at-rest storage and
// constant-length lookup.
func Hash(token string) string {
	return password.HashToken(token)
}
