// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation_test

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

// assertProcessGroupStillSet fails the test if Apply cleared the
// CREATE_SUSPENDED that proctree.Configure set, which Apply must keep when it
// attaches the logon token.
func assertProcessGroupStillSet(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&windows.CREATE_SUSPENDED == 0 {
		t.Error("Apply must preserve CREATE_SUSPENDED on the caller's SysProcAttr")
	}
}
