// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/fmtres"
)

// exprLimitsFromConfig maps the worker's operator-facing expr: section
// (workerconfig.ExprConfig) to the enforcement type the session manager and
// every phase-3 evaluator use (fmtres.ExprLimits).
//
// It is the one place in the worker binary that sees both types.
// internal/worker/config is the operator-facing layer and must not import the
// enforcement leaf; fmtres must not import configuration. That is the same
// split internal/config / internal/openjd / internal/server keep on the server
// side, with cmd/sqi-worker standing in for internal/server as the boot
// orchestrator.
//
// Five same-typed int64 fields copied across a struct boundary is the shape
// where a transposition compiles, boots, and enforces the wrong bound on the
// wrong dimension, so TestExprLimitsFromConfig passes five distinct values,
// and TestExprLimitsBounds_MatchFmtres pins the two packages' copies of the
// ranges (and defaults) against each other.
func exprLimitsFromConfig(cfg workerconfig.ExprConfig) fmtres.ExprLimits {
	return fmtres.ExprLimits{
		OperationLimit:          cfg.OperationLimit,
		MemoryLimit:             cfg.MemoryLimit,
		AssignmentPositions:     cfg.AssignmentPositions,
		AssignmentRetainedBytes: cfg.AssignmentRetainedBytes,
		LetRetainedBytes:        cfg.LetRetainedBytes,
	}
}
