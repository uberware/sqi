// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package isolation_test

import (
	"os/exec"
	"syscall"
	"testing"
)

// configureTestProcessGroup mirrors executor.configureProcessGroup: it sets
// Setpgid on cmd's SysProcAttr, the same field the real executor sets before
// calling isolation.Apply. Apply must preserve it.
func configureTestProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// assertProcessGroupStillSet fails the test if Apply cleared Setpgid.
func assertProcessGroupStillSet(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Error("Apply must preserve Setpgid on the caller's SysProcAttr")
	}
}
