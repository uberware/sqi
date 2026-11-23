// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package fsutil

import (
	"os"
	"syscall"
)

// createSecret creates path at [SecretFileMode], truncating any existing file.
//
// The mode is passed to the open itself rather than chmod'd afterwards, so a
// newly created file never exists at a wider mode with secret bytes already
// in it. umask can only remove bits from an open mode, never add them, so a
// permissive umask cannot widen this; a restrictive one that strips owner bits
// makes the caller's own subsequent write fail rather than exposing anything.
//
// The restrictSecret call is still needed: open(2) applies its mode argument
// only when it creates the file, so replacing an existing permissive key
// would keep the old mode and write the new private key into it.
// `sqi-server tls issue` re-issues a leaf over a previous one, which takes
// that path. Windows's createSecret re-applies the DACL unconditionally for
// the same reason.
//
// O_TRUNC empties the file as part of the open, so at the moment of the
// fchmod a pre-existing permissive file holds no bytes, and the caller's first
// write lands in a file already narrowed.
func createSecret(path string) (*os.File, error) {
	return openSecret(path, os.O_TRUNC)
}

// createSecretExclusive is [createSecret] for a path that must not exist yet:
// it fails with an error matching fs.ErrExist rather than truncating.
func createSecretExclusive(path string) (*os.File, error) {
	return openSecret(path, os.O_EXCL)
}

// openSecret creates path at [SecretFileMode] with the extra open flag (O_TRUNC
// or O_EXCL), then restricts the descriptor.
func openSecret(path string, flag int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|flag|syscall.O_NOFOLLOW, SecretFileMode.Perm())
	if err != nil {
		return nil, err
	}
	if err := restrictSecret(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// restrictSecret narrows an already-open file to [SecretFileMode].
//
// Chmod on the descriptor, not the path: the caller may hold a temporary file
// it is about to rename, and a path-based chmod would be a second lookup that
// a concurrent swap could redirect (the same reasoning
// internal/worker/staging applies to its stage-out source).
func restrictSecret(f *os.File) error {
	return f.Chmod(SecretFileMode.Perm())
}

// mkdirSecret creates dir at 0700.
func mkdirSecret(dir string) error {
	return os.Mkdir(dir, 0o700)
}

// isRestricted reports whether path carries no group or other permission bits.
//
// It does not require the mode to equal 0600 exactly: 0400 (a key an
// operator made read-only) and 0700 (a directory) are both restricted.
func isRestricted(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return info.Mode().Perm()&0o077 == 0, nil
}
