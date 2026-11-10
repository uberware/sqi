// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/store"
)

// TestCapacityNameMatchesStore is the duplication guard for the capacity name
// rule: internal/config validates a provider name with its own copy of the
// pattern internal/store applies to a capacity group's name, because the two
// packages cannot import each other. The names a group may carry and the names
// a provider may carry must stay the same set, since a group selects a
// provider as `script:<name>`.
func TestCapacityNameMatchesStore(t *testing.T) {
	valid := func(name string) bool {
		g := store.CapacityGroup{
			Name:         name,
			Kind:         store.CapacityGroupNamed,
			Provider:     store.CapacityProviderExternal,
			MaxMachines:  1,
			MaxStepUp:    1,
			IdleTimeout:  store.MinCapacityGroupTimeout,
			BootTimeout:  store.MinCapacityGroupTimeout,
			RetireAction: store.CapacityRetireSleep,
		}
		return g.Validate() == nil
	}
	if !valid("baseline") {
		t.Fatal("the fixture group must be valid apart from its name")
	}
	for _, name := range []string{
		"", "a", "A", "0", "a.b", "a_b", "a-b", "a b", "-a", ".a", "_a", "a/b", "a:b", "é",
		strings.Repeat("a", 63), strings.Repeat("a", 64), "script:x", "a\n",
	} {
		if got, want := config.ValidCapacityName(name), valid(name); got != want {
			t.Errorf("name %q: internal/config says %v, internal/store says %v -- the two patterns have drifted", name, got, want)
		}
	}
}
