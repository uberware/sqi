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
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// readExecuteAccess is Explorer's "Read & execute".
const readExecuteAccess = windows.ACCESS_MASK(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE)

// configDirKey names the configuration directory in a refusal, as
// `service install` names it.
const configDirKey = "config directory"

// isPrivileged reports whether this process is elevated or LocalSystem, and so
// may make BUILTIN\Administrators the owner of what it creates. A variable so
// a test can take the unprivileged branch from an elevated process.
var isPrivileged = func() bool {
	token := windows.GetCurrentProcessToken()
	if token.IsElevated() {
		return true
	}
	tu, err := token.GetTokenUser()
	return err == nil && tu.User.Sid.IsWellKnown(windows.WinLocalSystemSid)
}

// PrepareConfigDir makes dir the directory config init writes into. Missing: created owned by
// BUILTIN\Administrators with a protected DACL of inheritable full control for SYSTEM and
// Administrators, plus inheritable read-and-execute for account when account != "". Existing:
// must pass CheckPaths (account trusted), then gets the account's read-and-execute ACE when
// account != "". Never edits the ACL of a directory that failed. Caller is elevated.
//
// Missing parents are created the same way, without the account's ACE. A
// directory it created whose ancestors then fail the check is removed again,
// and the check's *UntrustedPathsError returned.
func PrepareConfigDir(dir, account string) error {
	reader, err := resolveUser(account)
	if err != nil {
		return err
	}
	trusted, err := trustedSet(reader)
	if err != nil {
		return err
	}
	return prepareConfigDir(dir, reader, trusted)
}

// prepareConfigDir is PrepareConfigDir with the account resolved (reader nil
// for none) and the trusted set given.
func prepareConfigDir(dir string, reader *windows.SID, trusted []*windows.SID) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("configuration directory %q is not an absolute path", dir)
	}
	dir = filepath.Clean(dir)
	created, err := createAdminOnly(dir, reader)
	if err != nil {
		return err
	}
	if err := checkPaths([]PathRef{{Key: configDirKey, Path: dir}}, trusted); err != nil {
		removeDirs(created)
		return err
	}
	if slices.Contains(created, dir) {
		return nil
	}
	// Existing, and nobody untrusted can change it: the check refused a
	// junction or symbolic link, but not a file.
	h, err := openPlainDir(dir, windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return fmt.Errorf("cannot use configuration directory: %w", err)
	}
	windows.CloseHandle(h) //nolint:errcheck // only its attributes were needed
	if reader == nil {
		return nil
	}
	return grantDir(dir, reader, readExecuteAccess)
}

// EnsureSqiDir creates %ProgramData%\sqi when it is missing, as
// PrepareConfigDir creates a directory: owned by BUILTIN\Administrators with
// a protected DACL of inheritable full control for SYSTEM and Administrators.
// An existing one is left as it is, for CheckPaths to judge. `service
// install` calls it before its check: the default log directory lives there,
// and a missing one would be judged by %ProgramData% itself, which every
// user can write. Caller is elevated.
func EnsureSqiDir() error {
	_, err := createAdminOnly(filepath.Join(ProgramDataDir(), "sqi"), nil)
	return err
}

// PrepareDataDir creates dir if missing (inheriting its parent's DACL) and grants account an
// inheritable Modify ACE (fileModifyAccess).
//
// A created dir is owned by BUILTIN\Administrators, as PrepareConfigDir's
// are, so the check `service install` makes passes it; the caller is
// elevated. dir's parent must exist. Either way a dir that is, or sits under,
// a junction or symbolic link is refused.
func PrepareDataDir(dir, account string) error {
	sid, err := resolveUser(account)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("data directory %q is not an absolute path", dir)
	}
	if err := checkParent(dir); err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return fmt.Errorf("well-known SID: %w", err)
	}
	created, err := createSecuredDir(dir, admins, nil)
	switch {
	case err != nil:
		return err
	case sid != nil:
		return grantDir(dir, sid, fileModifyAccess)
	case !created:
		return checkExistingDir(dir)
	}
	return nil
}

