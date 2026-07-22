// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// SecureWorkDir is unreachable in practice: the Windows provider's Resolve
// never returns a non-nil Credential (see provider_windows.go), so callers
// only ever invoke this with a nil cred, which is a no-op. It exists so the
// package compiles on Windows ahead of the real LogonUser-based provider.
func SecureWorkDir(_ string, cred *Credential) error {
	if cred == nil {
		return nil
	}
	return fmt.Errorf("%w: windows run-as-user is not yet implemented", ErrNotCapable)
}

// ChownRecursive is unreachable in practice for the same reason as
// SecureWorkDir above.
func ChownRecursive(_ string, cred *Credential) error {
	if cred == nil {
		return nil
	}
	return fmt.Errorf("%w: windows run-as-user is not yet implemented", ErrNotCapable)
}

// ValidateTraversable is a no-op on Windows: NTFS access control is
// ACL-based, not POSIX permission-bit-based, and the Windows provider's
// Capable() always fails (see provider_windows.go) — no worker on this
// platform can ever be "capable" of isolation, so there is nothing to
// validate at boot, and no assignment can ever carry a resolved credential
// per-assignment either. It exists so the package compiles on Windows ahead
// of the real LogonUser-based provider.
func ValidateTraversable(_ ...string) error {
	return nil
}

// WriteFileFchown (re)writes path with data via remove-then-O_EXCL-create
// (refusing to write THROUGH a pre-existing entry — see the POSIX
// implementation's doc for the full threat model and why a plain O_EXCL-only
// create, this function's own previous shape, incorrectly also refused a
// legitimate duplicate embedded-file name instead of only an attacker-planted
// entry). cred is accepted for cross-platform call-site parity with the
// POSIX implementation but is unused: the Windows Credential is always nil in
// practice (Resolve never returns a non-nil one; see provider_windows.go), so
// there is never anything to chown.
//
// O_NOFOLLOW has no Windows equivalent flag: NTFS symlinks/junctions require
// an explicit reparse-point create call rather than being creatable via a
// plain "create file" open the way POSIX symlinks are, so O_EXCL alone
// already refuses to write through a pre-existing entry of any kind here.
func WriteFileFchown(path string, data []byte, perm os.FileMode, _ *Credential) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove existing %q: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("create %q: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	return nil
}
