// SPDX-License-Identifier: AGPL-3.0-or-later

package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// SecretFileMode is the POSIX mode every secret this package writes carries:
// readable and writable by the owner, nothing for group or other.
const SecretFileMode os.FileMode = 0o600

// WriteSecret writes data to path so that only the writing user — plus the
// platform's administrative principals — can read it back.
//
// # Why this exists rather than os.WriteFile(path, data, 0o600)
//
// sqi writes four kinds of private key material: the farm CA key and the
// server/client TLS keys (internal/certgen, behind `sqi-server tls init` and
// `tls issue`) and the NATS broker nkey seed (internal/brokerauth). Every one
// of them was protected by a POSIX mode alone.
//
// On Windows a POSIX mode is very nearly a no-op. os.Chmod maps only to the
// read-only ATTRIBUTE — it cannot deny read access to anyone — and the mode
// argument to os.MkdirAll is discarded outright. A file created with 0o600
// therefore lands with whatever DACL it inherits from its parent, which for a
// data directory under %ProgramData% or a user profile is routinely readable
// by every local account. The mode is not merely weaker there; it provides no
// confidentiality at all, while looking in source exactly like the POSIX call
// that does.
//
// Nothing caught this because nothing could: the repository's lint and test
// jobs ran only on ubuntu-latest, so the assertions that would have failed
// (`want 600`, in cmd/sqi-server, internal/certgen, internal/brokerauth and
// internal/worker/enroll) were never executed on a host where they were false.
//
// # What "restricted" means per platform
//
//   - POSIX: mode 0600. Unchanged from what these call sites already did.
//   - Windows: a PROTECTED DACL — inheritance stripped — granting full
//     control to exactly three trustees: the writing user, LocalSystem, and
//     BUILTIN\Administrators. That is the closest available analog of 0600.
//     SYSTEM and Administrators are included for the same reason
//     internal/worker/isolation includes them: a service must be able to read
//     what it wrote, and an operator must not be locked out of a key on their
//     own machine. It is not a weakening — on POSIX, root can read a 0600 file
//     too.
//
// The DACL is applied to the handle BEFORE any secret byte is written, so the
// file is never observable at an inherited, permissive ACL with key material
// already in it. That ordering is the whole point, and it is why this cannot
// be "os.WriteFile then fix up the ACL".
//
// An existing file at path is replaced. WriteSecret does not create parent
// directories — call [MkdirSecret] first when the directory may be absent, so
// the directory's own restriction is an explicit decision rather than a side
// effect.
func WriteSecret(path string, data []byte) error {
	f, err := createSecret(path)
	if err != nil {
		return fmt.Errorf("fsutil: create secret %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("fsutil: write secret %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("fsutil: sync secret %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("fsutil: close secret %s: %w", path, err)
	}
	return nil
}

// RestrictSecret restricts an ALREADY-OPEN file to the writing user, for
// callers that must create the descriptor themselves.
//
// internal/brokerauth's SaveSeed is the motivating case: it writes through a
// temporary file and renames it into place so a seed is never observed
// half-written, and it needs the restriction applied to that temporary
// descriptor rather than to the final name. Call this immediately after the
// create and before the first write, for the ordering reason in
// [WriteSecret]'s doc.
func RestrictSecret(f *os.File) error {
	if err := restrictSecret(f); err != nil {
		return fmt.Errorf("fsutil: restrict %s: %w", f.Name(), err)
	}
	return nil
}

// MkdirSecret creates dir and every missing parent, restricting dir itself to
// the creating user the same way [WriteSecret] restricts a file.
//
// Only the leaf is restricted. Intermediate parents are ordinary directories:
// a secret's confidentiality comes from its own DACL (or mode), never from an
// ancestor being unreadable, and tightening a shared parent an operator
// already created is the widening/narrowing anti-pattern this codebase
// avoids elsewhere. An already-existing dir is left as the operator
// configured it — os.MkdirAll does not revisit an existing directory's mode,
// and this does not revisit its ACL.
func MkdirSecret(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if parent := filepath.Dir(dir); parent != dir {
		if err := os.MkdirAll(parent, 0o750); err != nil {
			return fmt.Errorf("fsutil: create %s: %w", parent, err)
		}
	}
	if err := mkdirSecret(dir); err != nil {
		return fmt.Errorf("fsutil: create secret dir %s: %w", dir, err)
	}
	return nil
}

// IsRestricted reports whether path is readable only by its owner and the
// platform's administrative principals — the property [WriteSecret]
// establishes.
//
// Exported because it is the only way a test can assert that property
// portably: `Mode().Perm() == 0o600` is a true statement on POSIX and an
// unsatisfiable one on Windows, which is precisely how this gap survived. A
// caller may also use it as a runtime self-check on operator-provisioned key
// material sqi did not write itself.
func IsRestricted(path string) (bool, error) {
	ok, err := isRestricted(path)
	if err != nil {
		return false, fmt.Errorf("fsutil: check %s: %w", path, err)
	}
	return ok, nil
}
