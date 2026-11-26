// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import "testing"

func TestJoinedLess_MatchesBuiltString(t *testing.T) {
	strs := []string{"", "/", "/a", "/a/", "/a/b", "/a/bc", "/ab", "/a/b/c", "/b", "/a~1b"}
	for _, parent := range strs {
		for _, token := range []string{"", "b", "bc", "~1", "a"} {
			for _, s := range strs {
				want := parent+"/"+token < s
				if got := joinedLess(parent, token, s); got != want {
					t.Errorf("joinedLess(%q, %q, %q) = %v, want %v", parent, token, s, got, want)
				}
			}
		}
	}
}
