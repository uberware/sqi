// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package wol

import (
	"fmt"
	"syscall"
)

// setBroadcast enables SO_BROADCAST on the socket before it is bound.
func setBroadcast(_, _ string, c syscall.RawConn) error {
	var sockErr error
	if err := c.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	}); err != nil {
		return err
	}
	if sockErr != nil {
		return fmt.Errorf("wol: set SO_BROADCAST: %w", sockErr)
	}
	return nil
}
