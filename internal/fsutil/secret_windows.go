// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package fsutil

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileAllAccess is FILE_ALL_ACCESS (winnt.h): STANDARD_RIGHTS_REQUIRED |
// SYNCHRONIZE | 0x1FF — the specific-rights spelling of full control.
//
// Deliberately NOT windows.GENERIC_ALL. Windows canonicalizes an INHERITABLE
// ACE carrying generic bits by splitting it in two — a non-inheritable
// effective ACE with the mapped specific rights, plus an inherit-only ACE
// keeping the generic bits — so three entries would land on disk as six, with
// the same effective access but an ACL no reader would recognize as the
// three-trustee one this file builds. internal/worker/isolation's
// acl_windows.go reaches the same conclusion for the same reason; the two
// stay separate because they grant to DIFFERENT trustees (that package is
// deliberately admin-only; this one must include the writing user), not
// because either is unaware of the other.
const fileAllAccess = windows.ACCESS_MASK(0x001F01FF)

// secretDACL builds the protected DACL described in [WriteSecret]'s doc:
// full control for the current process user, LocalSystem and
// BUILTIN\Administrators, and nobody else.
//
// The current user is included — unlike internal/worker/isolation's
// adminOnlyDACL — because this protects a key the CALLER writes and later
// reads back as itself: `sqi-server tls init` writes ca.key and the running
// server reads it. Omitting the writer would lock a non-administrator service
// account out of its own key, which POSIX 0600 does not do.
func secretDACL() (*windows.ACL, error) {
	entries := make([]windows.EXPLICIT_ACCESS, 0, 3)

	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("current token user: %w", err)
	}
	entries = append(entries, explicitFullControl(user.User.Sid))

	for _, wk := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinLocalSystemSid,
		windows.WinBuiltinAdministratorsSid,
	} {
		sid, err := windows.CreateWellKnownSid(wk)
		if err != nil {
			return nil, fmt.Errorf("well-known SID: %w", err)
		}
		entries = append(entries, explicitFullControl(sid))
	}

	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return nil, fmt.Errorf("build secret DACL: %w", err)
	}
	return acl, nil
}

// explicitFullControl builds one inheritable full-control entry for sid.
func explicitFullControl(sid *windows.SID) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: fileAllAccess,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}

// applyProtectedDACL replaces h's DACL with acl and marks it PROTECTED, which
// strips every inherited ACE. Without the protected flag the object keeps
// whatever its parent grants — typically BUILTIN\Users — and these entries
// would be an addition rather than the whole story, leaving the key readable
// by exactly the accounts this is meant to exclude.
func applyProtectedDACL(h windows.Handle, acl *windows.ACL) error {
	err := windows.SetSecurityInfo(
		h,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	)
	if err != nil {
		return fmt.Errorf("set protected DACL: %w", err)
	}
	return nil
}

// createSecret creates path with the restricted DACL already applied.
//
// WRITE_DAC and READ_CONTROL are requested on the CREATE call, not added
// later: Windows fixes a handle's access rights at open time and nothing done
// to an open handle can widen them, so a handle opened with GENERIC_WRITE
// alone could never apply a DACL afterwards. Asking via a second, path-based
// open instead would reintroduce a swap window — the same reasoning
// internal/worker/isolation's createExclusiveFile records.
//
// FILE_FLAG_OPEN_REPARSE_POINT means an entry already at path is opened
// itself rather than followed, so a planted symlink or junction cannot
// redirect the key elsewhere. CREATE_ALWAYS (not CREATE_NEW) because
// WriteSecret replaces an existing file; combined with the reparse flag, an
// attacker-planted link is truncated in place rather than written through.
func createSecret(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("encode path: %w", err)
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_WRITE|windows.WRITE_DAC|windows.READ_CONTROL,
		0, // no sharing: nothing else reads a key while it is being written
		nil,
		windows.CREATE_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), path)
	// Apply BEFORE returning, so the caller's first write lands in a file that
	// is already restricted rather than one still carrying inherited access.
	if err := restrictSecret(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// restrictSecret applies the restricted DACL to an already-open file.
//
// The handle must have been opened with WRITE_DAC — see createSecret. A
// caller that created its own descriptor with os.CreateTemp gets only
// GENERIC_READ|GENERIC_WRITE and cannot use this; use createSecret, or accept
// that the ACL must be set by path. internal/brokerauth takes the former
// route.
func restrictSecret(f *os.File) error {
	acl, err := secretDACL()
	if err != nil {
		return err
	}
	return applyProtectedDACL(windows.Handle(f.Fd()), acl)
}

// mkdirSecret creates dir and applies the restricted DACL to it.
//
// The ACL is applied after creation rather than through a SECURITY_ATTRIBUTES
// on CreateDirectory. The window is real but empty: a directory is created
// with no contents, so there is nothing inside it to disclose until the
// caller writes — and every write this package performs carries its own
// per-file DACL regardless of what the directory grants.
func mkdirSecret(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	acl, err := secretDACL()
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return fmt.Errorf("encode path: %w", err)
	}
	h, err := windows.CreateFile(
		p,
		windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return fmt.Errorf("open %s for ACL write: %w", dir, err)
	}
	defer windows.CloseHandle(h) //nolint:errcheck // best-effort close of a handle we are done with
	return applyProtectedDACL(h, acl)
}

// isRestricted reports whether path's DACL grants access to nobody beyond the
// owner and the administrative principals.
//
// It inspects TRUSTEES rather than computing effective access: the question a
// caller asks is "can an ordinary local account read this key", and a granting
// ACE for any SID outside the sanctioned set is what would make that true. A
// DACL that is not PROTECTED fails outright — inherited ACEs are exactly the
// permissive grants this exists to exclude, and an unprotected DACL means
// whatever the parent grants applies whether or not it appears here.
func isRestricted(path string) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		return false, err
	}
	control, _, err := sd.Control()
	if err != nil {
		return false, err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return false, nil
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return false, err
	}
	if dacl == nil {
		// A nil DACL grants everyone full control; an empty one grants
		// nobody. Only the former is a failure, and they are distinct.
		return false, nil
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return false, err
	}
	allowed, err := sanctionedSIDs(owner)
	if err != nil {
		return false, err
	}
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue // a DENY entry never widens access
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !allowed[sid.String()] {
			return false, nil
		}
	}
	return true, nil
}

// sanctionedSIDs is the set of trustees a restricted object may grant to: its
// own owner, LocalSystem and BUILTIN\Administrators. The owner is included
// rather than the CURRENT user so the check still answers correctly when it
// runs as someone else — an administrator auditing a service account's key.
func sanctionedSIDs(owner *windows.SID) (map[string]bool, error) {
	allowed := map[string]bool{}
	if owner != nil {
		allowed[owner.String()] = true
	}
	for _, wk := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinLocalSystemSid,
		windows.WinBuiltinAdministratorsSid,
	} {
		sid, err := windows.CreateWellKnownSid(wk)
		if err != nil {
			return nil, fmt.Errorf("well-known SID: %w", err)
		}
		allowed[sid.String()] = true
	}
	return allowed, nil
}
