// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import (
	"os"
	"syscall"
)

// stopSignals end a run cleanly: os.Interrupt is Ctrl-C and Ctrl-Break, and
// Go delivers a console close, logoff or shutdown as SIGTERM.
var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}
