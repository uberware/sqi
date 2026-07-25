// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance_test

import (
	"testing"

	"github.com/uberware/sqi/test/conformance"
)

func TestExtensionFor(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"base job template", "base/job_templates/1.1--minimal.yaml", "base"},
		{"base env template", "base/env_templates/7.1--env.yaml", "base"},
		{"expr", "EXPR/job_templates/expr1.1--arith.yaml", "EXPR"},
		{"task chunking", "TASK_CHUNKING/jobs/contiguous-even.test.yaml", "TASK_CHUNKING"},
		{"wrap actions", "WRAP_ACTIONS/env_templates/1--wrap.yaml", "WRAP_ACTIONS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := conformance.ExtensionFor(tt.path); got != tt.want {
				t.Errorf("ExtensionFor(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		ext  string
		want conformance.State
	}{
		{"base is always live", "base", conformance.StateLive},
		{"registered official extension", "TASK_CHUNKING", conformance.StateLive},
		{"registered official extension 2", "REDACTED_ENV_VARS", conformance.StateLive},
		{"unregistered extension", "EXPR", conformance.StateNotApplicable},
		{"unregistered extension 2", "WRAP_ACTIONS", conformance.StateNotApplicable},
		{"unregistered extension 3", "FEATURE_BUNDLE_1", conformance.StateNotApplicable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := conformance.Classify(tt.ext); got != tt.want {
				t.Errorf("Classify(%q) = %v, want %v", tt.ext, got, tt.want)
			}
		})
	}
}
