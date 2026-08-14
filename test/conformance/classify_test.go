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

func TestKindFor(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"base job template", "base/job_templates/1.1--minimal.yaml", "job_templates"},
		{"base env template", "base/env_templates/7.1--env.yaml", "env_templates"},
		{"expr job template", "EXPR/job_templates/expr1.1--arith.yaml", "job_templates"},
		{"wrap actions env template", "WRAP_ACTIONS/env_templates/1--wrap.yaml", "env_templates"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := conformance.KindFor(tt.path); got != tt.want {
				t.Errorf("KindFor(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		ext  string
		kind string
		want conformance.State
	}{
		{"base job template is live", "base", "job_templates", conformance.StateLive},
		{"registered official extension job template", "TASK_CHUNKING", "job_templates", conformance.StateLive},
		{"registered official extension 2 job template", "REDACTED_ENV_VARS", "job_templates", conformance.StateLive},
		{"EXPR job template is live since sub-project H2 marked it supported", "EXPR", "job_templates", conformance.StateLive},
		{"unregistered extension job template", "WRAP_ACTIONS", "job_templates", conformance.StateNotApplicable},
		{"unregistered extension 2 job template", "FEATURE_BUNDLE_1", "job_templates", conformance.StateNotApplicable},
		{"base env template is not applicable", "base", "env_templates", conformance.StateNotApplicable},
		{"registered extension env template is still not applicable", "TASK_CHUNKING", "env_templates", conformance.StateNotApplicable},
		// EXPR is a SUPPORTED extension and its job_templates row above is
		// live, so this row is the kind gate alone, not the extension gate.
		// Its six fixtures are "specificationVersion: environment-2023-09"
		// documents, a top-level format sqi does not implement at all: measured,
		// each is rejected on `/specificationVersion: unsupported version
		// "environment-2023-09"; expected "jobtemplate-2023-09"` plus follow-on
		// required-field errors, never on the EXPR defect it encodes. Scoring
		// them would report five ".invalid" fixtures as passes for the wrong
		// reason. This row stays not-applicable until sqi implements the
		// environment document type, which is not an EXPR question.
		{"EXPR env template is not applicable: sqi implements no standalone environment templates", "EXPR", "env_templates", conformance.StateNotApplicable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := conformance.Classify(tt.ext, tt.kind); got != tt.want {
				t.Errorf("Classify(%q, %q) = %v, want %v", tt.ext, tt.kind, got, tt.want)
			}
		})
	}
}
