// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// createLogDir creates a missing log directory the way [EnsureLogDir] does —
// a protected DACL granting full control to SYSTEM, Administrators and the
// account this process runs as — because a mode passed to os.MkdirAll is
// ignored on Windows, and %ProgramData%'s inherited ACL would let every local
// user read the service log. An existing directory keeps its DACL (spec §2),
// but one that is, or sits under, a junction or symbolic link is refused, as
// `service install` refuses it (see mkdirProtected).
func createLogDir(dir string) error {
	me, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("read process account: %w", err)
	}
	_, err = mkdirProtected(dir, me.User.Sid)
	return err
}
