// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration && !windows

package integration

import (
	"os"
	"os/exec"
	"syscall"
)

// setStoppableAttrs prepares cmd so [interruptProcess] can stop it; nothing is
// needed on POSIX.
func setStoppableAttrs(*exec.Cmd) {}

// interruptProcess asks p to shut down gracefully with SIGTERM, what a service
// manager sends a worker when its machine powers off.
func interruptProcess(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}
