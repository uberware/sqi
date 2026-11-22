// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"unsafe"

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
	// sqiDirAccountAccess is what a --user service needs on each shared
	// directory above its own log directory (%ProgramData%\sqi and
	// %ProgramData%\sqi\logs): to open it for its attributes (checkParent,
	// checkSqiAncestors) and pass through it. It cannot list, read or change
	// anything there.
	sqiDirAccountAccess = windows.ACCESS_MASK(windows.FILE_READ_ATTRIBUTES | windows.FILE_TRAVERSE | windows.SYNCHRONIZE)
)

// EnsureLogDir makes dir usable as the log directory of a service running as
// account ("" = LocalSystem).
//
// A missing dir is created with a protected DACL granting full control to
// SYSTEM, Administrators and account only — service logs can carry job and
// environment detail.
//
// An existing dir keeps its DACL and its protection state. When account is
// set, one inheritable Modify ACE for it is added: a LocalSystem service of
// the same name installed earlier may already have created dir without it,
// and this service could not then create its log. Either way an existing dir
// that is, or sits under, a junction or symbolic link is refused
// (errReparsePoint; see mkdirProtected).
//
// When account is set and dir is below %ProgramData%\sqi, the account also
// gets one non-inheritable ACE of read-attributes, traverse and synchronize
// (sqiDirAccountAccess) on each shared directory above dir (sqiAncestors),
// which are SYSTEM and Administrators only: the service opens them for their
// attributes at every start to refuse a junction, and would otherwise stop
// before it could log anything. Nothing it is given reaches another service's
// log directory.
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
	if err != nil || sid == nil {
		return err
	}
	if !created {
		if err := grantDir(dir, sid, fileModifyAccess); err != nil {
			return err
		}
	}
	for _, p := range sqiAncestors(dir) {
		if err := grantTraverse(p, sid); err != nil {
			return err
		}
	}
	return nil
}

// grantTraverse gives sid sqiDirAccountAccess on dir, through a handle to dir
// itself, unless sid holds an allow ACE covering it already. The ACE is not
// inheritable: nothing below dir is the account's to reach but its own log
// directory, which has its own protected DACL.
func grantTraverse(dir string, sid *windows.SID) error {
	return addGrant(dir, sid, sqiDirAccountAccess, windows.NO_INHERITANCE, true)
}

// objectACE is one ACE of a DACL that applies to the object itself.
type objectACE struct {
	typ  byte
	mask windows.ACCESS_MASK
	// sid is the trustee when typ is access-allowed, plain or callback (both
	// keep it where ACCESS_ALLOWED_ACE does); nil for any other type.
	sid *windows.SID
}

// objectACEs yields each ACE of dacl that applies to the object itself,
// skipping inherit-only ones, in order. A read error is yielded once, last.
func objectACEs(dacl *windows.ACL) iter.Seq2[objectACE, error] {
	return func(yield func(objectACE, error) bool) {
		for i := range uint32(dacl.AceCount) {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &ace); err != nil {
				yield(objectACE{}, fmt.Errorf("read ACE %d: %w", i, err))
				return
			}
			if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
				continue // applies only to children
			}
			e := objectACE{typ: ace.Header.AceType, mask: ace.Mask}
			if e.typ == windows.ACCESS_ALLOWED_ACE_TYPE || e.typ == accessAllowedCallbackACEType {
				e.sid = (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			}
			if !yield(e, nil) {
				return
			}
		}
	}
}

// allows reports whether dacl has an allow ACE for sid that applies to the
// object itself and grants every right in mask.
func allows(dacl *windows.ACL, sid *windows.SID, mask windows.ACCESS_MASK) (bool, error) {
	if dacl == nil {
		return false, nil
	}
	for e, err := range objectACEs(dacl) {
		if err != nil {
			return false, err
		}
		if e.typ == windows.ACCESS_ALLOWED_ACE_TYPE && e.mask&mask == mask && e.sid.Equals(sid) {
			return true, nil
		}
	}
	return false, nil
}

// checkExistingDir makes the read-only checks grantDir makes before it
// changes an ACL — dir's parent and dir itself must be plain directories, not
// junctions or symbolic links — and changes nothing.
func checkExistingDir(dir string) error {
	if err := checkParent(dir); err != nil {
		return err
	}
	if err := checkPlainDir(dir); err != nil {
		return fmt.Errorf("cannot use directory: %w", err)
	}
	return nil
}

// mkdirProtected creates dir, and any missing parents, and gives dir a
// protected DACL granting inheritable full control to SYSTEM, Administrators
// and account (when non-nil), in place from the moment dir exists, and owned
// by Administrators when this process is privileged (by its own account
// otherwise; see createProtected). Missing parents from %ProgramData%\sqi
// down are created protected too, and other parents it creates inherit as
// usual (see mkdirParent). When dir already exists it changes nothing and
// reports created false. Either way it refuses a dir that is, or sits under,
// a junction or symbolic link (see checkSqiAncestors, checkParent and
// checkExistingDir): the service would write, rename and delete its log files
// through it, as SYSTEM for a LocalSystem service. Install and a running
// service share this check, so a hand-registered service, which never ran
// `service install`, gets it too.
func mkdirProtected(dir string, account *windows.SID) (created bool, err error) {
	if err := checkSqiAncestors(dir); err != nil {
		return false, err
	}
	info, err := os.Stat(dir)
	switch {
	case err == nil && info.IsDir():
		return false, checkExistingDir(dir)
	case err == nil:
		return false, fmt.Errorf("%s exists and is not a directory", dir)
	case !errors.Is(err, fs.ErrNotExist):
		return false, fmt.Errorf("stat %s: %w", dir, err)
	}
	if err := checkParent(dir); err != nil {
		return false, err
	}
	if err := mkdirParent(dir); err != nil {
		return false, err
	}
	// A directory that fails to come out protected is removed again, so a
	// retry never finds it and keeps an inherited, unprotected DACL.
	created, err = createProtected(dir, account)
	if err != nil || created {
		return created, err
	}
	return false, checkExistingDir(dir)
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

// AdminsSID is BUILTIN\Administrators, the owner given to what is created
// administrators-only.
func AdminsSID() (*windows.SID, error) {
	sid, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, fmt.Errorf("well-known SID: %w", err)
	}
	return sid, nil
}

