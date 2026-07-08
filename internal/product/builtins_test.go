// SPDX-License-Identifier: AGPL-3.0-or-later

package product_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
)

func TestBuiltins_LoadValidateAndStamp(t *testing.T) {
	b := product.Builtins()
	names := make([]string, len(b))
	for i, p := range b {
		names[i] = p.Name
		if p.Source != store.SourceBuiltin {
			t.Errorf("%s: source = %q, want builtin", p.Name, p.Source)
		}
		if err := product.ValidateTemplate(p.Template, p.Format, true); err != nil {
			t.Errorf("%s: template invalid: %v", p.Name, err)
		}
	}
	// Sorted by name and exactly the expected three.
	want := []string{"container", "python", "script"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("builtins = %v, want %v", names, want)
	}
}

func TestBuiltins_ContainerDeclaresDockerRequirement(t *testing.T) {
	for _, p := range product.Builtins() {
		if p.Name == "container" {
			// Must use the tag namespace: the scheduler only resolves custom
			// attributes via "attr.worker.tag.<key>" (see internal/scheduler
			// matcher.go workerAttributeValue); an unprefixed name never matches.
			if !strings.Contains(p.Template, "attr.worker.tag.docker") {
				t.Fatalf("container missing docker host requirement: %s", p.Template)
			}
			return
		}
	}
	t.Fatal("container built-in not found")
}
