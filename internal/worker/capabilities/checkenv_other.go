// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package capabilities

// registryExists is a no-op on non-Windows platforms; registry checks are
// silently skipped (a detector relying only on registry emits no tag here).
func registryExists(string) bool { return false }
