// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix && !linux

package target

import "syscall"

// setParentDeathSignal does nothing: only Linux can tie a child's life to its
// parent's. A harness killed outright here can leave its children running.
func setParentDeathSignal(*syscall.SysProcAttr) {}
