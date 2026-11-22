// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix && !linux && !darwin

package proctree

// leaderStartTime is 0 where there is no process-table reader.
func leaderStartTime(int) int64 { return 0 }
