// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package staging

import (
	"fmt"
	"os"
	"syscall"
)

// noFollowFlag is OR'd into every os.OpenFile call in copyFile that must not
// traverse a final-component symlink — see that function's doc.
//
// Since H3, copyFile is the STAGE-IN path only: stage-out goes through
// openStageOutSource, which opens via os.Root and needs no such flag because
// the kernel refuses the lookup outright. This constant still matters because
// a stage-in source is a job-declared asset outside any root, so there is no
// os.Root to open it through.
const noFollowFlag = syscall.O_NOFOLLOW

// hasExtraHardlinks reports whether f's underlying inode has more than one
// directory entry (link) pointing at it. A hardlink shares one inode with
// whatever it is linked to, so a stage-out source with an extra hardlink
// leaks its link partner identically to a symlink once copied — see
// openStageOutSource's doc for the full threat model.
//
// It takes *os.File rather than os.FileInfo because that is the only shape
// BOTH platforms can satisfy: Windows exposes no link count through
// os.FileInfo at all (Sys() is *syscall.Win32FileAttributeData, and Go's
// Windows fileStat carries no NumberOfLinks field), only through
// GetFileInformationByHandle on an open handle. Taking a descriptor also
// makes every call site inherently fd-based, which is the property that
// closes the TOCTOU rather than merely narrowing it.
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
