// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Owner scoping for jobs. A principal without policy.JobsReadAll is
// "owner-scoped": it sees, and may mutate, only the jobs whose Owner matches
// its username. This file is the single place that decision is made for the
// REST surface — scopeFilter for collection routes, requireJobAccess for
// object routes.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/policy"
	"github.com/uberware/sqi/internal/store"
)

// scopeFilter reports whether the context principal is owner-scoped, and if so
// the owner value its queries must be pinned to.
//
// Returns ("", false) for a principal holding jobs.read.all — including the
// anonymous superuser injected when auth is disabled, which is what keeps
// auth-off behavior unchanged.
//
// A context carrying no principal fails closed: ("", true). Note that ""
// is also store.ListJobsOptions.Owner's zero value for "unfiltered" —
// callers MUST NOT assign this owner straight into ListJobsOptions.Owner;
// doing so would silently turn the fail-closed signal into "return
// everything". Callers must special-case owner == "" with scoped == true
// and short-circuit to an empty result instead of querying the store.
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

// requireJobAccess returns middleware enforcing owner scoping on a single job
// or task. Attach it per-route with chi.With — never as a group-level Use,
// because the existing jobs.read/jobs.write groups also contain collection
// routes (GET /jobs, POST /jobs) that have no object to check.
//
// The chi URL param "id" is a job id on /jobs/{id}... routes and a task id on
// /tasks/{id}... routes; the route pattern distinguishes them.
func (a *authz) requireJobAccess() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			owner, scoped := scopeFilter(r.Context())
			if !scoped {
				next.ServeHTTP(w, r)
				return
			}

			job, err := a.resolveJob(r)
			if errors.Is(err, store.ErrNotFound) {
				writeProblem(w, r, http.StatusNotFound, "not found")
				return
			}
			if err != nil {
				a.logger.ErrorContext(r.Context(), "authz: job access lookup failed",
					slog.Any("error", err))
				writeProblem(w, r, http.StatusInternalServerError, "failed to resolve job")
				return
			}

			if strings.EqualFold(job.Owner, owner) && job.Owner != "" {
				next.ServeHTTP(w, r)
				return
			}

			a.logger.WarnContext(r.Context(), "authz: job ownership denied",
				slog.String("job_id", job.ID),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)
			writeProblem(w, r, http.StatusForbidden, "forbidden")
		})
	}
}

// resolveJob loads the job addressed by the request: directly for /jobs/{id}
// routes, via the task's JobID for /tasks/{id} routes.
//
// Routes are mounted under r.Route("/api/v1", …) (router.go), so chi's
// RoutePattern() returns the accumulated pattern, e.g. "/api/v1/tasks/{id}"
// — never the bare "/tasks/{id}" a naive prefix check would expect. Matching
// on "/tasks/{id}" (the object segment, not a path prefix) correctly
// classifies /api/v1/tasks/{id} and its /logs, /attempts, /retry, /cancel
// children as task routes, while /api/v1/jobs/{id}/tasks — the only
// near-collision in the router — does NOT match: it has no "{id}" segment
// after "tasks", so it falls through to the job branch as intended.
func (a *authz) resolveJob(r *http.Request) (store.Job, error) {
	id := chi.URLParam(r, "id")
	pattern := chi.RouteContext(r.Context()).RoutePattern()
	if strings.Contains(pattern, "/tasks/{id}") {
		task, err := a.store.GetTask(r.Context(), id)
		if err != nil {
			return store.Job{}, err
		}
		id = task.JobID
	}
	return a.store.GetJob(r.Context(), id)
}