// grantDir adds an inheritable ACE granting mask to sid onto dir's existing
// DACL, keeping the DACL's other entries and its protection state. It reads
// and writes through one handle to dir itself, so it refuses a junction or
// symbolic link rather than changing the ACL of what it points to.
func grantDir(dir string, sid *windows.SID, mask windows.ACCESS_MASK) error {
	return addGrant(dir, sid, mask, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT, false)
}

// addGrant is grantDir with the ACE's inheritance given. With unlessAllowed it
// changes nothing when sid already holds an allow ACE on dir itself covering
// mask.
func addGrant(dir string, sid *windows.SID, mask windows.ACCESS_MASK, inheritance uint32, unlessAllowed bool) error {
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
	if unlessAllowed {
		if has, err := allows(existing, sid, mask); err != nil || has {
			return err
		}
	}
	return writeDACL(h, dir, []*windows.SID{sid}, mask, inheritance, existing, control&windows.SE_DACL_PROTECTED != 0)
}

// errReparsePoint is returned when a path this package must use as itself —
// the log directory or its parent, or a path CheckPaths judges — is a
// junction, symbolic link or other reparse point.
var errReparsePoint = errors.New("is a junction or symbolic link")

// checkParent refuses to go on when dir's parent exists and is not a plain
// directory. FILE_FLAG_OPEN_REPARSE_POINT protects only dir's final
// component, but a default log directory's parents, %ProgramData%\sqi and
// its logs, can be created as junctions by any local user without privilege
// (ProgramData grants Users create-folder and write-data), or empty ones
// converted into them; every path below would then resolve into the
// junction's target — and every host has C:\Windows\Logs (see also
// checkSqiAncestors). A missing parent is fine: MkdirAll
// creates it. Residual TOCTOU, accepted: the parent is checked and then used
// by path, so one swapped for a junction during the install itself is not
// caught.
func checkParent(dir string) error {
	switch err := checkPlainDir(filepath.Dir(dir)); {
	case isNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("cannot secure %s: parent %w", dir, err)
	}
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
		return 0, fmt.Errorf("cannot secure directory: %w", err)
	}
	return h, nil
}

// openPlainDir opens path itself (never a reparse point's target) with access,
// which must include FILE_READ_ATTRIBUTES, and fails unless it is a plain
// directory. The CreateFile error is wrapped, so a caller can test for a
// missing path.
func openPlainDir(path string, access uint32) (windows.Handle, error) {
	h, attrs, err := openPlain(path, access)
	switch {
	case errors.Is(err, errReparsePoint):
		return 0, fmt.Errorf("%w; it must be a real directory", err)
	case err != nil:
		return 0, err
	}
	if attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		windows.CloseHandle(h) //nolint:errcheck // returning the check's error
		return 0, fmt.Errorf("%s is not a directory", path)
	}
	return h, nil
}

// openPlain opens path itself (never a reparse point's target) with access,
// which must include FILE_READ_ATTRIBUTES, and fails with errReparsePoint
// when it is a junction, symbolic link or other reparse point. path may be a
// file or a directory; its attributes are returned, read from the same
// handle. The CreateFile error is wrapped, so a caller can test for a missing
// path.
func openPlain(path string, access uint32) (windows.Handle, uint32, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, fmt.Errorf("encode path %q: %w", path, err)
	}
	h, err := windows.CreateFile(p, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("open %s: %w", path, err)
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h) //nolint:errcheck // returning the read's error
		return 0, 0, fmt.Errorf("read attributes of %s: %w", path, err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(h) //nolint:errcheck // returning the check's error
		return 0, 0, fmt.Errorf("%s %w", path, errReparsePoint)
	}
	return h, info.FileAttributes, nil
}

// writeDACL writes to h a DACL of grants of mask to sids, with inheritance,
// merged onto base when base is non-nil. It is the only place this package
// changes an existing object's DACL (createSecuredDir gives a new one its
// DACL).
func writeDACL(h windows.Handle, dir string, sids []*windows.SID, mask windows.ACCESS_MASK, inheritance uint32,
	base *windows.ACL, protected bool,
) error {
	acl, err := buildDACL(sids, mask, inheritance, base)
	if err != nil {
		return err
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

// buildDACL is a DACL of grants of mask to sids, with inheritance, merged
// onto base when base is non-nil.
func buildDACL(sids []*windows.SID, mask windows.ACCESS_MASK, inheritance uint32, base *windows.ACL) (*windows.ACL, error) {
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(sids))
	for _, sid := range sids {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: mask,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, base)
	if err != nil {
		return nil, fmt.Errorf("build ACL: %w", err)
	}
	return acl, nil
}
