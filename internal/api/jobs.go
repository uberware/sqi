// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Job REST handlers.
//
// Route summary:
//
//	POST /api/v1/jobs — submit an OpenJD job
//	GET /api/v1/jobs — list jobs with pagination + filters
//	GET /api/v1/jobs/{id} — job detail with steps and task counts
//	PATCH /api/v1/jobs/{id} — priority, queue move, pause/resume
//	POST /api/v1/jobs/{id}/cancel — cancel job
//	POST /api/v1/jobs/{id}/retry — retry failed/canceled tasks
//	DELETE /api/v1/jobs/{id} — delete job and all data

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/ws"
)

// jobCanceler is the subset of [scheduler.Scheduler] used by the job handler.
// Keeping it as an interface makes the handler testable without a live NATS instance.
type jobCanceler interface {
	CancelJob(ctx context.Context, jobID string) error
	RetryJob(ctx context.Context, jobID string) (int, error)
	// WakeQueue wakes parked lease waiters on queueID so a newly-submitted
	// job's ready tasks are leased without waiting out the long-poll hold.
	WakeQueue(queueID string)
	// ReconcileDependents cancels or unblocks jobs waiting on upstreamJobID,
	// used to react immediately when the upstream is deleted rather than
	// waiting for the periodic sweep.
	ReconcileDependents(ctx context.Context, upstreamJobID string) error
}

// JobSubmitter is the subset of [openjd.Submitter] the submission handlers
// use. Keeping it an interface — the same reason [jobCanceler] is one, and the
// same reason [Deps.Store] is store.Store rather than *sqlite.Store — lets the
// handlers' error mapping be tested without driving the whole OpenJD pipeline.
// That mattered most for the one error the pipeline could not produce: while
// the EXPR extension was StatusInProgress an EXPR template was rejected before
// any expression was evaluated, so a wall-clock deadline breach was unreachable
// end to end. Sub-project H2 flipped the status and the real path is now
// covered directly (submitdeadline_test.go's end-to-end cases); the stub
// remains the cheap way to pin the status mapping alone.
//
// It is EXPORTED, and [Deps.Submitter] is typed as it rather than as
// *openjd.Submitter, so that a test can drive a router built by [NewRouter]
// with a recording stub. Without that, nothing could observe the
// Config → handler hop that carries the submission deadline: the value would
// reach a real Submitter and vanish into a pipeline that cannot currently
// spend it.
type JobSubmitter interface {
	Submit(
		ctx context.Context, rawTemplate string, format store.TemplateFormat, opts openjd.SubmitOptions,
	) (*openjd.SubmitResult, error)
}

// jobHandler handles all job-related REST endpoints.
type jobHandler struct {
	store     store.Store
	submitter JobSubmitter
	sched     jobCanceler
	// notifier pushes a removed event when a job is deleted so other connected
	// clients drop it live. May be nil (no push).
	notifier ws.Notifier
	logger   *slog.Logger
	// retryDefaults is the server-level fallback retry policy, used to report
	// the resolved effective_retry in job detail responses. The zero value is
	// tolerated (resolution clamps max attempts up to 1).
	retryDefaults scheduler.RetryPolicy
	// ownerLookup validates a submit-as owner override against known users.
	// Nil disables validation (auth.validate_job_owner = false).
	ownerLookup ownerLookup
	// exprDeadline is how long the OpenJD expression checker may work on ONE
	// submission before this server gives up on it
	// (config openjd.expr_submission_deadline). Zero disables the backstop.
	//
	// Stored as a DURATION and turned into an absolute instant per request by
	// [exprDeadlineAt]; see that function for why the conversion cannot be
	// hoisted anywhere that runs once.
	exprDeadline time.Duration
}

// ── Wire-format types ─────────────────────────────────────────────────────────

