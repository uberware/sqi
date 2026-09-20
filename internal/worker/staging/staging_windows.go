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
// Since H3 this only affects copyFile, which is the STAGE-IN path. Stage-out
// no longer opens by path at all: openStageOutSource opens through an
// os.Root, which on Windows maps to OBJ_DONT_REPARSE and makes the kernel
// refuse a lookup that meets ANY reparse point — a strictly stronger property
// than O_NOFOLLOW, and the one that closes the junction bypass.
//
// dest needs no equivalent: Go's syscall.Open sets FILE_FLAG_OPEN_REPARSE_POINT
// whenever O_CREAT|O_EXCL is requested, so copyFromFile's
// remove-then-O_EXCL-create can never be written through a pre-existing
// reparse point. See isolation.createExclusiveFile's doc for the same
// reasoning applied to the same problem.
const noFollowFlag = 0

// hasExtraHardlinks reports whether f's underlying file has more than one
// name (hardlink) on the volume. NTFS supports hardlinks and creating one
// requires no privilege, so this is a real check on Windows, not a stub.
//
// It used to be a stub — (false, nil), unconditionally — and turning it real
// was NOT free, so do not read it as pure parity. Both callers gained it at
// once: openStageOutSource (new with H3, stage-out only, adversarial) and
// copyFile, which is the built-in STAGE-IN copy plus copyTree's recursion and
// is not adversarial at all. So a job INPUT asset carrying a second NTFS
// hardlink now fails stage-in on Windows with "copy refused: ... has more
// than one hardlink", where before H3 it copied. Content-addressed and dedup
// asset stores, and "rsync --link-dest"-style delivery, produce multiply
// linked files as a matter of course. That cost is accepted — the refusal is
// correct under run-as-user isolation, and POSIX has always behaved this way
// — but it is a real change to legitimate Windows workloads, recorded here
// because this is the function that makes it happen. See copyFile's doc for
// the operator-facing half and docs/worker-configuration.md for the "a
// hardlink IS the file" reasoning.
//
// It must go through GetFileInformationByHandle because Windows exposes no
// link count via os.FileInfo: Sys() yields a *syscall.Win32FileAttributeData,
// which has no such field, and Go's Windows fileStat does not carry one
// either. Only the handle-based call answers the question.
//
// The handle is borrowed via SyscallConn, never f.Fd(): Fd() can disassociate
// a file from the runtime poller, whereas SyscallConn is the supported way to
// hand a descriptor to a syscall and keeps the file usable afterward.
//
// Windows caveat, recorded so it is not overclaimed: CreateHardLink refuses a
// target the caller cannot write, so this is a narrower primitive here than
// on POSIX (where fs.protected_hardlinks is the only thing narrowing it, and
// it is a host kernel setting sqi does not control). This check is justified
// on parity and defense in depth, not on a demonstrated escalation.
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
// sharing failure rather than a containment refusal — see
// classifyStageOutOpenError for why the two must not be worded alike.
//
// The sharing cases are the reason this is a per-platform helper at all: a
// task's background child that outlives it (nothing kills a task's process
// group on a SUCCESSFUL exit — see executor.processTree.release) can still
// hold the staged output open with a restrictive share mode, and NTFS answers
// the daemon's open with ERROR_SHARING_VIOLATION. Go maps neither that nor
// ERROR_LOCK_VIOLATION to fs.ErrPermission, so without naming them a mundane
// "the task left a writer open" is reported to the operator as a containment
// breach.
//
// An escape, and any reparse point met anywhere in the relative path, surface
// from os.Root as its own "path escapes from parent" error instead — verified
// on this platform, not assumed — so neither is misclassified here.
func isAccessError(err error) bool {
	return errors.Is(err, fs.ErrPermission) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
