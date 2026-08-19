// SPDX-License-Identifier: AGPL-3.0-or-later

package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/fsutil"
)

// shippedDefinitionDirs is every directory whose products this repo authors and
// ships: the embedded built-ins plus both preset trees. All three reach users
// through the same renderer, so all three answer to the same rules.
var shippedDefinitionDirs = []string{
	"builtins",
	"../../presets/sqi",
	"../../presets/testing",
}

// shippedDefinition is one product definition file, decoded far enough to check
// its prose. The template is deliberately not decoded -- the schema tests own
// that.
type shippedDefinition struct {
	Name        string `yaml:"name"`
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
	Readme      string `yaml:"readme"`
}

// loadShippedDefinitions reads every product definition this repo ships.
func loadShippedDefinitions(t *testing.T) map[string]shippedDefinition {
	t.Helper()
	out := map[string]shippedDefinition{}
	for _, dir := range shippedDefinitionDirs {
		files, err := fsutil.Glob(filepath.Join(dir, "*.yaml"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		if len(files) == 0 {
			t.Fatalf("no definitions found under %s", dir)
		}
		for _, f := range files {
			data, readErr := os.ReadFile(f)
			if readErr != nil {
				t.Fatalf("read %s: %v", f, readErr)
			}
			var def shippedDefinition
			if err := yaml.Unmarshal(data, &def); err != nil {
				t.Fatalf("parse %s: %v", f, err)
			}
			out[f] = def
		}
	}
	return out
}

// Every shipped product carries a readme. These are the products users meet
// first, and the readme is where a product explains what it produces -- a
// product without one leaves its detail page showing nothing but a template.
func TestShippedDefinitions_AllHaveAReadme(t *testing.T) {
	t.Parallel()
	for path, def := range loadShippedDefinitions(t) {
		if strings.TrimSpace(def.Readme) == "" {
			t.Errorf("%s: no readme", path)
		}
	}
}

// Descriptions are the short, plain-text blurb: rendered into an unclamped
// picker card and a native Blender EnumProperty tooltip, and the only field
// product search matches. Markdown in one renders as literal punctuation.
func TestShippedDefinitions_DescriptionsAreShortPlainText(t *testing.T) {
	t.Parallel()
	// 3 sentences is the house standard; the 500-rune cap in limits.go is the
	// hard bound, and this is the editorial one well inside it.
	const maxSentences = 3
	for path, def := range loadShippedDefinitions(t) {
		desc := strings.TrimSpace(def.Description)
		if desc == "" {
			t.Errorf("%s: no description", path)
			continue
		}
		if n := utf8.RuneCountInString(desc); n > MaxDescriptionLen {
			t.Errorf("%s: description is %d characters, limit is %d", path, n, MaxDescriptionLen)
		}
		if n := strings.Count(desc, ".") + strings.Count(desc, "!") + strings.Count(desc, "?"); n > maxSentences {
			t.Errorf("%s: description reads as %d sentences, house standard is at most %d",
				path, n, maxSentences)
		}
		for _, m := range []struct{ name, marker string }{
			{"a heading", "#"},
			{"bold or a list bullet", "**"},
			{"inline code", "`"},
			{"a link", "]("},
		} {
			if strings.Contains(desc, m.marker) {
				t.Errorf("%s: description contains %s (%q) -- descriptions are plain text",
					path, m.name, m.marker)
			}
		}
	}
}

// Readmes may use only the subset web/src/components/Markdown.tsx implements.
// Anything else renders as literal text in the UI -- silently, with no error,
// which is exactly how three shipped presets once shipped nested lists that
// rendered as stray paragraphs.
func TestShippedDefinitions_ReadmesUseTheSupportedSubset(t *testing.T) {
	t.Parallel()
	// Substring matching is too crude for these: `ffmpeg -ss <offset> -t ...`
	// in prose contains both "<" and "> ". Block constructs are therefore
	// matched at line starts, and the HTML check names tags an author might
	// genuinely expect to render. Raw HTML is harmless either way -- the
	// renderer emits React elements, so it escapes -- the check is about a
	// mistaken expectation, not a vulnerability.
	inlineUnsupported := []struct{ name, marker string }{
		{"an image", "!["},
	}
	htmlTags := []string{"<p>", "<br", "<div", "<span", "<b>", "<i>", "<em>", "<strong>", "<img", "<a ", "<script"}
	for path, def := range loadShippedDefinitions(t) {
		readme := def.Readme
		if strings.TrimSpace(readme) == "" {
			continue // reported by TestShippedDefinitions_AllHaveAReadme
		}
		if n := utf8.RuneCountInString(readme); n > MaxReadmeLen {
			t.Errorf("%s: readme is %d characters, limit is %d", path, n, MaxReadmeLen)
		}
		for _, u := range inlineUnsupported {
			if strings.Contains(readme, u.marker) {
				t.Errorf("%s: readme contains %s (%q), which the renderer does not support "+
					"and will render as literal text", path, u.name, u.marker)
			}
		}
		for _, tag := range htmlTags {
			if strings.Contains(strings.ToLower(readme), tag) {
				t.Errorf("%s: readme contains raw HTML (%q), which renders as literal text", path, tag)
			}
		}
		for i, line := range strings.Split(readme, "\n") {
			switch {
			case strings.HasPrefix(line, "> "), line == ">":
				t.Errorf("%s:%d: blockquote -- not supported, renders as literal text", path, i+1)
			case strings.HasPrefix(line, "|"):
				t.Errorf("%s:%d: table row -- not supported, renders as literal text", path, i+1)
			}
		}
		// The renderer's list regexes are anchored at column 0, so an indented
		// list item matches nothing and falls through into a paragraph with a
		// stray leading marker. The structure is lost with no error anywhere.
		for i, line := range strings.Split(readme, "\n") {
			trimmed := strings.TrimLeft(line, " \t")
			if len(line) == len(trimmed) {
				continue
			}
			if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
				t.Errorf("%s:%d: indented list item %q -- nesting is not supported",
					path, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// Every readme opens with a level-1 heading. The renderer offsets it to <h3>,
// nesting it under the detail page's own <h1>/<h2>, so this is what gives the
// rendered readme a title rather than starting mid-prose.
func TestShippedDefinitions_ReadmesOpenWithAHeading(t *testing.T) {
	t.Parallel()
	for path, def := range loadShippedDefinitions(t) {
		readme := strings.TrimSpace(def.Readme)
		if readme == "" {
			continue // reported by TestShippedDefinitions_AllHaveAReadme
		}
		first, _, _ := strings.Cut(readme, "\n")
		if !strings.HasPrefix(first, "# ") {
			t.Errorf("%s: readme opens with %q, want a `# ` heading", path, first)
		}
	}
}
