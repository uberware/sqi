// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Tests for exprlimits.go -- the one place in the worker binary that sees both
// the operator-facing expr: section and the enforcement type it configures.
//
// Two things can go wrong at a boundary like this and neither shows up as a
// compile error: the five same-typed fields can be TRANSPOSED, and the two
// packages' copies of the defaults and ranges can DRIFT apart (config would
// then accept a value the enforcement side considers out of range, or reject
// one it considers fine).

import (
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/fmtres"
)

// TestExprLimitsFromConfig passes five DISTINCT values, so a transposed pair
// fails rather than compiling and silently enforcing the wrong bound on the
// wrong dimension.
func TestExprLimitsFromConfig(t *testing.T) {
	got := exprLimitsFromConfig(workerconfig.ExprConfig{
		OperationLimit:          11,
		MemoryLimit:             22,
		AssignmentPositions:     33,
		AssignmentRetainedBytes: 44,
		LetRetainedBytes:        55,
	})
	want := fmtres.ExprLimits{
		OperationLimit:          11,
		MemoryLimit:             22,
		AssignmentPositions:     33,
		AssignmentRetainedBytes: 44,
		LetRetainedBytes:        55,
	}
	if got != want {
		t.Errorf("exprLimitsFromConfig = %+v, want %+v", got, want)
	}
}

// TestExprLimitsFromConfig_DefaultsAreTheEnforcementDefaults is the property
// that makes "a fresh install changes nothing" true end to end: the worker's
// built-in configuration must map to exactly the limits fmtres would have used
// with no configuration at all.
func TestExprLimitsFromConfig_DefaultsAreTheEnforcementDefaults(t *testing.T) {
	got := exprLimitsFromConfig(workerconfig.Default().Expr)
	if want := fmtres.DefaultExprLimits(); got != want {
		t.Errorf("the shipped worker defaults map to %+v, want fmtres.DefaultExprLimits() %+v", got, want)
	}
}

// TestExprLimitsBounds_MatchFmtres pins the two packages' copies of the
// defaults and the ranges against each other. internal/worker/config carries
// its own numbers on purpose (it must not import the enforcement leaf), and
// this is the only place that can see both -- exactly the role
// internal/server's TestExprLimits_ConfigMatchesOpenJD plays on the server.
//
// A drift here is not cosmetic: config would accept a value fmtres documents
// as out of range, or reject one it documents as fine, and the operator-facing
// message would name a bound nothing enforces.
func TestExprLimitsBounds_MatchFmtres(t *testing.T) {
	tests := []struct {
		name         string
		config, impl int64
	}{
		{"default operation_limit", workerconfig.DefaultWorkerExprOperationLimit, fmtres.DefaultExprLimits().OperationLimit},
		{"min operation_limit", workerconfig.MinWorkerExprOperationLimit, fmtres.MinExprOperationLimit},
		{"max operation_limit", workerconfig.MaxWorkerExprOperationLimit, fmtres.MaxExprOperationLimit},

		{"default memory_limit", workerconfig.DefaultWorkerExprMemoryLimit, fmtres.DefaultExprLimits().MemoryLimit},
		{"min memory_limit", workerconfig.MinWorkerExprMemoryLimit, fmtres.MinExprMemoryLimit},
		{"max memory_limit", workerconfig.MaxWorkerExprMemoryLimit, fmtres.MaxExprMemoryLimit},

		{
			"default assignment_positions",
			workerconfig.DefaultWorkerExprAssignmentPositions, fmtres.DefaultExprLimits().AssignmentPositions,
		},
		{"min assignment_positions", workerconfig.MinWorkerExprAssignmentPositions, fmtres.MinExprAssignmentPositions},
		{"max assignment_positions", workerconfig.MaxWorkerExprAssignmentPositions, fmtres.MaxExprAssignmentPositions},

		{
			"default assignment_retained_bytes",
			workerconfig.DefaultWorkerExprAssignmentRetainedBytes, fmtres.DefaultExprLimits().AssignmentRetainedBytes,
		},
		{
			"min assignment_retained_bytes",
			workerconfig.MinWorkerExprAssignmentRetainedBytes, fmtres.MinExprAssignmentRetainedBytes,
		},
		{
			"max assignment_retained_bytes",
			workerconfig.MaxWorkerExprAssignmentRetainedBytes, fmtres.MaxExprAssignmentRetainedBytes,
		},

		{
			"default let_retained_bytes",
			workerconfig.DefaultWorkerExprLetRetainedBytes, fmtres.DefaultExprLimits().LetRetainedBytes,
		},
		{"min let_retained_bytes", workerconfig.MinWorkerExprLetRetainedBytes, fmtres.MinExprLetRetainedBytes},
		{"max let_retained_bytes", workerconfig.MaxWorkerExprLetRetainedBytes, fmtres.MaxExprLetRetainedBytes},
	}
	for _, tc := range tests {
		if tc.config != tc.impl {
			t.Errorf("%s: internal/worker/config says %d, internal/worker/fmtres says %d",
				tc.name, tc.config, tc.impl)
		}
	}
}
