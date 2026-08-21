// SPDX-License-Identifier: AGPL-3.0-or-later

// Package brokerauth holds the credential primitives shared by sqi-server and
// sqi-worker for NATS broker authentication.
//
// It is deliberately a LEAF package: it imports neither internal/store nor
// internal/openjd, so the worker binary — which can never import the latter —
// may use it directly.
package brokerauth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nats-io/nkeys"
)

// GenerateSeed creates a new Ed25519 user nkey and returns its seed and the
// corresponding public key. The public key is returned alongside so that no
// caller has to derive it, and so none can derive it wrongly.
func GenerateSeed() (seed []byte, publicKey string, err error) {
	kp, err := nkeys.CreateUser()
	if err != nil {
		return nil, "", fmt.Errorf("brokerauth: create user key: %w", err)
	}
	seed, err = kp.Seed()
	if err != nil {
		return nil, "", fmt.Errorf("brokerauth: extract seed: %w", err)
	}
	publicKey, err = kp.PublicKey()
	if err != nil {
		return nil, "", fmt.Errorf("brokerauth: extract public key: %w", err)
	}
	return seed, publicKey, nil
}

// PublicKeyFromSeed derives the public key for a seed.
func PublicKeyFromSeed(seed []byte) (string, error) {
	kp, err := nkeys.FromSeed(seed)
	if err != nil {
		return "", fmt.Errorf("brokerauth: parse seed: %w", err)
	}
	pub, err := kp.PublicKey()
	if err != nil {
		return "", fmt.Errorf("brokerauth: extract public key: %w", err)
	}
	return pub, nil
}

// SaveSeed writes seed to path with owner-only permissions, creating parent
// directories as needed.
func SaveSeed(path string, seed []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("brokerauth: create seed dir: %w", err)
	}
	if err := os.WriteFile(path, seed, 0o600); err != nil {
		return fmt.Errorf("brokerauth: write seed %s: %w", path, err)
	}
	return nil
}

// LoadSeed reads a seed file, refusing one that is readable beyond its owner.
//
// The check is a real one, not hygiene theater: this seed IS the worker's
// identity, and a group-readable seed on a shared render node hands that
// identity to every account on the box. The mode check is skipped on Windows,
// where POSIX bits do not carry the same meaning.
func LoadSeed(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("brokerauth: stat seed %s: %w", path, err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return nil, fmt.Errorf(
				"brokerauth: seed file %s is mode %o; it must be readable only by its owner — run: chmod 600 %s",
				path, perm, path,
			)
		}
	}
	seed, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("brokerauth: read seed %s: %w", path, err)
	}
	if _, err := nkeys.FromSeed(seed); err != nil {
		return nil, fmt.Errorf("brokerauth: seed file %s is not a valid nkey seed: %w", path, err)
	}
	return seed, nil
}

// ValidatePublicKey reports whether pk is a well-formed user nkey.
func ValidatePublicKey(pk string) error {
	if !strings.HasPrefix(pk, "U") {
		return errors.New("brokerauth: public key must be a user nkey (starts with 'U')")
	}
	if !nkeys.IsValidPublicUserKey(pk) {
		return errors.New("brokerauth: public key is not a valid user nkey")
	}
	return nil
}
