// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix && !linux

package target

import "syscall"

// setParentDeathSignal does nothing: the harness ties a child's life to its
// own only on Linux. A harness killed outright here can leave its children
// running.
func setParentDeathSignal(*syscall.SysProcAttr) {}
