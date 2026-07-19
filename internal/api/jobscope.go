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

// ownerMatches reports whether a job owned by jobOwner is visible to the
// principal scoped to scopedOwner. It is the single owner-comparison rule for
// the REST surface: comparison is case-insensitive (matching the store's
// `COLLATE NOCASE`), and an empty jobOwner never matches — jobs submitted
// before auth was enabled carry no owner and must not become visible to
// whoever happens to have an empty username.
func ownerMatches(jobOwner, scopedOwner string) bool {
	return jobOwner != "" && strings.EqualFold(jobOwner, scopedOwner)
}

// requireJobAccessByJobID enforces owner scoping on a route whose {id} param
// is a job id. Attach it per-route with chi.With — never as a group-level Use,
// because the jobs.read/jobs.write groups also contain collection routes
// (GET /jobs, POST /jobs) that have no object to check.
func (a *authz) requireJobAccessByJobID() func(http.Handler) http.Handler {
	return a.requireJobAccess(a.jobFromJobID)
}

// requireJobAccessByTaskID enforces owner scoping on a route whose {id} param
// is a task id; the owning job is resolved through the task.
func (a *authz) requireJobAccessByTaskID() func(http.Handler) http.Handler {
	return a.requireJobAccess(a.jobFromTaskID)
}

// requireJobAccess builds the owner-scoping middleware around a resolver that
// maps the request to the job whose ownership governs it.
//
// The resolver is chosen per-route at mount time rather than inferred inside
// the middleware. An earlier version string-matched chi's accumulated route
// pattern for "/tasks/{id}" to decide whether {id} was a job or a task, which
// made the classification depend on a param name and path shape that nothing
// enforced: a route added with a differently-spelled task id would silently
// take the job branch and 404. Worse, that failure was invisible in testing —
// the middleware short-circuits for principals holding jobs.read.all, so an
// admin-run test suite never reaches the branch at all and only owner-scoped
// non-admins would hit it in production.
func (a *authz) requireJobAccess(resolve func(*http.Request) (store.Job, error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			owner, scoped := scopeFilter(r.Context())
			if !scoped {
				next.ServeHTTP(w, r)
				return
			}

			job, err := resolve(r)
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

			if ownerMatches(job.Owner, owner) {
				next.ServeHTTP(w, r)
				return
			}

			a.logger.WarnContext(
				r.Context(), "authz: job ownership denied",
				slog.String("job_id", job.ID),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)
			writeProblem(w, r, http.StatusForbidden, "forbidden")
		})
	}
}

// jobFromJobID loads the job named directly by the {id} URL param.
func (a *authz) jobFromJobID(r *http.Request) (store.Job, error) {
	return a.store.GetJob(r.Context(), chi.URLParam(r, "id"))
}

// jobFromTaskID loads the job owning the task named by the {id} URL param.
func (a *authz) jobFromTaskID(r *http.Request) (store.Job, error) {
	task, err := a.store.GetTask(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		return store.Job{}, err
	}
	return a.store.GetJob(r.Context(), task.JobID)
}
