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
// See secureDACL for why ownership is deliberately NOT transferred to the
// target, which is the one place this is intentionally stricter than POSIX.
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
// The inheritable ACE SecureWorkDir sets covers everything created AFTERWARDS,
// but not entries that already exist — staged content copied by an
// ACL-preserving tool arrives carrying explicit ACLs of its own, exactly as
// rsync -a preserves ownership on POSIX. A nil cred is a no-op.
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
		// No special-casing needed for a reparse point (symlink or
		// junction): WalkDir only ever recurses INTO an entry that reports
		// IsDir() true, and a reparse point never does (see
		// os/types_windows.go's fileStat.mode — a "surrogate" reparse point
		// deliberately does not carry the directory bit, precisely so
		// callers like this one don't need to guard against walking through
		// it). Descent is prevented structurally; there is nothing left for
		// this callback to do beyond securing the entry itself, exactly like
		// every other entry.
		//
		// An earlier revision returned filepath.SkipDir from a
		// ModeSymlink-specific branch here, believing that was needed to
		// stop descent. It was not, and it was actively wrong: SkipDir
		// returned for an entry whose IsDir() is false does not mean "don't
		// descend" to filepath.WalkDir — it means "stop visiting the REST OF
		// THE CURRENT DIRECTORY'S entries" (see the `for _, d1 := range
		// dirs { … if err == SkipDir { break } }` loop in
		// path/filepath/path.go). Since a reparse point sorts wherever its
		// name falls, that silently abandoned every sibling sorting after it
		// — a directory could look fully secured (ChownRecursive returning
		// nil) while part of the tree still carried whatever ACL it
		// inherited.
		return securePath(path, acl)
	})
}

// ValidateTraversable is a no-op on Windows, and — unlike the previous
// revision of this function — that is a property of the platform rather than
// of an unfinished implementation.
//
// Windows grants "Bypass traverse checking" (SeChangeNotifyPrivilege) to
// Everyone by default, which skips the access check on intermediate
// directories entirely: only the final object's ACL is evaluated. There is no
// POSIX-style ancestor "search bit" for a path-based check to inspect, so
// there is nothing here to validate.
//
// The guarantee this stands for is still checked, just somewhere else and
// against something real: logonUserProvider.Resolve inspects the TARGET'S OWN
// TOKEN for SeChangeNotifyPrivilege, because hardening guides do sometimes
// strip it, and refuses with a message naming the policy if it is absent.
// That keeps the POSIX philosophy — report, never silently widen an
// operator's directory — while checking the thing that is actually true here.
func ValidateTraversable(_ ...string) error {
	return nil
}

// WriteFileFchown (re)writes path with data via remove-then-O_EXCL-create
// (refusing to write THROUGH a pre-existing entry — see the POSIX
// implementation's doc for the full threat model and why a plain O_EXCL-only
// create, this function's own previous shape, incorrectly also refused a
// legitimate duplicate embedded-file name instead of only an attacker-planted
// entry), then — when cred is non-nil — applies cred's protected DACL to the
// still-open descriptor.
//
// The create goes through createExclusiveFile rather than os.OpenFile: a
// plain os.OpenFile(path, O_WRONLY|O_CREATE|O_EXCL, perm) only ever requests
// GENERIC_WRITE on Windows, which carries READ_CONTROL but not WRITE_DAC —
// and Windows fixes a handle's access rights at open time, so nothing done
// to that handle afterward can widen them. applyProtectedDACL would then
// fail ERROR_ACCESS_DENIED on every call. WRITE_DAC has to be requested on
// the SAME CreateFile call that creates the file, which is also exactly why
// this cannot be "create with os.OpenFile, then reopen by path with more
// access": a second, path-based open reintroduces the swap window this
// function exists to close (see the package doc's threat model above). The
// handle the write happens through is the only one that ever touches this
// inode, from create to ACL apply.
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
