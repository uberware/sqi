// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package staging

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// noFollowFlag is OR'd into every os.OpenFile call in copyFile that must not
// traverse a final-component symlink; see that function's doc.
//
// copyFile is used for stage-in only. Stage-out opens through an os.Root
// (openStageOutSource) and needs no such flag. A stage-in source is a
// job-declared asset outside any root, so there is no os.Root to open it
// through.
const noFollowFlag = syscall.O_NOFOLLOW

// hasExtraHardlinks reports whether f's underlying inode has more than one
// directory entry (link) pointing at it. A hardlink shares one inode with
// whatever it is linked to, so a stage-out source with an extra hardlink
// exposes its link partner once copied, as a symlink would (see
// openStageOutSource's doc).
//
// It takes *os.File rather than os.FileInfo because Windows exposes a link
// count only through GetFileInformationByHandle on an open handle (Sys() is
// *syscall.Win32FileAttributeData, and Go's Windows fileStat carries no
// NumberOfLinks field). Taking a descriptor also means every call site checks
// the inode it reads.
func hasExtraHardlinks(f *os.File) (bool, error) {
	info, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("fstat %q: %w", f.Name(), err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("stat_t unavailable for %q", f.Name())
	}
	return stat.Nlink > 1, nil
}

// isAccessError reports whether err from an os.Root lookup is a permission or
// I/O failure rather than a containment refusal; see
// classifyStageOutOpenError for why the two must not be worded alike. An
// escape or a refused reparse point surfaces from os.Root as its own
// "path escapes from parent" error (or ELOOP), never as one of these.
func isAccessError(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EIO)
}
