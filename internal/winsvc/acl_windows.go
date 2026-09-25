// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/windows"
)

const (
	// fileAllAccess is FILE_ALL_ACCESS (winnt.h), full control spelled as
	// specific rights; x/sys exports no constant for it. Deliberately not
	// windows.GENERIC_ALL: Windows splits an inheritable ACE carrying generic
	// bits into an effective ACE plus an inherit-only one, so the DACL on disk
	// would not be the one written (internal/worker/isolation and
	// internal/fsutil reach the same conclusion).
	fileAllAccess = windows.ACCESS_MASK(0x001F01FF)
	// fileModifyAccess is Explorer's "Modify": read, write, execute and
	// delete, but not change permissions or take ownership.
	fileModifyAccess = windows.ACCESS_MASK(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE |
		windows.FILE_GENERIC_EXECUTE | windows.DELETE)
)

// EnsureLogDir makes dir usable as the log directory of a service running as
// account ("" = LocalSystem).
//
// A missing dir is created with a protected DACL granting full control to
// SYSTEM, Administrators and account only — service logs can carry job and
// environment detail (spec §2).
//
// An existing dir keeps its DACL and its protection state. When account is
// set, one inheritable Modify ACE for it is added: a LocalSystem service
// installed earlier may already have created dir without it, and this
// service could not then create its log (plan clarification 4).
func EnsureLogDir(dir, account string) error {
	var sid *windows.SID
	if account != "" {
		s, _, _, err := windows.LookupSID("", lookupName(account))
		if err != nil {
			return fmt.Errorf("look up account %s: %w", account, err)
		}
		sid = s
	}
	created, err := mkdirProtected(dir, sid)
	if err != nil || created || sid == nil {
		return err
	}
	return grantDir(dir, sid, fileModifyAccess)
}

// mkdirProtected creates dir, and any missing parents, and gives dir a
// protected DACL granting inheritable full control to SYSTEM, Administrators
// and account (when non-nil). Parents it creates inherit as usual. When dir
// already exists it changes nothing and reports created false.
func mkdirProtected(dir string, account *windows.SID) (created bool, err error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return false, fmt.Errorf("create %s: %w", filepath.Dir(dir), err)
	}
	if err := os.Mkdir(dir, 0o750); err != nil {
		if info, statErr := os.Stat(dir); statErr == nil && info.IsDir() {
			return false, nil
		}
		return false, fmt.Errorf("create %s: %w", dir, err)
	}
	trustees, err := protectedTrustees(account)
	if err == nil {
		err = setDirDACL(dir, trustees, fileAllAccess, nil, true)
	}
	if err != nil {
		// Leave nothing behind: a retry would otherwise find the directory
		// and keep its inherited, unprotected DACL.
		os.Remove(dir)
		return false, err
	}
	return true, nil
}

// protectedTrustees is SYSTEM, Administrators and, unless it is one of those,
// account.
func protectedTrustees(account *windows.SID) ([]*windows.SID, error) {
	sids := make([]*windows.SID, 0, 3)
	for _, wk := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(wk)
		if err != nil {
			return nil, fmt.Errorf("well-known SID: %w", err)
		}
		sids = append(sids, sid)
	}
	if account != nil && !slices.ContainsFunc(sids, account.Equals) {
		sids = append(sids, account)
	}
	return sids, nil
}

// grantDir adds an inheritable ACE granting mask to sid onto dir's existing
// DACL, keeping the DACL's other entries and its protection state.
func grantDir(dir string, sid *windows.SID, mask windows.ACCESS_MASK) error {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read ACL of %s: %w", dir, err)
	}
	existing, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read ACL of %s: %w", dir, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		return fmt.Errorf("read ACL control of %s: %w", dir, err)
	}
	return setDirDACL(dir, []*windows.SID{sid}, mask, existing, control&windows.SE_DACL_PROTECTED != 0)
}

// setDirDACL writes a DACL of inheritable grants of mask to sids, merged onto
// base when base is non-nil.
func setDirDACL(dir string, sids []*windows.SID, mask windows.ACCESS_MASK, base *windows.ACL, protected bool) error {
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(sids))
	for _, sid := range sids {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: mask,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, base)
	if err != nil {
		return fmt.Errorf("build ACL: %w", err)
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if protected {
		info |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		info |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, info, nil, nil, acl, nil); err != nil {
		return fmt.Errorf("secure %s: %w", dir, err)
	}
	return nil
}
