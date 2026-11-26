// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build conformance

package conformance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/openjd"
)

// sweptExtensionDirs are the suite directories for the extensions sqi
// supports. FEATURE_BUNDLE_1 and WRAP_ACTIONS are not, so their keys are
// rightly unknown; a fixture elsewhere that declares one is skipped too.
var sweptExtensionDirs = []string{"base", "EXPR", "TASK_CHUNKING", "REDACTED_ENV_VARS"}

// TestConformance_ValidFixturesHaveNoUnknownKeys checks that no valid fixture
// trips the unknown-key check: every key a valid template uses is one the
// schema defines, so a failure here means the allowed-key table in
// internal/openjd/unknownkeys.go is missing a key. It reads the jobs/ fixtures'
// templates too, which TestConformance_Templates never scores.
func TestConformance_ValidFixturesHaveNoUnknownKeys(t *testing.T) {
	sources := validFixtureTemplates(t)
	if len(sources) == 0 {
		t.Fatal("no fixtures found; is the submodule checked out?")
	}
	for path, src := range sources {
		format := openjd.FormatYAML
		if strings.HasSuffix(path, ".json") {
			format = openjd.FormatJSON
		}
		tmpl, err := openjd.Parse(src, format)
		if err != nil || declaresUnsupported(tmpl) {
			continue // scored by TestConformance_Templates, or rightly unknown
		}
		for _, e := range openjd.Validate(tmpl) {
			if strings.HasPrefix(e.Message, "unknown field; the ") {
				t.Errorf("%s: %s: %s", path, e.Pointer, e.Message)
			}
		}
	}
}

// declaresUnsupported reports whether tmpl declares an extension sqi does not
// support (FEATURE_BUNDLE_1's bash: action, say), whose keys are rightly unknown.
func declaresUnsupported(tmpl *openjd.JobTemplate) bool {
	for _, name := range tmpl.Extensions {
		if ext, ok := openjd.LookupExtension(name); !ok || ext.Status != openjd.StatusSupported {
			return true
		}
	}
	return false
}

// validFixtureTemplates returns the template of every valid job_templates/ and
// jobs/ fixture in sweptExtensionDirs, keyed by fixture path.
func validFixtureTemplates(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, dir := range sweptExtensionDirs {
		for _, pattern := range []string{"job_templates/*.yaml", "job_templates/*.json", "jobs/*.test.yaml"} {
			paths, err := filepath.Glob(filepath.Join(SuiteRoot, dir, pattern))
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range paths {
				if strings.Contains(filepath.Base(p), ".invalid.") {
					continue
				}
				out[p] = fixtureTemplate(t, p)
			}
		}
	}
	return out
}

// fixtureTemplate returns a fixture's template: the whole file for a
// job_templates/ fixture, the re-serialized template: key for a jobs/ one.
func fixtureTemplate(t *testing.T, path string) []byte {
	t.Helper()
	if !strings.HasSuffix(path, ".test.yaml") {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Template yaml.Node `yaml:"template"`
	}
	if err := yaml.Unmarshal(data, &fx); err != nil {
		t.Fatalf("%s: decode fixture: %v", path, err)
	}
	raw, err := yaml.Marshal(&fx.Template)
	if err != nil {
		t.Fatalf("%s: re-serialize template: %v", path, err)
	}
	return raw
}
