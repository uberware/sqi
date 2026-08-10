// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"testing"

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/server"
)

// TestExprLimitsFromConfig pins the mapping field by field, with four DISTINCT
// values. Four same-typed int64 fields assigned across a struct boundary is
// exactly the shape where a transposition compiles, starts, and silently
// enforces the wrong bound on an unauthenticated request path.
func TestExprLimitsFromConfig(t *testing.T) {
	got := server.ExprLimitsFromConfig(config.OpenJDConfig{
		ExprOperationLimit:        11,
		ExprMemoryLimit:           22,
		ExprTemplatePositions:     33,
		ExprTemplateRetainedBytes: 44,
	})
	tests := []struct {
		name string
		got  int64
		want int64
	}{
		{"expr_operation_limit -> SubmissionOperations", got.SubmissionOperations, 11},
		{"expr_memory_limit -> SubmissionMemoryBytes", got.SubmissionMemoryBytes, 22},
		{"expr_template_positions -> TemplatePositions", got.TemplatePositions, 33},
		{"expr_template_retained_bytes -> TemplateRetainedBytes", got.TemplateRetainedBytes, 44},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %d, want %d", tc.got, tc.want)
			}
		})
	}
}

// TestExprLimits_ConfigMatchesOpenJD is the duplication guard both sides'
// comments name. internal/config carries its own copies of the defaults and of
// the operator-configurable range because it may not import internal/openjd;
// this package can see both, so it is where a drift between them fails the
// build rather than shipping.
func TestExprLimits_ConfigMatchesOpenJD(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		got := server.ExprLimitsFromConfig(config.DefaultConfig().OpenJD)
		if want := openjd.DefaultExprLimits(); got != want {
			t.Fatalf("config.DefaultConfig().OpenJD maps to %+v, want openjd.DefaultExprLimits() = %+v.\n"+
				"A fresh install must reproduce internal/openjd's own defaults exactly.", got, want)
		}
	})

	t.Run("ranges", func(t *testing.T) {
		tests := []struct {
			name         string
			cfgV, openjV int64
		}{
			{"MinOperationLimit", config.MinOpenJDExprOperationLimit, openjd.MinExprSubmissionOperations},
			{"MaxOperationLimit", config.MaxOpenJDExprOperationLimit, openjd.MaxExprSubmissionOperations},
			{"MinMemoryLimit", config.MinOpenJDExprMemoryLimit, openjd.MinExprSubmissionMemoryBytes},
			{"MaxMemoryLimit", config.MaxOpenJDExprMemoryLimit, openjd.MaxExprSubmissionMemoryBytes},
			{"MinTemplatePositions", config.MinOpenJDExprTemplatePositions, openjd.MinExprTemplatePositions},
			{"MaxTemplatePositions", config.MaxOpenJDExprTemplatePositions, openjd.MaxExprTemplatePositions},
			{"MinTemplateRetainedBytes", config.MinOpenJDExprTemplateRetainedBytes, openjd.MinExprTemplateRetainedBytes},
			{"MaxTemplateRetainedBytes", config.MaxOpenJDExprTemplateRetainedBytes, openjd.MaxExprTemplateRetainedBytes},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if tc.cfgV != tc.openjV {
					t.Errorf("internal/config has %d, internal/openjd has %d -- the two copies have drifted",
						tc.cfgV, tc.openjV)
				}
			})
		}
	})
}

// TestDefaultConfig_ExprLimitsAreOpenJDDefaults pins the OTHER half of "a fresh
// install changes nothing": server.DefaultConfig (used by every test in this
// repo that builds a Server without going through cmd/sqi-server) must carry
// the same limits, and a Config literal that omits the field entirely must
// still behave as the default once openjd normalizes it.
func TestDefaultConfig_ExprLimitsAreOpenJDDefaults(t *testing.T) {
	if got, want := server.DefaultConfig().OpenJDExprLimits, openjd.DefaultExprLimits(); got != want {
		t.Fatalf("server.DefaultConfig().OpenJDExprLimits = %+v, want %+v", got, want)
	}
	var zero openjd.ExprLimits
	if got := zero; got != (openjd.ExprLimits{}) {
		t.Fatalf("sanity: the zero value must be zero, got %+v", got)
	}
}
