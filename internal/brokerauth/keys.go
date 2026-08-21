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
//
// The write is atomic: seed is written to a temporary file in the same
// directory as path, synced, and moved into place with os.Rename. A plain
// os.WriteFile truncates the target before writing, so a crash, power loss,
// or full disk between the truncate and the write would otherwise leave a
// zero-length seed file — a state worse than a missing one, since it is
// indistinguishable from "enrolled" to every caller that only checks
// existence, and the worker can never recover without an operator deleting
// the file by hand. Renaming within one directory is atomic on every
// platform sqi supports, so a reader always observes either the previous
// seed or the new one, never a truncated one. The temp file is created in
// filepath.Dir(path) rather than the system temp directory because a rename
// across filesystems is not atomic and can fail outright.
//
// The temp file is synced before the rename so the new bytes are not left
// sitting only in the page cache: a rename is atomic with respect to
// ordering, but on a hard power loss an unsynced write can still vanish,
// silently reverting a saved seed to whatever was on disk before it. The
// file is small enough (an nkey seed is a few dozen bytes) that this costs
// nothing worth avoiding.
//
// The final mode is 0600 regardless of what, if anything, existed at path
// before: the temp file is created 0600 by os.CreateTemp and that mode is
// set explicitly rather than relied upon, and the rename replaces whatever
// was there — including a more permissive leftover from key rotation, an
// earlier bug, or an operator who chmod'd it to look at it — with a file
// that was never observable under any mode but 0600.
func SaveSeed(path string, seed []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("brokerauth: create seed dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("brokerauth: create temp seed file: %w", err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()

	if chmodErr := tmp.Chmod(0o600); chmodErr != nil {
		_ = tmp.Close()
		return fmt.Errorf("brokerauth: chmod temp seed file %s: %w", tmpPath, chmodErr)
	}
	if _, writeErr := tmp.Write(seed); writeErr != nil {
		_ = tmp.Close()
		return fmt.Errorf("brokerauth: write temp seed file %s: %w", tmpPath, writeErr)
	}
	if syncErr := tmp.Sync(); syncErr != nil {
		_ = tmp.Close()
		return fmt.Errorf("brokerauth: sync temp seed file %s: %w", tmpPath, syncErr)
	}
	if closeErr := tmp.Close(); closeErr != nil {
		return fmt.Errorf("brokerauth: close temp seed file %s: %w", tmpPath, closeErr)
	}

	if renameErr := os.Rename(tmpPath, path); renameErr != nil {
		return fmt.Errorf("brokerauth: rename seed into place %s: %w", path, renameErr)
	}
	renamed = true
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
