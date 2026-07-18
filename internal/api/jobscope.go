// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Owner scoping for jobs. A principal without policy.JobsReadAll is
// "owner-scoped": it sees, and may mutate, only the jobs whose Owner matches
// its username. This file is the single place that decision is made for the
// REST surface — scopeFilter for collection routes, requireJobAccess for
// object routes.

import (
	"context"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/policy"
)

// scopeFilter reports whether the context principal is owner-scoped, and if so
// the owner value its queries must be pinned to.
//
// Returns ("", false) for a principal holding jobs.read.all — including the
// anonymous superuser injected when auth is disabled, which is what keeps
// auth-off behavior unchanged.
//
// A context carrying no principal fails closed: ("", true). No job has an
// owner equal to "", so a misordered middleware chain hides everything rather
// than exposing everything.
func scopeFilter(ctx context.Context) (owner string, scoped bool) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return "", true
	}
	if policy.Can(p, policy.JobsReadAll) {
		return "", false
	}
	return p.Username, true
}
