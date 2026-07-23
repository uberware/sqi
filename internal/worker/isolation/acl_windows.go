// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// explicitFullControl builds one inheritable full-control entry for sid.
func explicitFullControl(sid *windows.SID) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
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
// (writeSecured). SYSTEM and Administrators only, deliberately — no
// CREATOR OWNER, no entry for whoever happens to be running set-credential.
//
// This is what makes the documented "run set-credential from an elevated
// shell" requirement an ENFORCED control rather than a convention an
// operator could ignore: the only sanctioned caller already carries an
// elevated Administrator token, so SYSTEM+Administrators is sufficient for
// the real flow and grants nothing extra. Adding CREATOR OWNER — even as an
// inherit-only placeholder — would not stay a placeholder: NTFS
// canonicalizes an inheritable "this folder, subfolders, and files"
// CREATOR OWNER ACE applied to a container into a second, DIRECT ACE for
// the directory's actual current owner, i.e. whoever's Put call is running
// right now. That would let ANY account able to create the isolation
// directory — elevated or not — complete the entire provisioning flow,
// silently reopening the exact credential-disclosure finding this ACL
// exists to close. Machine-scope DPAPI is decryptable by anything on the
// host that can read the file, so this ACL, not the encryption, is the
// actual security boundary for the stored password.
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
// FILE_FLAG_OPEN_REPARSE_POINT is the whole point of this helper and is the
// Windows analog of workdir_unix.go's insistence on os.Lchown over
// os.Chown. Path-based SetNamedSecurityInfo follows junctions and symlinks,
// so a task that replaces an entry inside its own session directory with a
// junction to, say, C:\Windows could have sqi hand its own account full
// control of the target. On Windows this threat is WORSE than on POSIX:
// creating a directory junction requires no privilege at all, unlike a
// symlink (which needs SeCreateSymbolicLinkPrivilege or developer mode).
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
// whatever its parent grants — typically BUILTIN\Users — and the explicit
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
// It grants the target an ACE but deliberately does NOT make it the OWNER,
// which is where this departs from the POSIX behavior on purpose. A Windows
// owner implicitly holds WRITE_DAC, so transferring ownership would let a
// task rewrite its own session ACL and re-open the directory to other
// accounts — something POSIX's chown genuinely does allow (the owner can
// chmod 0777). Matching POSIX exactly would mean adopting its weaker position
// for no benefit, so this is intentionally tighter.
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
// occupies that name (CREATE_NEW — the Win32 equivalent of O_CREAT|O_EXCL),
// with extraAccess folded into the SAME CreateFile call as GENERIC_WRITE.
//
// This mirrors exactly what os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL,
// perm) does on Windows under the hood (see syscall.Open in the standard
// library's syscall_windows.go: GENERIC_WRITE access, FILE_SHARE_READ|
// FILE_SHARE_WRITE sharing, CREATE_NEW disposition, FILE_FLAG_OPEN_REPARSE_POINT
// so the create does not follow a reparse point already at path) — with one
// addition: extraAccess is requested up front, on the create call itself.
// Windows fixes a handle's access rights at the moment it is opened; there is
// no way to widen them on an already-open handle. A caller that will need
// WRITE_DAC on this handle later (WriteFileFchown, to apply an ACL before the
// handle is closed) has to ask for it here, or it can never get it at all —
// and asking via a SECOND, path-based open instead would reintroduce exactly
// the swap-the-entry race this create-then-act shape exists to close.
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