// createAdminOnly creates dir and each of its missing parents, outermost
// first, owned by Administrators with a protected DACL of inheritable full
// control for SYSTEM and Administrators, dir's also granting reader (when
// non-nil) inheritable read-and-execute. It returns those it created; one
// that appeared meanwhile is left as it is, for the caller's check. On an
// error it removes what it created.
func createAdminOnly(dir string, reader *windows.SID) ([]string, error) {
	missing, err := missingDirs(dir)
	if err != nil || len(missing) == 0 {
		return nil, err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, fmt.Errorf("well-known SID: %w", err)
	}
	trustees, err := protectedTrustees(nil)
	if err != nil {
		return nil, err
	}
	parentDACL, err := buildDACL(trustees, fileAllAccess, nil)
	if err != nil {
		return nil, err
	}
	leafDACL := parentDACL
	if reader != nil {
		if leafDACL, err = buildDACL([]*windows.SID{reader}, readExecuteAccess, parentDACL); err != nil {
			return nil, err
		}
	}
	var created []string
	for _, p := range missing {
		dacl := parentDACL
		if p == dir {
			dacl = leafDACL
		}
		ok, err := createSecuredDir(p, admins, dacl)
		if err != nil {
			removeDirs(created)
			return nil, err
		}
		if ok {
			created = append(created, p)
		}
	}
	return created, nil
}

// missingDirs is dir and each of its ancestors that does not exist, outermost
// first; empty when dir exists.
func missingDirs(dir string) ([]string, error) {
	var missing []string
	for p := dir; ; {
		_, err := os.Lstat(p)
		switch {
		case err == nil:
			slices.Reverse(missing)
			return missing, nil
		case !errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("stat %s: %w", p, err)
		}
		missing = append(missing, p)
		parent := filepath.Dir(p)
		if parent == p {
			return nil, fmt.Errorf("no part of %s exists", dir)
		}
		p = parent
	}
}

// removeDirs removes the directories createAdminOnly created, innermost
// first. They are empty: nobody else could create anything in them.
func removeDirs(dirs []string) {
	for _, d := range slices.Backward(dirs) {
		os.Remove(d) // best-effort cleanup; the caller returns the error that caused it
	}
}

// mkdirParent creates dir's missing parents for mkdirProtected.
//
// %ProgramData%\sqi, the default log directory's parent, is created protected
// rather than inheriting ProgramData's ACL, which lets every local user create
// and write files in it. A privileged process (`service install`, a
// LocalSystem service's first start) gives it SYSTEM and Administrators only,
// owned by Administrators, as PrepareConfigDir does. An unprivileged one (a
// hand-registered service running as an ordinary account, on its first start)
// cannot make Administrators the owner, and could not create logs in a
// directory only they may change, so it owns the directory and adds its own
// account: the service account controlling its own directory is not an
// escalation. An existing %ProgramData%\sqi is left as it is. Every other
// parent is created by os.MkdirAll and inherits as usual.
func mkdirParent(dir string) error {
	parent := filepath.Dir(dir)
	if !strings.EqualFold(parent, filepath.Join(ProgramDataDir(), "sqi")) {
		return mkdirAll(parent)
	}
	switch _, err := os.Lstat(parent); {
	case err == nil:
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("stat %s: %w", parent, err)
	}
	if err := mkdirAll(filepath.Dir(parent)); err != nil {
		return err
	}
	var self *windows.SID
	if !isPrivileged() {
		me, err := processUser()
		if err != nil {
			return err
		}
		self = me
	}
	created, err := createProtected(parent, self)
	if err != nil || created {
		return err
	}
	// Someone else created it meanwhile: refuse it if it is a junction.
	return checkParent(dir)
}

func mkdirAll(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return nil
}

