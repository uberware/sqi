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
const noFollowFlag = syscall.O_NOFOLLOW

// hasExtraHardlinks reports whether info's underlying inode has more than one
// directory entry (link) pointing at it. A hardlink shares one inode with
// whatever it is linked to, so a stage-out source with an extra hardlink
// leaks its link partner identically to a symlink once copied — see
// validateStageOutSource's doc for the full threat model.
func hasExtraHardlinks(info os.FileInfo) (bool, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("stat_t unavailable for %q", info.Name())
	}
	return stat.Nlink > 1, nil
}
