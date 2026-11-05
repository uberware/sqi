// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows && !unix

package observe

import (
	"errors"
	"time"
)

// processCPUTime is unavailable on this platform, so the generator's CPU is
// not sampled.
func processCPUTime() (time.Duration, error) {
	return 0, errors.New("observe: process CPU time is not available on this platform")
}
