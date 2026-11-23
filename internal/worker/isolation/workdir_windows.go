// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// SecureWorkDir restricts dir to cred's identity: a protected DACL granting
// full control to the target account, SYSTEM, and Administrators, and nothing
// else. Called once at session creation so a session's scratch directory is
// unreadable to any account but the one its tasks run as. A nil cred is a
// no-op, matching Apply's no-isolation behavior.
//
// This is the Windows counterpart of workdir_unix.go's chown + chmod 0700.
// See secureDACL for why ownership is not transferred to the target, the one
// place this is stricter than POSIX.
func SecureWorkDir(dir string, cred *Credential) error {
	if cred == nil {
		return nil
	}
	sid, err := lookupUserSID(cred.User)
	if err != nil {
		return err
	}
	acl, err := secureDACL(sid)
	if err != nil {
		return err
	}
	return securePath(dir, acl)
}

// ChownRecursive walks root and applies cred's protected DACL to every entry.
//
// The inheritable ACE SecureWorkDir sets covers everything created afterward,
// but not entries that already exist: staged content copied by an
// ACL-preserving tool arrives carrying explicit ACLs of its own, as rsync -a
// preserves ownership on POSIX. A nil cred is a no-op.
//
// Every entry is opened with FILE_FLAG_OPEN_REPARSE_POINT (see openForACL),
// never touched through a path-based call, so a junction planted inside the
// tree has its own entry secured rather than whatever it points at.
func ChownRecursive(root string, cred *Credential) error {
	if cred == nil {
		return nil
	}
	sid, err := lookupUserSID(cred.User)
	if err != nil {
		return err
	}
	acl, err := secureDACL(sid)
	if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// A reparse point (symlink or junction) needs no special case:
		// WalkDir recurses only into an entry whose IsDir() is true, and a
		// reparse point's never is (in os/types_windows.go's fileStat.mode a
		// "surrogate" reparse point does not carry the directory bit). The
		// callback secures the entry itself, like every other entry.
		//
		// Returning filepath.SkipDir from a ModeSymlink branch to stop
		// descent would be wrong: for an entry whose IsDir() is false,
		// SkipDir tells filepath.WalkDir to skip the rest of the current
		// directory's entries (see the `for _, d1 := range dirs { … if err
		// == SkipDir { break } }` loop in path/filepath/path.go). Every
		// sibling sorting after the reparse point would be skipped, and
		// ChownRecursive would return nil with part of the tree still
		// carrying whatever ACL it inherited.
		return securePath(path, acl)
	})
}

// ValidateTraversable is a no-op on Windows.
//
// Windows grants "Bypass traverse checking" (SeChangeNotifyPrivilege) to
// Everyone by default, which skips the access check on intermediate
// directories: only the final object's ACL is evaluated. There is no
// POSIX-style ancestor "search bit" for a path-based check to inspect.
//
// The equivalent check is in logonUserProvider.Resolve, which inspects the
// target's own token for SeChangeNotifyPrivilege (hardening guides sometimes
// strip it) and refuses with a message naming the policy if it is absent. As
// on POSIX, sqi reports the problem and never widens an operator's
// directory.
func ValidateTraversable(_ ...string) error {
	return nil
}

// WriteFileFchown (re)writes path with data by removing any existing entry
// and creating the file exclusively, so it never writes through a
// pre-existing entry (see the POSIX implementation's doc for the threat model
// and why the remove comes first), then, when cred is non-nil, applies cred's
// protected DACL to the still-open descriptor.
//
// The create goes through createExclusiveFile rather than os.OpenFile: a
// plain os.OpenFile(path, O_WRONLY|O_CREATE|O_EXCL, perm) only ever requests
// GENERIC_WRITE on Windows, which carries READ_CONTROL but not WRITE_DAC,
// and Windows fixes a handle's access rights at open time, so
// applyProtectedDACL would fail with ERROR_ACCESS_DENIED. WRITE_DAC has to be
// requested in the CreateFile call that creates the file: reopening by path
// with more access would reopen the swap window this function closes (see
// the POSIX implementation's doc). The handle the write goes through is the
// only one that touches this file, from create to ACL apply.
//
// O_NOFOLLOW has no Windows equivalent flag: NTFS symlinks/junctions require
// an explicit reparse-point create call rather than being creatable via a
// plain "create file" open the way POSIX symlinks are, so O_EXCL alone
// already refuses to write through a pre-existing entry of any kind here.
func WriteFileFchown(path string, data []byte, perm os.FileMode, cred *Credential) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove existing %q: %w", path, err)
	}
	var extraAccess uint32
	if cred != nil {
		extraAccess = windows.WRITE_DAC | windows.READ_CONTROL
	}
	h, err := createExclusiveFile(path, perm, extraAccess)
	if err != nil {
		return fmt.Errorf("create %q: %w", path, err)
	}
	f := os.NewFile(uintptr(h), path)
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	if cred == nil {
		return nil
	}
	sid, err := lookupUserSID(cred.User)
	if err != nil {
		return err
	}
	acl, err := secureDACL(sid)
	if err != nil {
		return err
	}
	return applyProtectedDACL(windows.Handle(f.Fd()), acl)
}
