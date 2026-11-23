// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// fileAllAccess is FILE_ALL_ACCESS (winnt.h): STANDARD_RIGHTS_REQUIRED |
// SYNCHRONIZE | 0x1FF, the specific-rights spelling of full control on a
// file or directory. golang.org/x/sys/windows exports no constant for it,
// hence the literal.
//
// It is used instead of windows.GENERIC_ALL. Windows canonicalizes an
// inheritable ACE that carries generic bits by splitting it into two: a
// non-inheritable effective ACE holding the mapped specific rights, plus an
// inherit-only ACE that keeps the generic bits for children to map. The three
// entries below would land on disk as six ACEs with the same trustees and
// effective access, and the "exactly these three trustees" invariant could
// not be checked by inspection. Granting the already-mapped mask gives one
// ACE per trustee, effective and inheritable at once.
const fileAllAccess = windows.ACCESS_MASK(0x001F01FF)

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

// privilegedTrustees returns the two entries every sqi-created ACL carries:
// SYSTEM, because the worker service must be able to read what it wrote and
// tear down what it created, and Administrators, so an operator is never
// locked out of a directory on their own machine.
func privilegedTrustees() ([]windows.EXPLICIT_ACCESS, error) {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, fmt.Errorf("well-known SID (SYSTEM): %w", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, fmt.Errorf("well-known SID (Administrators): %w", err)
	}
	return []windows.EXPLICIT_ACCESS{
		explicitFullControl(system),
		explicitFullControl(admins),
	}, nil
}

// adminOnlyDACL builds the DACL for everything the credential store creates
// under the isolation directory (credstore_windows.go): both the directory
// itself (secureDir) and the per-account secret file inside it
// (writeSecured). SYSTEM and Administrators only: no CREATOR OWNER, and no
// entry for whoever is running set-credential.
//
// This enforces the documented "run set-credential from an elevated shell"
// requirement: the only sanctioned caller carries an elevated Administrator
// token, so SYSTEM+Administrators is enough for it and grants nothing extra.
// A CREATOR OWNER entry, even inherit-only, does not stay a placeholder: NTFS
// canonicalizes an inheritable "this folder, subfolders, and files" CREATOR
// OWNER ACE applied to a container into a second, direct ACE for the
// directory's current owner, the account running Put. Any account able to
// create the isolation directory, elevated or not, could then complete the
// whole provisioning flow and read stored credentials. Machine-scope DPAPI is
// decryptable by anything on the host that can read the file, so this ACL,
// not the encryption, is the security boundary for the stored password.
func adminOnlyDACL() (*windows.ACL, error) {
	entries, err := privilegedTrustees()
	if err != nil {
		return nil, err
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return nil, fmt.Errorf("build admin-only DACL: %w", err)
	}
	return acl, nil
}

// openForACL opens path for a security-descriptor write WITHOUT following a
// reparse point.
//
// FILE_FLAG_OPEN_REPARSE_POINT is the Windows analog of workdir_unix.go's use
// of os.Lchown over os.Chown. Path-based SetNamedSecurityInfo follows
// junctions and symlinks, so a task that replaces an entry inside its own
// session directory with a junction to, say, C:\Windows could have sqi hand
// its own account full control of the target. Creating a directory junction
// needs no privilege, unlike a symlink (which needs
// SeCreateSymbolicLinkPrivilege or developer mode).
//
// FILE_FLAG_BACKUP_SEMANTICS is required to open a directory handle at all.
func openForACL(path string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("encode path %q: %w", path, err)
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
		return 0, fmt.Errorf("open %q for ACL write: %w", path, err)
	}
	return h, nil
}

// applyProtectedDACL replaces h's DACL with acl and marks it PROTECTED, which
// strips every inherited ACE. Without the protected flag the object would keep
// whatever its parent grants (typically BUILTIN\Users), and the explicit
// entries would be an addition rather than the whole story.
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

// lookupUserSID resolves a local account name to its SID. An empty system
// name asks Windows to search the local account database, matching how
// logonUserOS passes a nil domain to LogonUserW.
func lookupUserSID(user string) (*windows.SID, error) {
	sid, _, _, err := windows.LookupSID("", user)
	if err != nil {
		return nil, fmt.Errorf("look up SID for %q: %w", user, err)
	}
	return sid, nil
}

// secureDACL builds the DACL for an isolated session working directory: full
// control for the target account, SYSTEM, and Administrators, and nothing
// else. This is the Windows counterpart of the POSIX chown + chmod 0700 in
// workdir_unix.go.
//
// It grants the target an ACE but does not make it the owner, which is
// stricter than POSIX. A Windows owner implicitly holds WRITE_DAC, so
// transferring ownership would let a task rewrite its own session ACL and
// reopen the directory to other accounts, as a POSIX owner can with
// chmod 0777.
func secureDACL(target *windows.SID) (*windows.ACL, error) {
	entries, err := privilegedTrustees()
	if err != nil {
		return nil, err
	}
	entries = append(entries, explicitFullControl(target))
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return nil, fmt.Errorf("build session DACL: %w", err)
	}
	return acl, nil
}

// createExclusiveFile creates path for writing, failing if anything already
// occupies that name (CREATE_NEW, the Win32 equivalent of O_CREAT|O_EXCL),
// with extraAccess requested in the same CreateFile call as GENERIC_WRITE.
//
// This matches what os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL,
// perm) does on Windows (see syscall.Open in the standard library's
// syscall_windows.go: GENERIC_WRITE access, FILE_SHARE_READ|FILE_SHARE_WRITE
// sharing, CREATE_NEW disposition, FILE_FLAG_OPEN_REPARSE_POINT so the create
// does not follow a reparse point already at path), plus extraAccess.
// Windows fixes a handle's access rights when it is opened and cannot widen
// them afterward, so a caller that needs WRITE_DAC on this handle later
// (WriteFileFchown, to apply an ACL before the handle is closed) must ask
// for it here. A second, path-based open would reopen the race in which the
// entry is swapped between create and use.
func createExclusiveFile(path string, perm os.FileMode, extraAccess uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("encode path %q: %w", path, err)
	}
	attrs := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if perm&0o200 == 0 { // no owner-write bit: mirror os.OpenFile's syscallMode->S_IWRITE check
		attrs = windows.FILE_ATTRIBUTE_READONLY
	}
	attrs |= windows.FILE_FLAG_OPEN_REPARSE_POINT
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_WRITE|extraAccess,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.CREATE_NEW,
		attrs,
		0,
	)
	if err != nil {
		return 0, err
	}
	return h, nil
}

// securePath applies acl to a single path, opening it reparse-point-safely.
func securePath(path string, acl *windows.ACL) error {
	h, err := openForACL(path)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // best-effort close of a handle we are done with
	return applyProtectedDACL(h, acl)
}
