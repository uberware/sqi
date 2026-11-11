// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package marker

import "syscall"

func processGroup() int { return syscall.Getpgrp() }
