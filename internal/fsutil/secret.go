// SPDX-License-Identifier: AGPL-3.0-or-later

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SecretFileMode is the POSIX mode every secret this package writes carries:
// readable and writable by the owner, nothing for group or other.
const SecretFileMode os.FileMode = 0o600

// WriteSecret writes data to path so that only the writing user, plus the
// platform's administrative principals, can read it back.
//
// # Why not os.WriteFile(path, data, 0o600)
//
// sqi writes its private key material through this: the farm CA key and the
// server/client TLS keys (internal/certgen, behind `sqi-server tls init` and
// `tls issue`) and the NATS broker nkey seed (internal/brokerauth).
//
// On Windows a POSIX mode is close to a no-op. os.Chmod maps only to the
// read-only attribute (it cannot deny read access to anyone) and os.MkdirAll
// ignores its mode argument. A file created with 0o600 therefore lands with
// whatever DACL it inherits from its parent, which for a data directory under
// %ProgramData% or a user profile is often readable by every local account.
//
// # What "restricted" means per platform
//
//   - POSIX: mode 0600.
//   - Windows: a protected DACL (inheritance stripped) granting full control
//     to exactly three trustees: the writing user, LocalSystem, and
//     BUILTIN\Administrators, the closest available analog of 0600. SYSTEM
//     and Administrators are included for the same reason
//     internal/worker/isolation includes them: a service must be able to read
//     what it wrote, and an operator must not be locked out of a key on their
//     own machine. On POSIX, root can read a 0600 file too.
//
// The DACL is applied to the handle before any secret byte is written, so the
// file is never observable at an inherited, permissive ACL with key material
// already in it. That ordering is why this cannot be "os.WriteFile then fix up
// the ACL".
//
// An existing file at path is replaced. WriteSecret does not create parent
// directories: call [MkdirSecret] first when the directory may be absent, so
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

// CreateSecretExclusive creates an empty file at path, restricted to the
// writing user as [WriteSecret] restricts one, for a caller whose content is
// then written by something else into the file it names (a database backup
// written by SQLite's VACUUM INTO or by pg_dump). The restriction is in place
// before the first byte lands. It fails with an error matching [fs.ErrExist]
// when path already exists, so it never replaces, or widens, a file.
func CreateSecretExclusive(path string) error {
	f, err := createSecretExclusive(path)
	if err != nil {
		return fmt.Errorf("fsutil: create secret %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return errors.Join(fmt.Errorf("fsutil: close secret %s: %w", path, err), os.Remove(path))
	}
	return nil
}

// RestrictSecret restricts an already-open file to the writing user, for
// callers that must create the descriptor themselves. Call it immediately
// after the create and before the first write, for the ordering reason in
// [WriteSecret]'s doc. On Windows the handle must carry WRITE_DAC (see
// restrictSecret).
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
// configured it: os.MkdirAll does not revisit an existing directory's mode,
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
// platform's administrative principals, the property [WriteSecret]
// establishes.
//
// It is exported because it is the only portable way for a test to assert
// that property: `Mode().Perm() == 0o600` says nothing about a file's
// protection on Windows. A caller may also use it as a runtime self-check on
// operator-provisioned key material sqi did not write itself.
func IsRestricted(path string) (bool, error) {
	ok, err := isRestricted(path)
	if err != nil {
		return false, fmt.Errorf("fsutil: check %s: %w", path, err)
	}
	return ok, nil
}
