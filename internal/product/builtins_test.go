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
		if err := product.ValidateTemplate(p.Template, p.Format, product.ValidateOptions{EnforceLimits: true}); err != nil {
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

// Every built-in ships a readme. These three are the first products a new
// operator opens, so they double as the worked example of what the readme
// field is for and which Markdown the renderer actually supports.
func TestBuiltins_AllHaveAReadme(t *testing.T) {
	builtins := product.Builtins()
	if len(builtins) == 0 {
		t.Fatal("no builtins loaded")
	}
	for _, p := range builtins {
		if strings.TrimSpace(p.Readme) == "" {
			t.Errorf("builtin %q has no readme", p.Name)
		}
		if err := product.ValidateMetadata(p); err != nil {
			t.Errorf("builtin %q: %v", p.Name, err)
		}
	}
}

// The built-in readmes are the reference for authors, so between them they
// must exercise the whole supported subset -- and nothing outside it. A
// construct the renderer does not support would render as literal text in the
// very examples people copy.
func TestBuiltins_ReadmesExerciseTheSupportedSubset(t *testing.T) {
	builtins := product.Builtins()
	var all strings.Builder
	for _, p := range builtins {
		all.WriteString(p.Readme)
		all.WriteString("\n")
	}
	corpus := all.String()

	supported := map[string]string{
		"ATX heading": "\n# ",
		"bullet list": "\n- ",
		"fenced code": "```",
		"bold":        "**",
		"inline code": "`",
		"a link":      "](http",
	}
	for name, marker := range supported {
		if !strings.Contains(corpus, marker) {
			t.Errorf("no builtin readme demonstrates %s (looked for %q)", name, marker)
		}
	}

	// Unsupported constructs render as literal text; none may appear.
	for _, p := range builtins {
		for _, bad := range []struct{ name, marker string }{
			{"an image", "!["},
			{"a blockquote", "\n> "},
			{"a table", "\n|"},
			{"raw HTML", "<"},
		} {
			if strings.Contains(p.Readme, bad.marker) {
				t.Errorf("builtin %q readme contains %s (%q), which the renderer does not support",
					p.Name, bad.name, bad.marker)
			}
		}
		// Nested list items silently lose their structure -- the renderer's
		// list regexes are anchored at column 0.
		for line := range strings.SplitSeq(p.Readme, "\n") {
			trimmed := strings.TrimLeft(line, " ")
			if len(line) > len(trimmed) && (strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ")) {
				t.Errorf("builtin %q readme has an indented list item %q; nesting is not supported", p.Name, line)
			}
		}
	}
}
