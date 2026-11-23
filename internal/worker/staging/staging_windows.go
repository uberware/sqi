// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package staging

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// noFollowFlag is a no-op on Windows: there is no portable O_NOFOLLOW open
// flag, and NTFS reparse points (its symlink and junction equivalent) are
// handled by a different API than the POSIX open(2) flags this constant
// augments on unix.
//
// This affects only copyFile, the stage-in path. Stage-out does not open by
// path: openStageOutSource opens through an os.Root, which on Windows walks
// with OBJ_DONT_REPARSE and refuses every junction (see its doc for the one
// in-root symlink case it follows).
//
// dest needs no equivalent: Go's syscall.Open sets FILE_FLAG_OPEN_REPARSE_POINT
// whenever O_CREAT|O_EXCL is requested, so copyFromFile's
// remove-then-O_EXCL-create is never written through a pre-existing reparse
// point. See isolation.createExclusiveFile's doc for the same reasoning.
const noFollowFlag = 0

// hasExtraHardlinks reports whether f's underlying file has more than one
// name (hardlink) on the volume. NTFS supports hardlinks and creating one
// requires no privilege.
//
// Both callers use it: openStageOutSource (stage-out) and copyFile, which is
// the built-in stage-in copy and the one copyTree recurses through. A job
// input asset carrying a second NTFS hardlink therefore fails stage-in on
// Windows with "copy refused: ... has more than one hardlink". Content-addressed
// and dedup asset stores, and "rsync --link-dest"-style delivery, produce
// multiply linked files routinely. POSIX behaves the same way. See copyFile's
// doc, and docs/worker-configuration.md for why a hardlink is treated as the
// file itself.
//
// It goes through GetFileInformationByHandle because Windows exposes no link
// count via os.FileInfo: Sys() yields a *syscall.Win32FileAttributeData, which
// has no such field, and Go's Windows fileStat does not carry one either.
//
// The handle is borrowed via SyscallConn, never f.Fd(): Fd() can disassociate
// a file from the runtime poller, whereas SyscallConn hands a descriptor to a
// syscall and keeps the file usable afterward.
//
// CreateHardLink refuses a target the caller cannot write, so the primitive
// is narrower here than on POSIX (where only the host's fs.protected_hardlinks
// setting narrows it, and sqi does not control that). The check is kept for
// parity with POSIX and as defense in depth.
func hasExtraHardlinks(f *os.File) (bool, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return false, fmt.Errorf("syscall conn for %q: %w", f.Name(), err)
	}
	var (
		info    syscall.ByHandleFileInformation
		infoErr error
	)
	if err := rc.Control(func(fd uintptr) {
		infoErr = syscall.GetFileInformationByHandle(syscall.Handle(fd), &info)
	}); err != nil {
		return false, fmt.Errorf("borrow handle for %q: %w", f.Name(), err)
	}
	if infoErr != nil {
		return false, fmt.Errorf("GetFileInformationByHandle %q: %w", f.Name(), infoErr)
	}
	return info.NumberOfLinks > 1, nil
}

// isAccessError reports whether err from an os.Root lookup is a permission or
// sharing failure rather than a containment refusal; see
// classifyStageOutOpenError for why the two must not be worded alike.
//
// The sharing cases are why this is a per-platform helper: a task's background
// child that outlives it (the process tree is not killed when a task exits
// successfully; see proctree.Tree.Release) can still hold the staged output
// open with a restrictive share mode, and NTFS answers the worker's open with
// ERROR_SHARING_VIOLATION. Go maps neither that nor ERROR_LOCK_VIOLATION to
// fs.ErrPermission, so without naming them a task that left a writer open
// would be reported to the operator as a containment breach.
//
// An escape, and any reparse point met anywhere in the relative path, surface
// from os.Root as its own "path escapes from parent" error, so neither is
// misclassified here.
func isAccessError(err error) bool {
	return errors.Is(err, fs.ErrPermission) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