// createProtected creates path, whose parent must exist, with a protected
// DACL of inheritable full control for SYSTEM, Administrators and account
// (when non-nil), owned by Administrators when this process is privileged and
// by the token's default owner, its own account, otherwise. It reports false,
// changing nothing, when path already exists.
func createProtected(path string, account *windows.SID) (bool, error) {
	var owner *windows.SID // nil: the token's default
	if isPrivileged() {
		admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
		if err != nil {
			return false, fmt.Errorf("well-known SID: %w", err)
		}
		owner = admins
	}
	trustees, err := protectedTrustees(account)
	if err != nil {
		return false, err
	}
	dacl, err := buildDACL(trustees, fileAllAccess, nil)
	if err != nil {
		return false, err
	}
	return createSecuredDir(path, owner, dacl)
}

func processUser() (*windows.SID, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read process account: %w", err)
	}
	return tu.User.Sid, nil
}

// createSecuredDir creates path, whose parent must exist, owned by owner
// (nil: the token's default owner) and, when dacl is non-nil, with dacl as its
// protected DACL (nil inherits the parent's). Both are in the security
// descriptor CreateDirectory is given, so they are in place from the moment
// path exists: nothing can be created in it under an inherited DACL first.
// Callers that need Administrators as the owner pass it, since the default
// depends on the creating token (an elevated process started from Git Bash
// owns what it creates; one started from PowerShell gives it to
// Administrators). A restricted token may refuse even its own account as an
// explicit owner, so the unprivileged callers leave it to the default.
//
// It reports false, changing nothing, when path already exists. It then reads
// the security back through a handle to path itself and removes path, failing,
// unless it is what was asked for — a file system without ACLs ignores the
// descriptor.
func createSecuredDir(path string, owner *windows.SID, dacl *windows.ACL) (bool, error) {
	sd, err := newSecurityDescriptor(owner, dacl)
	if err != nil {
		return false, fmt.Errorf("secure %s: %w", path, err)
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, fmt.Errorf("encode path %q: %w", path, err)
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	if err := windows.CreateDirectory(p, &sa); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return false, nil
		}
		return false, fmt.Errorf("create %s: %w", path, err)
	}
	if err := verifySecurity(path, owner, dacl != nil); err != nil {
		os.Remove(path) // leave nothing behind; returning the reason
		return false, err
	}
	return true, nil
}

// newSecurityDescriptor is an absolute security descriptor carrying owner
// and dacl, as a protected DACL, each when non-nil.
func newSecurityDescriptor(owner *windows.SID, dacl *windows.ACL) (*windows.SECURITY_DESCRIPTOR, error) {
	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	if owner != nil {
		if err := sd.SetOwner(owner, false); err != nil {
			return nil, err
		}
	}
	if dacl == nil {
		return sd, nil
	}
	if err := sd.SetDACL(dacl, true, false); err != nil {
		return nil, err
	}
	if err := sd.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return nil, err
	}
	return sd, nil
}

// verifySecurity fails unless path has an owner, which is owner when owner is
// non-nil, and, when protected, a protected DACL. It opens path itself for
// READ_CONTROL only, which its owner holds whatever its DACL grants.
func verifySecurity(path string, owner *windows.SID, protected bool) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("encode path %q: %w", path, err)
	}
	h, err := windows.CreateFile(p, windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer windows.CloseHandle(h) //nolint:errcheck // best-effort close of a handle we are done with
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read security of %s: %w", path, err)
	}
	got, _, err := sd.Owner()
	switch {
	case err != nil:
		return fmt.Errorf("read owner of %s: %w", path, err)
	case got == nil:
		return fmt.Errorf("secure %s: it was created with no owner", path)
	case owner != nil && !got.Equals(owner):
		return fmt.Errorf("secure %s: it was not created owned by %s", path, accountName(owner))
	}
	control, _, err := sd.Control()
	switch {
	case err != nil:
		return fmt.Errorf("read ACL control of %s: %w", path, err)
	case protected && control&windows.SE_DACL_PROTECTED == 0:
		return fmt.Errorf("secure %s: it was not created with a protected ACL", path)
	}
	return nil
}
