// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux && !darwin

package bus

// fdHeadroom always passes: this system has no per-process descriptor limit
// sqi reads.
func fdHeadroom() error { return nil }
