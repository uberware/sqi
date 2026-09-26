// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"errors"
	"fmt"
	"io/fs"
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
// service could not then create its log (plan clarification 4). Either way an
// existing dir that is, or sits under, a junction or symbolic link is refused
// (errReparsePoint): for an account because its ACE would land on the target,
// and for LocalSystem, which changes no ACL, because the service would write
// its log through it (see checkExistingDir).
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
	switch {
	case err != nil || created:
		return err
	case sid == nil:
		return checkExistingDir(dir)
	}
	return grantDir(dir, sid, fileModifyAccess)
}

// checkExistingDir makes the read-only checks grantDir makes before it
// changes an ACL — dir's parent and dir itself must be plain directories, not
// junctions or symbolic links — and changes nothing. A LocalSystem install
// needs no ACE, but registering a service that would write its log through a
// pre-positioned junction is still refused, as it is for --user. (The
// service's own runtime check, createLogDir, deliberately leaves an existing
// directory alone.)
func checkExistingDir(dir string) error {
	if err := checkParent(dir); err != nil {
		return err
	}
	h, err := openPlainDir(dir, windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return fmt.Errorf("cannot use log directory: %w", err)
	}
	windows.CloseHandle(h) //nolint:errcheck // only its attributes were needed
	return nil
}

// mkdirProtected creates dir, and any missing parents, and gives dir a
// protected DACL granting inheritable full control to SYSTEM, Administrators
// and account (when non-nil). Parents it creates inherit as usual. When dir
// already exists it changes nothing and reports created false. It refuses to
// create dir under a reparse-point parent (see checkParent).
func mkdirProtected(dir string, account *windows.SID) (created bool, err error) {
	info, err := os.Stat(dir)
	switch {
	case err == nil && info.IsDir():
		return false, nil
	case err == nil:
		return false, fmt.Errorf("%s exists and is not a directory", dir)
	case !errors.Is(err, fs.ErrNotExist):
		return false, fmt.Errorf("stat %s: %w", dir, err)
	}
	if err := checkParent(dir); err != nil {
		return false, err
	}
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
		err = protectNewDir(dir, trustees)
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

// protectNewDir gives the directory mkdirProtected just created a protected
// DACL of inheritable full control for sids.
func protectNewDir(dir string, sids []*windows.SID) error {
	h, err := openDirForACL(dir)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // best-effort close of a handle we are done with
	return writeDACL(h, dir, sids, fileAllAccess, nil, true)
}

// grantDir adds an inheritable ACE granting mask to sid onto dir's existing
// DACL, keeping the DACL's other entries and its protection state. It reads
// and writes through one handle to dir itself, so it refuses a junction or
// symbolic link rather than changing the ACL of what it points to.
func grantDir(dir string, sid *windows.SID, mask windows.ACCESS_MASK) error {
	if err := checkParent(dir); err != nil {
		return err
	}
	h, err := openDirForACL(dir)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // best-effort close of a handle we are done with
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
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
	return writeDACL(h, dir, []*windows.SID{sid}, mask, existing, control&windows.SE_DACL_PROTECTED != 0)
}

// errReparsePoint is returned when the log directory, or its parent, is a
// junction or symbolic link.
var errReparsePoint = errors.New("is a junction or symbolic link")

// checkParent refuses to go on when dir's parent exists and is not a plain
// directory. FILE_FLAG_OPEN_REPARSE_POINT protects only dir's final
// component, but the default log directory's parent, %ProgramData%\sqi, can
// be created as a junction by any local user without privilege (ProgramData
// grants Users create-folder and write-data), or an empty one converted into
// one; every path below it would then resolve into the junction's target —
// and every host has C:\Windows\Logs. A missing parent is fine: MkdirAll
// creates it. Residual TOCTOU, accepted: the parent is checked and then used
// by path, so one swapped for a junction during the install itself is not
// caught.
func checkParent(dir string) error {
	h, err := openPlainDir(filepath.Dir(dir), windows.FILE_READ_ATTRIBUTES)
	switch {
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
		return nil
	case err != nil:
		return fmt.Errorf("cannot secure log directory %s: parent %w", dir, err)
	}
	windows.CloseHandle(h) //nolint:errcheck // only its attributes were needed
	return nil
}

// openDirForACL opens dir itself for reading and writing its DACL, and fails
// unless dir is a plain directory.
//
// FILE_FLAG_OPEN_REPARSE_POINT opens a junction or symbolic link at dir as
// itself, never its target. Any local user can create %ProgramData%\sqi and
// plant `logs` as a junction (a junction needs no privilege), and a DACL
// write that followed it would give the service account Modify on the
// target. Whether path-based Get/SetNamedSecurityInfo follow a final
// junction is undocumented (Windows 11 build 26200 does not), so the ACL is
// read and written through this handle instead, and the attributes are read
// from the same handle so nothing can be swapped in between. Adapted from
// internal/worker/isolation's openForACL, which this leaf package may not
// import. FILE_FLAG_BACKUP_SEMANTICS is required to open a directory at all.
func openDirForACL(dir string) (windows.Handle, error) {
	h, err := openPlainDir(dir, windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return 0, fmt.Errorf("cannot secure log directory: %w", err)
	}
	return h, nil
}

// openPlainDir opens path itself (never a reparse point's target) with access,
// which must include FILE_READ_ATTRIBUTES, and fails unless it is a plain
// directory. The CreateFile error is wrapped, so a caller can test for a
// missing path.
func openPlainDir(path string, access uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("encode path %q: %w", path, err)
	}
	h, err := windows.CreateFile(p, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	if err := checkPlainDir(h, path); err != nil {
		windows.CloseHandle(h) //nolint:errcheck // returning the check's error
		return 0, err
	}
	return h, nil
}

func checkPlainDir(h windows.Handle, path string) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return fmt.Errorf("read attributes of %s: %w", path, err)
	}
	switch {
	case info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		return fmt.Errorf("%s %w; it must be a real directory", path, errReparsePoint)
	case info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0:
		return fmt.Errorf("%s is not a directory", path)
	}
	return nil
}

// writeDACL writes to h a DACL of inheritable grants of mask to sids, merged
// onto base when base is non-nil. It is the only place this package writes a
// DACL.
func writeDACL(h windows.Handle, dir string, sids []*windows.SID, mask windows.ACCESS_MASK, base *windows.ACL, protected bool) error {
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
	if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, info, nil, nil, acl, nil); err != nil {
		return fmt.Errorf("secure %s: %w", dir, err)
	}
	return nil
}
