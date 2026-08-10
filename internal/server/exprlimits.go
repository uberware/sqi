// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/openjd"
)

// ExprLimitsFromConfig maps the operator's four openjd.expr_* settings onto
// the value internal/openjd actually enforces (EXPR sub-project E4d).
//
// It lives here, and is exported, for one reason: this package is the ONLY one
// that may see both sides. internal/config must not import internal/openjd
// (a cross-import between two same-level internal packages, and internal/openjd
// pulls in internal/store), so the mapping cannot live next to either
// definition -- and left inline in cmd/sqi-server's Config literal it would be
// four assignments between four same-typed int64 fields with nothing testing
// that they are paired correctly. Transposing two of them would compile,
// start, and quietly enforce the wrong bounds. TestExprLimitsFromConfig gives
// each field a distinct value precisely to catch that.
//
// The ranges these values must fall in are enforced by config.Validate at
// startup, not here: by the time this runs the configuration has already been
// accepted or the process has already exited.
func ExprLimitsFromConfig(c config.OpenJDConfig) openjd.ExprLimits {
	return openjd.ExprLimits{
		SubmissionOperations:  c.ExprOperationLimit,
		SubmissionMemoryBytes: c.ExprMemoryLimit,
		TemplatePositions:     c.ExprTemplatePositions,
		TemplateRetainedBytes: c.ExprTemplateRetainedBytes,
	}
}
