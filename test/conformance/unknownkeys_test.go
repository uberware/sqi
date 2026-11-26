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
	"github.com/uberware/sqi/test/conformance"
)

// TestConformance_ValidFixturesHaveNoUnknownKeys checks that no valid fixture
// trips the unknown-key check: every key a valid template uses is one the
// schema defines, so a failure here means the allowed-key table in
// internal/openjd/unknownkeys.go is missing a key. It reads the jobs/ fixtures'
// templates too, which TestConformance_Templates never scores. Only fixtures of
// the extensions sqi supports are swept (conformance.Classify): another
// extension's keys are rightly unknown.
func TestConformance_ValidFixturesHaveNoUnknownKeys(t *testing.T) {
	paths := validFixturePaths(t)
	if len(paths) == 0 {
		t.Fatal("no fixtures found; is the submodule checked out?")
	}
	for _, path := range paths {
		format := openjd.FormatYAML
		if strings.HasSuffix(path, ".json") {
			format = openjd.FormatJSON
		}
		tmpl, err := openjd.Parse(fixtureTemplate(t, path), format)
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

// validFixturePaths returns every valid job_templates/ and jobs/ fixture whose
// extension directory is live.
func validFixturePaths(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, pattern := range []string{"job_templates/*.yaml", "job_templates/*.json", "jobs/*.test.yaml"} {
		paths, err := filepath.Glob(filepath.Join(SuiteRoot, "*", pattern))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range paths {
			// A leading dot is an AppleDouble sidecar or other OS metadata;
			// no fixture begins with one.
			base := filepath.Base(p)
			if strings.HasPrefix(base, ".") || strings.Contains(base, ".invalid.") ||
				conformance.Classify(conformance.ExtensionFor(p), conformance.KindFor(p)) != conformance.StateLive {
				continue
			}
			out = append(out, p)
		}
	}
	return out
}

// fixtureTemplate returns a fixture's template: the whole file for a
// job_templates/ fixture, the re-serialized template: key for a jobs/ one.
// Only that key is decoded (not loadChunkingJob): the expected: block differs
// between extensions' jobs/ fixtures.
func fixtureTemplate(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, ".test.yaml") {
		return data
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
