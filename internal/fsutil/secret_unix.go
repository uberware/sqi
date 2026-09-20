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
// NEWLY CREATED file never exists at a wider mode with secret bytes already in
// it. umask can only ever REMOVE bits from an open mode, never add them, so a
// permissive umask cannot widen this; a restrictive one that strips owner bits
// would make the caller's own subsequent write fail loudly rather than
// silently exposing anything.
//
// The restrictSecret call is NOT redundant with that mode, and leaving it out
// was a real defect: open(2) applies its mode argument only when it CREATES
// the file, so REPLACING an existing permissive key left the old mode in place
// and wrote the new private key into it. `sqi-server tls issue` re-issues a
// leaf over a previous one every time it runs, which is exactly that path.
// Windows never had the bug, because its createSecret re-applies the DACL
// unconditionally -- so the two platforms disagreed about what this function
// promises, and the POSIX half quietly did not keep the guarantee
// [WriteSecret]'s doc states.
//
// The ordering guarantee survives: O_TRUNC empties the file as part of the
// open, so at the moment of the fchmod a pre-existing permissive file holds no
// bytes, and the caller's first write lands in a file already narrowed.
func createSecret(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, SecretFileMode.Perm())
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
// Chmod on the DESCRIPTOR, not the path: the caller may hold a temporary file
// it is about to rename, and a path-based chmod would be a second lookup that
// a concurrent swap could redirect — the same reasoning
// internal/worker/staging applies to its stage-out source.
func restrictSecret(f *os.File) error {
	return f.Chmod(SecretFileMode.Perm())
}

// mkdirSecret creates dir at 0700.
func mkdirSecret(dir string) error {
	return os.Mkdir(dir, 0o700)
}

// isRestricted reports whether path carries no group or other permission bits.
//
// It deliberately does NOT require the mode to equal 0600 exactly: 0400 (a key
// an operator made read-only) and 0700 (a directory) are both restricted, and
// an assertion that insisted on 0600 would fail on them for no security
// reason.
func isRestricted(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return info.Mode().Perm()&0o077 == 0, nil
}
