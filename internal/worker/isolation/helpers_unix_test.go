// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package isolation_test

import (
	"os/exec"
	"testing"
)

// assertProcessGroupStillSet fails the test if Apply cleared the Setpgid that
// proctree.Configure set.
func assertProcessGroupStillSet(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Error("Apply must preserve Setpgid on the caller's SysProcAttr")
	}
}
