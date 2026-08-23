// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package staging

import (
	"fmt"
	"os"
	"syscall"
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
