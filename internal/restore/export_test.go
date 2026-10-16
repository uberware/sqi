// SPDX-License-Identifier: AGPL-3.0-or-later

package restore

import "testing"

// SetRenameForTest replaces the rename Run uses to move files, for t's
// duration.
func SetRenameForTest(t testing.TB, f func(from, to string) error) {
	t.Helper()
	prev := rename
	rename = f
	t.Cleanup(func() { rename = prev })
}
