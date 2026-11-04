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

// SetRetryBackoff shortens the retry pauses for a test and returns a function
// restoring them.
func SetRetryBackoff(base, maxWait time.Duration) (restore func()) {
	oldBase, oldMax := retryBase, retryMax
	retryBase, retryMax = base, maxWait
	return func() { retryBase, retryMax = oldBase, oldMax }
}