// jobResponse is the JSON representation of a job returned by the list and
// create endpoints.
type jobResponse struct {
	ID             string     `json:"id"`
	FarmID         string     `json:"farm_id"`
	QueueID        string     `json:"queue_id"`
	QueueName      string     `json:"queue_name,omitempty"`
	Name           string     `json:"name"`
	Owner          string     `json:"owner"`
	Submitter      string     `json:"submitter"`
	Priority       int        `json:"priority"`
	Status         string     `json:"status"`
	DependsOn      []string   `json:"depends_on,omitempty"`
	Project        string     `json:"project,omitempty"`
	TemplateFormat string     `json:"template_format"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	// FailedAttempts is the job's cumulative count of genuine task failures.
	FailedAttempts int `json:"failed_attempts"`
	// ParkReason is set when the failure-limit sweep auto-parked the job.
	ParkReason string `json:"park_reason,omitempty"`
	// MaxAttempts, RetryDelaySeconds, and FailureLimit are the configured
	// per-job retry-policy overrides. Nil means inherit (queue -> farm ->
	// server default).
	MaxAttempts       *int `json:"max_attempts,omitempty"`
	RetryDelaySeconds *int `json:"retry_delay_seconds,omitempty"`
	FailureLimit      *int `json:"failure_limit,omitempty"`
}

// stepResponse is the JSON representation of a step within a job detail response.
type stepResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	StepOrder int       `json:"step_order"`
	Status    string    `json:"status"`
	DependsOn []string  `json:"depends_on,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// taskCountsResponse is the per-status task count summary included in job detail.
type taskCountsResponse struct {
	Total     int `json:"total"`
	Pending   int `json:"pending"`
	Ready     int `json:"ready"`
	Assigned  int `json:"assigned"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Canceled  int `json:"canceled"`
	// Unschedulable is the number of Ready tasks that currently carry a
	// non-empty unschedulable reason (a subset of Ready, not an additional
	// status).
	Unschedulable int `json:"unschedulable"`
}

// jobDetailResponse is returned by GET /api/v1/jobs/{id}.
type jobDetailResponse struct {
	jobResponse

	Steps      []stepResponse     `json:"steps"`
	TaskCounts taskCountsResponse `json:"task_counts"`
	// FailureSummary aggregates the job's failed tasks by failure reason. Nil
	// when the job has no failed tasks with a recorded reason.
	FailureSummary *failureSummaryResponse `json:"failure_summary,omitempty"`
	// EffectiveRetry is the RESOLVED retry policy the job runs under, after
	// job -> queue -> farm -> server-default inheritance. Omitted only when
	// the queue or farm lookup fails.
	EffectiveRetry *effectiveRetryResponse `json:"effective_retry,omitempty"`
}

// effectiveRetryResponse is the resolved (non-nullable) retry policy included
// in job detail. FailureLimit 0 means the auto-park failure limit is off.
type effectiveRetryResponse struct {
	MaxAttempts       int `json:"max_attempts"`
	RetryDelaySeconds int `json:"retry_delay_seconds"`
	FailureLimit      int `json:"failure_limit"`
}

// failureSummaryResponse is the JSON representation of [store.FailureSummary],
// included in job detail when the job has failed tasks with recorded reasons.
type failureSummaryResponse struct {
	FailedCount     int    `json:"failed_count"`
	DominantReason  string `json:"dominant_reason,omitempty"`
	DistinctReasons int    `json:"distinct_reasons"`
}

// jobListItemResponse is a single entry in the job list. It carries the base
// job fields plus the aggregate task counts used to render progress without a
// per-row detail fetch.
type jobListItemResponse struct {
	jobResponse

	TaskCounts taskCountsResponse `json:"task_counts"`
}

// jobListResponse is the paginated result returned by GET /api/v1/jobs.
type jobListResponse struct {
	Items  []jobListItemResponse `json:"items"`
	Total  int                   `json:"total"`
	Limit  int                   `json:"limit"`
	Offset int                   `json:"offset"`
}

// patchJobRequest is the body accepted by PATCH /api/v1/jobs/{id}.
// Fields are optional; only non-zero/non-nil fields are applied.
type patchJobRequest struct {
	// Priority sets the job priority when non-nil. Values ≤ 0 are ignored.
	Priority *int `json:"priority,omitempty"`
	// QueueID moves the job to a different queue when non-empty.
	QueueID string `json:"queue_id,omitempty"`
	// Action must be "pause" or "resume" when non-empty.
	Action string `json:"action,omitempty"`
	// MaxAttempts, RetryDelaySeconds, and FailureLimit update the job's retry
	// policy overrides when non-nil.
	MaxAttempts       *int `json:"max_attempts,omitempty"`
	RetryDelaySeconds *int `json:"retry_delay_seconds,omitempty"`
	FailureLimit      *int `json:"failure_limit,omitempty"`
}

// ── Handler constructors ──────────────────────────────────────────────────────

// newJobHandler returns a jobHandler wired to the given store, submitter,
// scheduler, and optional notifier. validateOwner controls whether a submit-as
// owner override is checked against known users (config.AuthConfig.ValidateJobOwner).
// exprDeadline is the configured wall-clock allowance for one submission's
// expression evaluation; zero disables the backstop.
func newJobHandler(
	st store.Store,
	sub JobSubmitter,
	sched jobCanceler,
	notifier ws.Notifier,
	logger *slog.Logger,
	retryDefaults scheduler.RetryPolicy,
	validateOwner bool,
	exprDeadline time.Duration,
) *jobHandler {
	return &jobHandler{
		store:         st,
		submitter:     sub,
		sched:         sched,
		notifier:      notifier,
		logger:        logger,
		retryDefaults: retryDefaults,
		ownerLookup:   newOwnerLookup(st, validateOwner),
		exprDeadline:  exprDeadline,
	}
}

// ── POST /api/v1/jobs ─────────────────────────────────────────────────────────

// submitJob accepts a raw OpenJD template (YAML or JSON) and creates the job.
//
// The Content-Type header determines the template format:
//   - application/yaml, application/x-yaml, text/yaml → YAML
//   - anything else (or absent) → JSON
//
// Query parameters: farm_id (required), queue_id (required), owner,
// submitter, priority (integer ≥ 1; defaults to 50), project, max_attempts,
// retry_delay_seconds, failure_limit (all optional per-job retry-policy
// overrides; omitted or unparseable means inherit queue -> farm -> server
// default).
// Job-parameter values are supplied as param.<Name>=<value> query parameters
// (e.g. ?param.FrameStart=1&param.Quality=high).
func (h *jobHandler) submitJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	farmID := r.URL.Query().Get("farm_id")
	queueID := r.URL.Query().Get("queue_id")
	if farmID == "" || queueID == "" {
		writeProblem(w, r, http.StatusBadRequest, "missing required query parameters: farm_id, queue_id")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4 MiB cap
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "failed to read request body")
		return
	}

	format, storeFormat := detectFormat(r.Header.Get("Content-Type"))

	var priority int
	if p := r.URL.Query().Get("priority"); p != "" {
		if n, convErr := strconv.Atoi(p); convErr == nil {
			priority = n
		}
	}

	maxAttempts, retryDelaySeconds, failureLimit, problem := parseRetryOverridesQuery(r.URL.Query())
	if problem != "" {
		writeProblem(w, r, http.StatusBadRequest, problem)
		return
	}

	owner, submitter, identityProblem, identityStatus := bindSubmitIdentity(
		ctx, h.ownerLookup, r.URL.Query().Get("owner"), r.URL.Query().Get("submitter"),
	)
	if identityStatus != 0 {
		writeProblem(w, r, identityStatus, identityProblem)
		return
	}

	opts := openjd.SubmitOptions{
		FarmID:            farmID,
		QueueID:           queueID,
		Owner:             owner,
		Submitter:         submitter,
		Priority:          priority,
		Project:           r.URL.Query().Get("project"),
		Parameters:        parseParamQueryParams(r.URL.Query()),
		MaxAttempts:       maxAttempts,
		RetryDelaySeconds: retryDelaySeconds,
		FailureLimit:      failureLimit,
		DependsOn:         r.URL.Query()["depends_on"],
		Deadline:          exprDeadlineAt(h.exprDeadline),
	}

	result, err := h.submitter.Submit(ctx, string(body), storeFormat, opts)
	if err != nil {
		// The wall-clock backstop FIRST, and as a 503. It is checked ahead of
		// the validation branch because the two claims are different: 422 says
		// the template is wrong and retrying is pointless, while this says the
		// server gave up on a template that might be perfectly valid — the
		// same body would validate on an idle host. Reporting a load-dependent
		// outcome as the submitter's fault would make acceptance depend on
		// machine load, which no client can reason about or retry sensibly.
		if writeExprDeadlineProblem(w, r, h.logger, err,
			"jobs: submit exceeded its expression deadline", exprDeadlineProblemDetail, h.exprDeadline,
			slog.String("farm_id", farmID), slog.String("queue_id", queueID)) {
			return
		}
		// Distinguish parse/validation errors (client fault) from storage errors.
		if isSubmitValidationError(err) {
			writeProblem(w, r, http.StatusUnprocessableEntity, err.Error())
			return
		}
		h.logger.ErrorContext(
			ctx, "jobs: submit failed",
			slog.String("farm_id", farmID),
			slog.String("queue_id", queueID),
			slog.String("format", string(format)),
			slog.Any("error", err),
		)
		writeProblem(w, r, http.StatusInternalServerError, "failed to create job")
		return
	}

	// Wake any workers parked on this queue so the new ready tasks are leased
	// promptly rather than after the long-poll hold elapses.
	h.sched.WakeQueue(result.Job.QueueID)

	respJob := refetchDependsOn(ctx, h.store, h.logger, result.Job, opts.DependsOn, "jobs")

	writeJSON(w, http.StatusCreated, toJobResponse(respJob))
}

// refetchDependsOn re-fetches job by ID when requested is non-empty, so a
// submit response echoes the persisted DependsOn edges: openjd.Submitter's
// returned job never carries DependsOn (only [store.Store.GetJob] populates
// it, via a join over job_dependencies). Best-effort: on failure it logs
// under component (e.g. "jobs" or "products", matching each handler's other
// log lines) and falls back to the unpopulated job rather than failing the
// whole request. Shared by submitJob and productHandler.submitProductJob.
func refetchDependsOn(
	ctx context.Context, st store.Store, logger *slog.Logger, job store.Job, requested []string, component string,
) store.Job {
	if len(requested) == 0 {
		return job
	}
	full, err := st.GetJob(ctx, job.ID)
	if err != nil {
		logger.ErrorContext(ctx, component+": refetch after submit for depends_on failed",
			slog.String("job_id", job.ID), slog.Any("error", err))
		return job
	}
	return full
}

// ── GET /api/v1/jobs ──────────────────────────────────────────────────────────

// listJobs returns a paginated, filtered, and sorted list of jobs.
//
// Query parameters:
//   - status    — filter by [store.JobStatus]
//   - owner     — filter by owner string
//   - farm_id   — filter by farm ID
//   - queue_id  — filter by queue ID
//   - project   — filter by project label
//   - search    — case-insensitive substring over name, id, owner, project
//   - sort_by   — created_at | priority | status | updated_at | name  (default: created_at)
//   - sort_dir  — asc | desc  (default: asc)
//   - limit     — page size, 1–1000  (default: 50)
//   - offset    — zero-based item offset  (default: 0)
func (h *jobHandler) listJobs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	pg := store.Pagination{
		Limit:  parseIntQuery(q.Get("limit"), store.DefaultLimit),
		Offset: parseIntQuery(q.Get("offset"), 0),
	}
	pg.Validate() //nolint:errcheck // Validate only clamps; never errors

	opts := store.ListJobsOptions{
		FarmID:  q.Get("farm_id"),
		QueueID: q.Get("queue_id"),
		Status:  store.JobStatus(q.Get("status")),
		Owner:   q.Get("owner"),
		Project: q.Get("project"),
		Search:  q.Get("search"),
		SortBy:  toJobSortField(q.Get("sort_by")),
		SortDir: toSortDir(q.Get("sort_dir")),

		Pagination: pg,
	}

	// Owner scoping: a principal without jobs.read.all may only see its own
	// jobs. The forced value OVERRIDES any client-supplied ?owner= rather than
	// merging with it — a scoped caller aiming the filter at another user gets
	// its own jobs, not an error and not the other user's.
	owner, scoped := scopeFilter(ctx)
	if scoped {
		opts.Owner = owner
	}

	// A scoped request with an empty owner means scopeFilter failed closed
	// (no principal in the context at all — see jobscope.go). opts.Owner == ""
	// is store.ListJobsOptions' zero value for "unfiltered", so passing it
	// through to ListJobs would return every job in the system instead of
	// none. Short-circuit to an empty page rather than querying.
	var page store.Page[store.Job]
	if scoped && owner == "" {
		page = store.Page[store.Job]{
			Items:  []store.Job{},
			Total:  0,
			Limit:  opts.Pagination.Limit,
			Offset: opts.Pagination.Offset,
		}
	} else {
		var err error
		page, err = h.store.ListJobs(ctx, opts)
		if err != nil {
			h.logger.ErrorContext(ctx, "jobs: list failed", slog.Any("error", err))
			writeProblem(w, r, http.StatusInternalServerError, "failed to list jobs")
			return
		}
	}

	queueNames := resolveQueueNames(ctx, h.store, page.Items)

	resp := jobListResponse{
		Items:  make([]jobListItemResponse, len(page.Items)),
		Total:  page.Total,
		Limit:  page.Limit,
		Offset: page.Offset,
	}
	for i, j := range page.Items {
		r := toJobResponse(j)
		r.QueueName = queueNames[j.QueueID]
		item := jobListItemResponse{jobResponse: r}
		// Per-job count queries, mirroring resolveQueueNames' per-row
		// enrichment. This is N additional queries per list call (one pair
		// per job, matching the pre-existing CountTasksByJob pattern rather
		// than introducing a new N+1). A count failure degrades to zero
		// counts rather than failing the list.
		if counts, err := h.store.CountTasksByJob(ctx, j.ID); err == nil {
			unschedulable, uErr := h.store.CountUnschedulableTasksByJob(ctx, j.ID)
			if uErr != nil {
				unschedulable = 0
			}
			item.TaskCounts = toTaskCountsResponse(counts, unschedulable)
		}
		resp.Items[i] = item
	}

	writeJSON(w, http.StatusOK, resp)
}

// ── GET /api/v1/jobs/{id} ─────────────────────────────────────────────────────

// getJob returns the job with its steps and aggregate task counts by status.
func (h *jobHandler) getJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	job, err := h.store.GetJob(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "job not found")
			return
		}
		h.logger.ErrorContext(ctx, "jobs: get failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve job")
		return
	}

	steps, err := h.store.ListSteps(ctx, id)
	if err != nil {
		h.logger.ErrorContext(ctx, "jobs: list steps failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve job steps")
		return
	}

	taskCounts, err := h.store.CountTasksByJob(ctx, id)
	if err != nil {
		h.logger.ErrorContext(ctx, "jobs: count tasks failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve task counts")
		return
	}

	unschedulableCount, err := h.store.CountUnschedulableTasksByJob(ctx, id)
	if err != nil {
		h.logger.ErrorContext(ctx, "jobs: count unschedulable tasks failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve task counts")
		return
	}

	stepResps := make([]stepResponse, len(steps))
	for i, s := range steps {
		stepResps[i] = toStepResponse(s)
	}

	jr := toJobResponse(job)
	queue, queueErr := h.store.GetQueue(ctx, job.QueueID)
	if queueErr == nil {
		jr.QueueName = queue.Name
	}

	resp := jobDetailResponse{
		jobResponse: jr,
		Steps:       stepResps,
		TaskCounts:  toTaskCountsResponse(taskCounts, unschedulableCount),
	}

	// Resolved retry policy (job -> queue -> farm -> server default). Best
	// effort like QueueName: omitted if the queue or farm lookup fails.
	if farm, err := h.store.GetFarm(ctx, job.FarmID); err == nil && queueErr == nil {
		policy := scheduler.ResolveRetryPolicy(job, queue, farm, h.retryDefaults)
		resp.EffectiveRetry = &effectiveRetryResponse{
			MaxAttempts:       policy.MaxAttempts,
			RetryDelaySeconds: int(policy.RetryDelay / time.Second),
			FailureLimit:      policy.FailureLimit,
		}
	}

	// Only worth a query when the job has failed tasks — this endpoint is
	// refetched on every WS-driven invalidation and most jobs have none.
	if taskCounts[store.TaskStatusFailed] > 0 {
		if summary, err := h.store.FailureReasonSummary(ctx, id); err == nil && summary.FailedCount > 0 {
			resp.FailureSummary = &failureSummaryResponse{
				FailedCount:     summary.FailedCount,
				DominantReason:  summary.DominantReason,
				DistinctReasons: summary.DistinctReasons,
			}
		} else if err != nil {
			h.logger.WarnContext(ctx, "jobs: failure reason summary failed", slog.String("id", id), slog.Any("error", err))
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// ── PATCH /api/v1/jobs/{id} ───────────────────────────────────────────────────

// patchJob supports priority changes, queue moves, and pause/resume.
// The request body is a JSON object; only present fields are applied.
func (h *jobHandler) patchJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	var req patchJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	job, err := h.store.GetJob(ctx, id)
	if err != nil {
		handleStoreError(w, r, err, h.logger, "jobs: patch get failed", id)
		return
	}

	if job.Status.IsTerminal() {
		writeProblem(w, r, http.StatusConflict, "job is in a terminal state and cannot be modified")
		return
	}

	if req.Priority != nil && *req.Priority > 0 {
		job.Priority = *req.Priority
	}

	if req.QueueID != "" {
		if !h.validateQueueMove(w, r, req.QueueID) {
			return
		}
		job.QueueID = req.QueueID
	}

	if !applyRetryOverridePatch(w, r, &req, &job) {
		return
	}

	newStatus, ok := h.resolveAction(w, r, job.Status, req.Action)
	if !ok {
		return
	}

	updated, err := h.store.UpdateJob(ctx, job)
	if err != nil {
		handleStoreError(w, r, err, h.logger, "jobs: patch update failed", id)
		return
	}

	if newStatus != "" {
		updated, err = h.applyStatusChange(w, r, id, req.Action, newStatus, updated)
		if err != nil {
			return
		}
	}

	writeJSON(w, http.StatusOK, toJobResponse(updated))
}

// validateQueueMove checks that queueID exists and writes an error response if
// it does not. It returns true when the queue is valid.
func (h *jobHandler) validateQueueMove(w http.ResponseWriter, r *http.Request, queueID string) bool {
	_, err := h.store.GetQueue(r.Context(), queueID)
	if err == nil {
		return true
	}
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, r, http.StatusBadRequest, "target queue_id does not exist")
		return false
	}
	h.logger.ErrorContext(
		r.Context(), "jobs: patch get queue failed",
		slog.String("queue_id", queueID),
		slog.Any("error", err),
	)
	writeProblem(w, r, http.StatusInternalServerError, "failed to validate queue")
	return false
}

// applyRetryOverridePatch validates the PATCH request's optional retry
// override fields and applies the present ones to job. On an out-of-range
// value it writes the 400 response and reports false. Extracted from patchJob
// to keep its cyclomatic complexity in check.
func applyRetryOverridePatch(w http.ResponseWriter, r *http.Request, req *patchJobRequest, job *store.Job) bool {
	if problem := validateRetryOverrides(req.MaxAttempts, req.RetryDelaySeconds, req.FailureLimit); problem != "" {
		writeProblem(w, r, http.StatusBadRequest, problem)
		return false
	}
	if req.MaxAttempts != nil {
		job.MaxAttempts = req.MaxAttempts
	}
	if req.RetryDelaySeconds != nil {
		job.RetryDelaySeconds = req.RetryDelaySeconds
	}
	if req.FailureLimit != nil {
		job.FailureLimit = req.FailureLimit
	}
	return true
}

// resolveAction maps a pause/resume action string to the target [store.JobStatus].
// It writes an error response and returns ("", false) when the action or current
// status is invalid. An empty action is valid and returns ("", true).
func (*jobHandler) resolveAction(
	w http.ResponseWriter,
	r *http.Request,
	current store.JobStatus,
	action string,
) (store.JobStatus, bool) {
	switch action {
	case "pause":
		if current != store.JobStatusRunning && current != store.JobStatusPending {
			writeProblem(w, r, http.StatusConflict, "job cannot be paused in its current state")
			return "", false
		}
		return store.JobStatusPaused, true
	case "resume":
		if current != store.JobStatusPaused {
			writeProblem(w, r, http.StatusConflict, "only paused jobs can be resumed")
			return "", false
		}
		return store.JobStatusPending, true
	case "":
		return "", true
	default:
		writeProblem(w, r, http.StatusBadRequest, `action must be "pause" or "resume"`)
		return "", false
	}
}

// applyStatusChange persists a status transition and returns the updated job.
// It writes an error response and returns a non-nil error when persistence fails.
// A resume goes through [store.JobStore.ResumeJob] so an auto-parked job also
// has its park reason cleared and failure counter reset (re-arming the failure
// limit); other transitions use the plain status update.
func (h *jobHandler) applyStatusChange(
	w http.ResponseWriter,
	r *http.Request,
	id, action string,
	newStatus store.JobStatus,
	job store.Job,
) (store.Job, error) {
	var err error
	if action == "resume" {
		err = h.store.ResumeJob(r.Context(), id, time.Now().UTC())
	} else {
		err = h.store.UpdateJobStatus(r.Context(), id, newStatus)
	}
	if err != nil {
		h.logger.ErrorContext(
			r.Context(), "jobs: patch status update failed",
			slog.String("id", id),
			slog.String("status", string(newStatus)),
			slog.Any("error", err),
		)
		writeProblem(w, r, http.StatusInternalServerError, "failed to update job status")
		return job, err
	}
	job.Status = newStatus
	if action == "resume" && job.ParkReason != "" {
		// Mirror ResumeJob's park-state reset in the response body.
		job.FailedAttempts = 0
		job.ParkReason = ""
	}
	return job, nil
}

// handleStoreError writes the appropriate HTTP error for a store lookup failure.
// It logs unexpected errors. id is included in log output for diagnostics.
func handleStoreError(w http.ResponseWriter, r *http.Request, err error, logger *slog.Logger, msg, id string) {
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, r, http.StatusNotFound, "job not found")
		return
	}
	logger.ErrorContext(r.Context(), msg, slog.String("id", id), slog.Any("error", err))
	writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve job")
}

// ── POST /api/v1/jobs/{id}/cancel ────────────────────────────────────────────

// cancelJob cancels the job and propagates the cancellation to any assigned
// workers via the scheduler.
func (h *jobHandler) cancelJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	job, err := h.store.GetJob(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "job not found")
			return
		}
		h.logger.ErrorContext(ctx, "jobs: cancel get failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve job")
		return
	}

	// Fast-path: already canceled → idempotent 204; completed/failed → 409.
	// This check also runs inside CancelJobStatus (atomic SQL guard), so a
	// concurrent transition that races past here is still safe.
	switch job.Status {
	case store.JobStatusCanceled:
		w.WriteHeader(http.StatusNoContent)
		return
	case store.JobStatusCompleted, store.JobStatusFailed:
		writeProblem(w, r, http.StatusConflict, "job has already completed and cannot be canceled")
		return
	}

	// CancelJob handles task cancellation and NATS signal dispatch.
	if err = h.sched.CancelJob(ctx, id); err != nil {
		h.logger.ErrorContext(ctx, "jobs: cancel scheduler failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to cancel job tasks")
		return
	}

	// CancelJobStatus uses a conditional UPDATE (WHERE status NOT IN terminal
	// states) so a concurrent scheduler transition that completed the job
	// between the GetJob check above and this call is not overwritten.
	if err = h.store.CancelJobStatus(ctx, id); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Job reached a terminal state (completed/failed) concurrently.
			// Tasks were already canceled above; treat as a conflict.
			writeProblem(w, r, http.StatusConflict, "job completed before cancellation could be applied")
			return
		}
		h.logger.ErrorContext(ctx, "jobs: cancel status update failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to update job status")
		return
	}

	// A canceled upstream can never satisfy jobs blocked on it — cancel them
	// now rather than waiting for the periodic sweep, matching deleteJob.
	if err := h.sched.ReconcileDependents(ctx, id); err != nil {
		h.logger.ErrorContext(ctx, "jobs: reconcile dependents after cancel failed",
			slog.String("job_id", id), slog.Any("error", err))
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── POST /api/v1/jobs/{id}/retry ──────────────────────────────────────────────

// retryJobResponse is returned by POST /api/v1/jobs/{id}/retry.
type retryJobResponse struct {
	JobID   string `json:"job_id"`
	Retried int    `json:"retried"`
}

// retryJob revives every failed/canceled task of the job (and the job/step
// status) so the scheduler re-runs them. It is idempotent: a job with no
// eligible tasks returns 200 with retried=0.
func (h *jobHandler) retryJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	if _, err := h.store.GetJob(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "job not found")
			return
		}
		h.logger.ErrorContext(ctx, "jobs: retry get failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve job")
		return
	}

	n, err := h.sched.RetryJob(ctx, id)
	if err != nil {
		h.logger.ErrorContext(ctx, "jobs: retry failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retry job tasks")
		return
	}

	writeJSON(w, http.StatusOK, retryJobResponse{JobID: id, Retried: n})
}

// ── DELETE /api/v1/jobs/{id} ──────────────────────────────────────────────────

// deleteJob hard-deletes a job and all of its data. If the job is still active
// (not in a terminal state) its tasks are canceled first via the scheduler so
// assigned workers stop work, exactly as cancelJob does. The delete itself is
// always permitted regardless of job state.
func (h *jobHandler) deleteJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	job, err := h.store.GetJob(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "job not found")
			return
		}
		h.logger.ErrorContext(ctx, "jobs: delete get failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve job")
		return
	}

	// Active job → cancel its tasks immediately so workers stop. Terminal jobs
	// have nothing in flight, so the cancel is skipped.
	if !job.Status.IsTerminal() {
		if err := h.sched.CancelJob(ctx, id); err != nil {
			h.logger.ErrorContext(ctx, "jobs: delete cancel failed", slog.String("id", id), slog.Any("error", err))
			writeProblem(w, r, http.StatusInternalServerError, "failed to cancel job tasks")
			return
		}
	}

	if err := h.store.DeleteJob(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "job not found")
			return
		}
		h.logger.ErrorContext(ctx, "jobs: delete failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to delete job")
		return
	}

	if h.notifier != nil {
		h.notifier.NotifyJob(ws.JobEvent{
			JobID: job.ID,
			Name:  job.Name,
			// Owner must be set explicitly here: the row is already gone by
			// this point, so the hub's ownerCache fallback (GetJob on a
			// deleted row) cannot resolve it and would drop this envelope for
			// every owner-scoped subscriber, including the owner whose own
			// job this was. This is the only NotifyJob call site where that's
			// true — every other caller (internal/scheduler) fires while the
			// row still exists.
			Owner:     job.Owner,
			QueueID:   job.QueueID,
			Status:    ws.JobStatusRemoved,
			UpdatedAt: time.Now().UTC(),
		})
	}

	// A deleted upstream can never satisfy jobs blocked on it — cancel them now
	// rather than waiting for the periodic sweep.
	if err := h.sched.ReconcileDependents(ctx, id); err != nil {
		h.logger.ErrorContext(ctx, "jobs: reconcile dependents after delete failed",
			slog.String("job_id", id), slog.Any("error", err))
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Conversion helpers ────────────────────────────────────────────────────────

// resolveQueueNames builds a map of queue ID → queue name for the unique queue
// IDs present in jobs. Missing or erroring queues are silently omitted so a
// deleted queue does not break the job list response.
func resolveQueueNames(ctx context.Context, s store.Store, jobs []store.Job) map[string]string {
	seen := make(map[string]struct{}, len(jobs))
	for _, j := range jobs {
		seen[j.QueueID] = struct{}{}
	}
	names := make(map[string]string, len(seen))
	for id := range seen {
		if q, err := s.GetQueue(ctx, id); err == nil {
			names[id] = q.Name
		}
	}
	return names
}

// toJobResponse converts a [store.Job] into the API wire type.
func toJobResponse(j store.Job) jobResponse {
	return jobResponse{
		ID:             j.ID,
		FarmID:         j.FarmID,
		QueueID:        j.QueueID,
		Name:           j.Name,
		Owner:          j.Owner,
		Submitter:      j.Submitter,
		Priority:       j.Priority,
		Status:         string(j.Status),
		DependsOn:      j.DependsOn,
		Project:        j.Project,
		TemplateFormat: string(j.TemplateFormat),
		CreatedAt:      j.CreatedAt,
		UpdatedAt:      j.UpdatedAt,
		StartedAt:      j.StartedAt,
		CompletedAt:    j.CompletedAt,

		FailedAttempts:    j.FailedAttempts,
		ParkReason:        j.ParkReason,
		MaxAttempts:       j.MaxAttempts,
		RetryDelaySeconds: j.RetryDelaySeconds,
		FailureLimit:      j.FailureLimit,
	}
}

// toStepResponse converts a [store.Step] into the API wire type.
func toStepResponse(s store.Step) stepResponse {
	return stepResponse{
		ID:        s.ID,
		Name:      s.Name,
		StepOrder: s.StepOrder,
		Status:    string(s.Status),
		DependsOn: s.DependsOn,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
}

// toTaskCountsResponse builds a [taskCountsResponse] from a status→count map
// plus the separately-queried unschedulable count.
func toTaskCountsResponse(counts map[store.TaskStatus]int, unschedulable int) taskCountsResponse {
	tc := taskCountsResponse{Unschedulable: unschedulable}
	for status, n := range counts {
		tc.Total += n
		switch status {
		case store.TaskStatusPending:
			tc.Pending = n
		case store.TaskStatusReady:
			tc.Ready = n
		case store.TaskStatusAssigned:
			tc.Assigned = n
		case store.TaskStatusRunning:
			tc.Running = n
		case store.TaskStatusSucceeded:
			tc.Succeeded = n
		case store.TaskStatusFailed:
			tc.Failed = n
		case store.TaskStatusCanceled:
			tc.Canceled = n
		}
	}
	return tc
}

// ── Format detection ──────────────────────────────────────────────────────────

// templateFormat is the wire-format label used in log messages.
type templateFormat string

const (
	templateFormatYAML templateFormat = "yaml"
	templateFormatJSON templateFormat = "json"
)

// detectFormat returns the template format implied by a Content-Type header
// value and the corresponding [store.TemplateFormat].
// YAML types: application/yaml, application/x-yaml, text/yaml, text/x-yaml.
// Everything else is treated as JSON.
func detectFormat(contentType string) (templateFormat, store.TemplateFormat) {
	ct := strings.ToLower(contentType)
	// Strip parameters (e.g. "; charset=utf-8").
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch ct {
	case "application/yaml", "application/x-yaml", "text/yaml", "text/x-yaml":
		return templateFormatYAML, store.TemplateFormatYAML
	default:
		return templateFormatJSON, store.TemplateFormatJSON
	}
}

// ── Sort / pagination helpers ─────────────────────────────────────────────────

// toJobSortField maps a query-parameter string to a [store.JobSortField].
// Unknown values fall back to the default sort field.
func toJobSortField(s string) store.JobSortField {
	switch s {
	case "priority":
		return store.JobSortByPriority
	case "status":
		return store.JobSortByStatus
	case "updated_at":
		return store.JobSortByUpdatedAt
	case "name":
		return store.JobSortByName
	default:
		return store.JobSortByCreatedAt
	}
}

// toSortDir maps a query-parameter string to a [store.SortDir].
// "desc" → descending; anything else → ascending.
func toSortDir(s string) store.SortDir {
	if strings.EqualFold(s, "desc") {
		return store.SortDesc
	}
	return store.SortAsc
}

// parseIntQuery parses a string as an integer, returning fallback on failure.
func parseIntQuery(s string, fallback int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return fallback
}

// ── Validation helpers ────────────────────────────────────────────────────────

// isSubmitValidationError returns true for errors originating from the OpenJD
// parser, validator, or storage-location checker, signaling that a 422 rather
// than a 500 is appropriate.  It uses [errors.As] against the sentinel
// [openjd.SubmitValidationError] type so the check is robust to message
// refactors, wrapped errors, and new error paths.
func isSubmitValidationError(err error) bool {
	var ve *openjd.SubmitValidationError
	return errors.As(err, &ve)
}

// isSubmitDeadlineError reports whether err is the submission pipeline's
// wall-clock backstop tripping (EXPR sub-project H1) rather than any verdict
// about the template.
//
// Matched STRUCTURALLY, on the exported sentinel, never by reading a message:
// a budget breach and a deadline travel the same return path, and the whole
// point of the sentinel is that the two are tellable apart without string
// matching. The pipeline never wraps a deadline in a
// [openjd.SubmitValidationError], so this and [isSubmitValidationError] cannot
// both be true.
func isSubmitDeadlineError(err error) bool {
	return errors.Is(err, expr.ErrDeadlineExceeded)
}

// exprDeadlineProblemDetail is the 503 body every route that walks a
// client-supplied OpenJD TEMPLATE returns when the wall-clock backstop trips.
// It names the configuration key deliberately: nothing about the submitted body
// is wrong, so the only actions available are retrying or asking the operator
// to widen the budget. (The preset route sends its own wording because what the
// caller asked this server to load is a preset definition, not a template.)
const exprDeadlineProblemDetail = "template validation exceeded its time budget on this server; retry, " +
	"or ask the operator about openjd.expr_submission_deadline"

// exprDeadlineAt returns the absolute wall-clock instant one request's
// expression evaluation must stop at, given the configured duration (config
// openjd.expr_submission_deadline). A non-positive duration means no backstop
// is configured and yields the zero time.
//
// Called once per request, on purpose. The configured value is a duration; what
// the pipeline needs is an instant. Computing that instant anywhere that runs
// once — at server boot, on the shared Submitter — would give every request the
// same deadline, so every submission arriving after it would fail forever while
// every one before it got a shrinking allowance.
func exprDeadlineAt(d time.Duration) time.Time {
	if d <= 0 {
		return time.Time{}
	}
	return time.Now().Add(d)
}

// writeExprDeadlineProblem reports err as a 503 when it is the submission
// pipeline's wall-clock backstop tripping, and reports whether it did. A false
// return means err is something else entirely and the caller's own mapping
// applies unchanged.
//
// It is one function because the 503-vs-4xx split is a single contract shared by
// every route that walks a client-supplied template, and four copies of it had
// already begun to drift. Callers keep their own guard ordering: the deadline
// must be tested BEFORE any validation branch, since a breach and a verdict
// travel the same return path.
//
// 503, NEVER a 4xx. A 4xx says the body is wrong and retrying is pointless,
// while this says the server gave up on a body that might be perfectly valid —
// the same bytes would validate on an idle host. Reporting a load-dependent
// outcome as the caller's fault would make acceptance depend on machine load,
// which no client can reason about or retry sensibly.
//
// WARN, NOT ERROR. Nothing is broken: the server met a bound it was configured
// to meet, on a request the client is explicitly told to retry. Error is also
// the wrong level for a CLIENT-PROVOKABLE, LOAD-DEPENDENT event on an anonymous
// path — every occurrence takes a slot in the bounded diagnostics ring buffer
// (internal/diag), so a run of deadlines would evict the genuine server errors
// an operator opened that buffer to find. The scheduler's protocol-version gate
// logs repeating heartbeat mismatches at debug for exactly this reason; see
// discardOnVersionMismatch. The neighboring validation branches log nothing at
// all, on the same reasoning taken one step further.
//
// logMsg names the route, detail is the client-facing body (see
// [exprDeadlineProblemDetail]), and extra carries any route-specific log
// attributes — logged ahead of the deadline and the error, so each call site
// keeps the attribute order it had.
func writeExprDeadlineProblem(
	w http.ResponseWriter, r *http.Request, logger *slog.Logger,
	err error, logMsg, detail string, deadline time.Duration, extra ...any,
) bool {
	if !isSubmitDeadlineError(err) {
		return false
	}
	logger.WarnContext(r.Context(), logMsg,
		append(extra, slog.Duration("deadline", deadline), slog.Any("error", err))...)
	writeProblem(w, r, http.StatusServiceUnavailable, detail)
	return true
}

// parseParamQueryParams extracts job-parameter values from query parameters
// using the prefix convention "param.<Name>=<value>".
//
// For example, ?param.FrameStart=1&param.FrameEnd=100 yields:
//
//	map[string]string{"FrameStart": "1", "FrameEnd": "100"}
//
// Keys that do not start with "param." are silently ignored.  The returned
// map is nil when no matching keys are present.
func parseParamQueryParams(query map[string][]string) map[string]string {
	const prefix = "param."
	var result map[string]string
	for key, vals := range query {
		name, ok := strings.CutPrefix(key, prefix)
		if !ok || name == "" || len(vals) == 0 {
			continue
		}
		if result == nil {
			result = make(map[string]string)
		}
		result[name] = vals[0] // first value wins; duplicates are ignored
	}
	return result
}

// writeProblem, writeJSON, and the problemDetail type are defined in errors.go.
