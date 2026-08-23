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

	"github.com/uberware/sqi/internal/fsutil"

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
// The result is owner-only regardless of what, if anything, existed at path
// before: fsutil.WriteSecret restricts the temp file before its first byte is
// written, and the rename replaces whatever was there — a more permissive
// leftover from key rotation, an earlier bug, or an operator who chmod'd it to
// look at it — with a file that was never observable unrestricted. On NTFS the
// rename carries the file's own security descriptor with it, and that
// descriptor is PROTECTED, so the seed does not re-inherit the directory's
// access on arrival.
//
// Because the write goes through a temporary file created in the same
// directory as path, the caller needs write permission on that CONTAINING
// DIRECTORY, not just on the seed file itself: a directory an operator locked
// down to 0500 while leaving an owner-writable seed inside it will fail here
// even though a direct write to the existing file would have succeeded.
func SaveSeed(path string, seed []byte) error {
	dir := filepath.Dir(path)
	if err := fsutil.MkdirSecret(dir); err != nil {
		return fmt.Errorf("brokerauth: create seed dir: %w", err)
	}

	// os.CreateTemp reserves a unique name; it is closed immediately and the
	// seed goes in through fsutil.WriteSecret, which reopens with the access
	// rights needed to set an ACL and applies that ACL BEFORE the first byte
	// is written. Chmod(0o600) on the CreateTemp handle — what this used to do
	// — is a no-op on Windows, where os.Chmod maps only to the read-only
	// attribute and cannot deny read access to anyone, so the nkey seed landed
	// with whatever DACL it inherited. The empty placeholder that exists in
	// between carries no secret, so its inherited access discloses nothing.
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
	if closeErr := tmp.Close(); closeErr != nil {
		return fmt.Errorf("brokerauth: close temp seed file %s: %w", tmpPath, closeErr)
	}

	if writeErr := fsutil.WriteSecret(tmpPath, seed); writeErr != nil {
		return fmt.Errorf("brokerauth: write temp seed file %s: %w", tmpPath, writeErr)
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
// identity, and a seed readable by other accounts on a shared render node hands
// that identity to every one of them.
//
// It runs on EVERY platform. An earlier revision skipped it on Windows on the
// grounds that "POSIX bits do not carry the same meaning there" — true of the
// bits, and the wrong conclusion: the property being asserted is not "the mode
// is 0600", it is "nobody else can read this". Windows expresses that through a
// DACL instead, and skipping meant the one platform where SaveSeed could not
// actually restrict the file was also the one that never checked. The writer
// and the reader shared a blind spot, so nothing in the system could report it.
// fsutil.IsRestricted answers the real question on both.
func LoadSeed(path string) ([]byte, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("brokerauth: stat seed %s: %w", path, err)
	}
	restricted, err := fsutil.IsRestricted(path)
	if err != nil {
		return nil, fmt.Errorf("brokerauth: check seed %s: %w", path, err)
	}
	if !restricted {
		return nil, fmt.Errorf(
			"brokerauth: seed file %s is readable beyond its owner; it holds this worker's "+
				"private key and must be restricted — %s",
			path, restrictHint(path),
		)
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

// restrictHint returns the platform-appropriate remediation for a seed file
// that is readable beyond its owner.
//
// Only the ADVICE is platform-specific. The check itself
// (fsutil.IsRestricted) is not, and deliberately so: an earlier revision
// skipped the whole check on Windows, which meant the one platform where
// SaveSeed could not actually restrict the file was also the one that never
// verified it — the writer and the reader had the same blind spot, so nothing
// could report it.
func restrictHint(path string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(
			"remove inherited access with: icacls %q /inheritance:r /grant:r %%USERNAME%%:F", path,
		)
	}
	return "run: chmod 600 " + path
}
