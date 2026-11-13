// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix && !linux

package diskfill

import (
	"errors"
	"os"
)

// preallocate always fails off Linux, so the ballast is written.
func preallocate(*os.File, int64) error { return errors.New("no fallocate") }
