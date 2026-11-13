// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !unix

package marker

// processGroup is 0 where there are no process groups to signal.
func processGroup() int { return 0 }
