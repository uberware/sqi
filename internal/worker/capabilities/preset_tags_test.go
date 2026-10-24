// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestReferencePresets_RequireOnlyValidIdentifierTags checks the key of every
// attr.worker.tag.<key> a shipped preset requires. OpenJD identifiers allow only
// letters, digits and underscores and must not start with a digit, so a
// conformant implementation rejects a template that names anything else. sqi's own
// validator is more lenient and accepted 3dsmax and 3delight for a long time,
// which is how those two tags shipped invalid; this is the check that would have
// caught them.
func TestReferencePresets_RequireOnlyValidIdentifierTags(t *testing.T) {
	valid := regexp.MustCompile("^[A-Za-z_][A-Za-z0-9_]*$")
	// The key runs to the end of the name: whitespace, a quote or a colon ends
	// it. The coverage test above uses a narrower pattern that tolerates the
	// characters this one is looking for.
	keyRe := regexp.MustCompile(`attr\.worker\.tag\.([^\s"':]+)`)

	root := filepath.Join("..", "..", "..", "presets", "sqi")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read presets dir: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range keyRe.FindAllStringSubmatch(string(data), -1) {
			key := strings.TrimRight(m[1], ".,)`")
			checked++
			if !valid.MatchString(key) {
				t.Errorf("%s requires attr.worker.tag.%s, which is not a valid OpenJD identifier", e.Name(), key)
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no attr.worker.tag.<key> in any preset; the pattern or the path is wrong")
	}
}
