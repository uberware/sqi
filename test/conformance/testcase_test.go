// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance_test

import (
	"testing"

	"github.com/uberware/sqi/test/conformance"
)

func TestParseTestCase(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		section string
		desc    string
		invalid bool
		jobTest bool
	}{
		{
			name:    "plain template test",
			path:    "base/job_templates/1.1--minimal-job-template.yaml",
			section: "1.1", desc: "minimal-job-template",
		},
		{
			name:    "invalid template test",
			path:    "base/job_templates/2.1--missing-name.invalid.yaml",
			section: "2.1", desc: "missing-name", invalid: true,
		},
		{
			name:    "expr-prefixed section",
			path:    "EXPR/job_templates/expr1.1--arithmetic-expr.yaml",
			section: "expr1.1", desc: "arithmetic-expr",
		},
		{
			name:    "job execution test",
			path:    "EXPR/jobs/expr2.2.4--upper.test.yaml",
			section: "expr2.2.4", desc: "upper", jobTest: true,
		},
		{
			name:    "invalid job execution test",
			path:    "base/jobs/5--bad-action.invalid.test.yaml",
			section: "5", desc: "bad-action", invalid: true, jobTest: true,
		},
		{
			name:    "no section prefix",
			path:    "TASK_CHUNKING/jobs/contiguous-even.test.yaml",
			section: "", desc: "contiguous-even", jobTest: true,
		},
		{
			name:    "multi-part section with dashes in description",
			path:    "base/job_templates/3.3.2--allof-nested-attr.yaml",
			section: "3.3.2", desc: "allof-nested-attr",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := conformance.ParseTestCase(tt.path)
			if got.Section != tt.section {
				t.Errorf("Section = %q, want %q", got.Section, tt.section)
			}
			if got.Description != tt.desc {
				t.Errorf("Description = %q, want %q", got.Description, tt.desc)
			}
			if got.Invalid != tt.invalid {
				t.Errorf("Invalid = %v, want %v", got.Invalid, tt.invalid)
			}
			if got.IsJobTest != tt.jobTest {
				t.Errorf("IsJobTest = %v, want %v", got.IsJobTest, tt.jobTest)
			}
			if got.Path != tt.path {
				t.Errorf("Path = %q, want %q", got.Path, tt.path)
			}
		})
	}
}

// TestIsFixtureFile guards the fixture walker against non-fixture files that
// happen to share the .yaml suffix.
//
// The motivating case is real: on a macOS checkout backed by exFAT or a network
// share, the OS writes AppleDouble sidecars named "._<original>" beside every
// file. Those end in ".yaml", so a suffix-only filter collects them as fixtures,
// they fail to parse, and the run reports spurious regressions that exist
// nowhere in the suite. No legitimate fixture name begins with a dot.
func TestIsFixtureFile(t *testing.T) {
	tests := []struct {
		name string
		file string
		want bool
	}{
		{"plain fixture", "1.1--minimal-job-template.yaml", true},
		{"invalid fixture", "2.1--missing-name.invalid.yaml", true},
		{"job execution fixture", "expr2.2.4--upper.test.yaml", true},
		{"appledouble sidecar", "._1.1--minimal-job-template.yaml", false},
		{"appledouble of an invalid fixture", "._2.1--missing-name.invalid.yaml", false},
		{"dotfile with yaml suffix", ".hidden.yaml", false},
		{"json sibling", "1.2--json-format-valid.json", false},
		{"no extension", "README", false},
		{"ds store", ".DS_Store", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := conformance.IsFixtureFile(tt.file); got != tt.want {
				t.Errorf("IsFixtureFile(%q) = %v, want %v", tt.file, got, tt.want)
			}
		})
	}
}
