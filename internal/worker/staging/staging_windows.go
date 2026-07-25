// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package staging

import "os"

// noFollowFlag is a no-op on Windows: there is no portable O_NOFOLLOW open
// flag, and NTFS reparse points (its symlink equivalent) are handled by a
// different API than the POSIX open(2) flags this constant augments on unix.
//
// Windows run-as-user isolation (internal/worker/isolation) IS now supported
// (Provider.Capable() reports the real privilege check — see that package's
// doc), so the root-privilege-escalation scenario this hardening exists for
// IS reachable on a Windows worker: a task running under a resolved
// credential could plant a symlink or junction for a subsequent stage-out to
// follow. builtinCopy's own os.Lstat check (which IS portable) still refuses
// a symlinked stage-out source on Windows; the fd-based O_NOFOLLOW re-check
// this constant would augment does not have a Windows equivalent yet. This is
// a known, currently-open gap, not a theoretical one — see
// TestCopyFile_RefusesSourceWithExtraHardlink and
// TestStager_StageOut_RefusesHardlinkedSource, both skipped on Windows.
const noFollowFlag = 0

// hasExtraHardlinks always reports false on Windows. NTFS does support
// hardlinks, but querying a link count portably requires
// GetFileInformationByHandle rather than anything exposed via os.FileInfo —
// left unimplemented. Since Windows run-as-user isolation is now supported
// (see noFollowFlag's doc above), the hardlink-swap scenario this guards
// against on POSIX is a real, open gap on Windows too, not one made moot by
// isolation being absent.
func hasExtraHardlinks(_ os.FileInfo) (bool, error) {
	return false, nil
}
