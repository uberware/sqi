// SPDX-License-Identifier: AGPL-3.0-or-later

// Package brokerauth holds the credential primitives shared by sqi-server and
// sqi-worker for NATS broker authentication.
//
// It is a leaf package: it imports neither internal/store nor internal/openjd,
// so the worker binary, which can never import the latter, may use it
// directly.
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
	return generate(nkeys.CreateUser, "user")
}

// GenerateAccountSeed creates a new Ed25519 account nkey and returns its seed
// and public key (which starts with "A"). It mirrors [GenerateSeed] for the
// account identity that signs user JWTs.
func GenerateAccountSeed() (seed []byte, publicKey string, err error) {
	return generate(nkeys.CreateAccount, "account")
}

// generate creates a key with create and returns its seed and public key.
func generate(create func() (nkeys.KeyPair, error), kind string) (seed []byte, publicKey string, err error) {
	kp, err := create()
	if err != nil {
		return nil, "", fmt.Errorf("brokerauth: create %s key: %w", kind, err)
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
// directory as path, synced, and moved into place with os.Rename, so a reader
// always sees either the previous seed or the new one. A truncated or
// zero-length seed file would read as "enrolled" to every caller that checks
// only existence, and the worker could not recover without an operator
// deleting it. The temp file lives in filepath.Dir(path), not the system temp
// directory, because a rename across filesystems is not atomic and can fail.
// The sync before the rename keeps a hard power loss from reverting a saved
// seed to the previous contents.
//
// The result is owner-only whatever existed at path before:
// fsutil.WriteSecret restricts the temp file before its first byte is
// written, and the rename replaces any more permissive file (a leftover from
// key rotation, or one an operator chmod'd to inspect it) with one that was
// never readable by others. On NTFS the rename carries the file's own
// security descriptor, which is protected, so the seed does not re-inherit
// the directory's access.
//
// The caller needs write permission on the directory containing path, not
// only on the seed file: a directory locked down to 0500 around an
// owner-writable seed fails here.
func SaveSeed(path string, seed []byte) error {
	dir := filepath.Dir(path)
	if err := fsutil.MkdirSecret(dir); err != nil {
		return fmt.Errorf("brokerauth: create seed dir: %w", err)
	}

	// os.CreateTemp reserves a unique name; it is closed immediately and the
	// seed goes in through fsutil.WriteSecret, which reopens with the access
	// rights needed to set an ACL and applies that ACL before the first byte
	// is written. Chmod(0o600) on the CreateTemp handle would be a no-op on
	// Windows, where os.Chmod maps only to the read-only attribute and cannot
	// deny read access to anyone. The empty placeholder that exists in
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
// The seed is the worker's identity: a seed readable by other accounts on a
// shared render node hands that identity to every one of them.
//
// The check runs on every platform. The property is "nobody else can read
// this", which POSIX expresses through mode bits and Windows through a DACL;
// fsutil.IsRestricted checks whichever applies.
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
			path, RestrictHint(path),
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

// RestrictHint returns the platform-appropriate remediation for a seed file
// that is readable beyond its owner.
//
// Only the advice is platform-specific. The check itself
// (fsutil.IsRestricted) is not: see LoadSeed.
func RestrictHint(path string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(
			"remove inherited access with: icacls %q /inheritance:r /grant:r %%USERNAME%%:F", path,
		)
	}
	return "run: chmod 600 " + path
}
