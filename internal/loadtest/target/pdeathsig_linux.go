// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import "syscall"

// setParentDeathSignal has the kernel kill the child when the harness dies,
// so a harness killed outright (SIGKILL, OOM) leaves no server or worker
// behind. The signal fires when the creating OS thread exits; Go retires a
// thread only when a goroutine exits while locked to it, which the harness
// never does around a process start.
func setParentDeathSignal(attr *syscall.SysProcAttr) {
	attr.Pdeathsig = syscall.SIGKILL
}
