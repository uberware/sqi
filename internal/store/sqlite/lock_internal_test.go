// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestJoinLinkTarget(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "links")
	cases := []struct{ name, target, want string }{
		{"relative", "sqi.db", filepath.Join(dir, "sqi.db")},
		{"parent", filepath.Join("..", "sqi.db"), filepath.Join(filepath.Dir(dir), "sqi.db")},
		{"absolute", filepath.Join(t.TempDir(), "sqi.db"), ""},
	}
	cases[2].want = cases[2].target
	if runtime.GOOS == "windows" {
		// Rooted but without a volume: on the link directory's volume.
		cases = append(cases, struct{ name, target, want string }{"rooted", `\data\sqi.db`, filepath.VolumeName(dir) + `\data\sqi.db`})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinLinkTarget(dir, tc.target); got != tc.want {
				t.Errorf("joinLinkTarget(%q, %q) = %q, want %q", dir, tc.target, got, tc.want)
			}
		})
	}
}
