// SPDX-License-Identifier: AGPL-3.0-or-later

package client

import "time"

// SetRequestTimeout shortens the per-request deadline for a test and returns
// a function restoring it.
func SetRequestTimeout(d time.Duration) (restore func()) {
	old := requestTimeout
	requestTimeout = d
	return func() { requestTimeout = old }
}
