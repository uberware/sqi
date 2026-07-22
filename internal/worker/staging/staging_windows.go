// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package staging

import "os"

// noFollowFlag is a no-op on Windows: there is no portable O_NOFOLLOW open
// flag, and NTFS reparse points (its symlink equivalent) are handled by a
// different API than the POSIX open(2) flags this constant augments on unix.
// Task isolation (internal/worker/isolation) is not yet supported on Windows
// at all (Provider.Capable() unconditionally refuses — see that package's
// doc), so the root-privilege-escalation scenario this hardening exists for
// cannot occur on a Windows worker today: there is no run-as-user credential
// switch for a task to have gained a foothold under. builtinCopy's own
// os.Lstat check (which IS portable) still refuses a symlinked stage-out
// source on Windows.
const noFollowFlag = 0

// hasExtraHardlinks always reports false on Windows. NTFS does support
// hardlinks, but querying a link count portably requires
// GetFileInformationByHandle rather than anything exposed via os.FileInfo —
// left unimplemented because, as above, isolation (the scenario this guards
// against) is not reachable on Windows yet. Revisit alongside a real Windows
// isolation provider (see internal/worker/isolation's "Platform support"
// doc).
func hasExtraHardlinks(_ os.FileInfo) (bool, error) {
	return false, nil
}
