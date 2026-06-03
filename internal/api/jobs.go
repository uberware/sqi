// SPDX-License-Identifier: AGPL-3.0-only

package api

// Job REST handlers — tasks 71–75.
//
// Route summary:
//
//	POST   /api/v1/jobs              — submit an OpenJD job (task 71)
//	GET    /api/v1/jobs              — list jobs with pagination + filters (task 72)
//	GET    /api/v1/jobs/{id}         — job detail with steps and task counts (task 73)
//	PATCH  /api/v1/jobs/{id}         — priority, queue move, pause/resume (task 74)
//	DELETE /api/v1/jobs/{id}         — cancel job (task 75)

import (
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
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/store"
)

// jobHandler handles all job-related REST endpoints.
type jobHandler struct {
	store     store.Store
	submitter *openjd.Submitter
	sched     *scheduler.Scheduler
	logger    *slog.Logger
}

// ── Wire-format types ─────────────────────────────────────────────────────────

// jobResponse is the JSON representation of a job returned by the list and
// create endpoints.
type jobResponse struct {
	ID             string     `json:"id"`
	FarmID         string     `json:"farm_id"`
	QueueID        string     `json:"queue_id"`
	Name           string     `json:"name"`
	Owner          string     `json:"owner"`
	Submitter      string     `json:"submitter"`
	Priority       int        `json:"priority"`
	Status         string     `json:"status"`
	Project        string     `json:"project,omitempty"`
	TemplateFormat string     `json:"template_format"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
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
}

// jobDetailResponse is returned by GET /api/v1/jobs/{id}.
type jobDetailResponse struct {
	jobResponse

	Steps      []stepResponse     `json:"steps"`
	TaskCounts taskCountsResponse `json:"task_counts"`
}

// jobListResponse is the paginated result returned by GET /api/v1/jobs.
type jobListResponse struct {
	Items  []jobResponse `json:"items"`
	Total  int           `json:"total"`
	Limit  int           `json:"limit"`
	Offset int           `json:"offset"`
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
}

// ── Handler constructors ──────────────────────────────────────────────────────

// newJobHandler returns a jobHandler wired to the given store, submitter, and
// scheduler.
func newJobHandler(
	st store.Store,
	sub *openjd.Submitter,
	sched *scheduler.Scheduler,
	logger *slog.Logger,
) *jobHandler {
	return &jobHandler{
		store:     st,
		submitter: sub,
		sched:     sched,
		logger:    logger,
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
// submitter, priority (integer ≥ 1; defaults to 50), project.
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

	opts := openjd.SubmitOptions{
		FarmID:    farmID,
		QueueID:   queueID,
		Owner:     r.URL.Query().Get("owner"),
		Submitter: r.URL.Query().Get("submitter"),
		Priority:  priority,
		Project:   r.URL.Query().Get("project"),
	}

	result, err := h.submitter.Submit(ctx, string(body), storeFormat, opts)
	if err != nil {
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

	writeJSON(w, http.StatusCreated, toJobResponse(result.Job))
}

// ── GET /api/v1/jobs ──────────────────────────────────────────────────────────

// listJobs returns a paginated, filtered, and sorted list of jobs.
//
// Query parameters:
//   - status    — filter by [store.JobStatus]
//   - owner     — filter by owner string
//   - queue_id  — filter by queue ID
//   - project   — filter by project label
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
		QueueID: q.Get("queue_id"),
		Status:  store.JobStatus(q.Get("status")),
		Owner:   q.Get("owner"),
		Project: q.Get("project"),
		SortBy:  toJobSortField(q.Get("sort_by")),
		SortDir: toSortDir(q.Get("sort_dir")),

		Pagination: pg,
	}

	page, err := h.store.ListJobs(ctx, opts)
	if err != nil {
		h.logger.ErrorContext(ctx, "jobs: list failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to list jobs")
		return
	}

	resp := jobListResponse{
		Items:  make([]jobResponse, len(page.Items)),
		Total:  page.Total,
		Limit:  page.Limit,
		Offset: page.Offset,
	}
	for i, j := range page.Items {
		resp.Items[i] = toJobResponse(j)
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

	stepResps := make([]stepResponse, len(steps))
	for i, s := range steps {
		stepResps[i] = toStepResponse(s)
	}

	resp := jobDetailResponse{
		jobResponse: toJobResponse(job),
		Steps:       stepResps,
		TaskCounts:  toTaskCountsResponse(taskCounts),
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

	if isTerminalJob(job.Status) {
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
		updated, err = h.applyStatusChange(w, r, id, newStatus, updated)
		if err != nil {
			return
		}
	}

	writeJSON(w, http.StatusOK, toJobResponse(updated))
}

// isTerminalJob reports whether a job in the given status cannot be modified.
func isTerminalJob(s store.JobStatus) bool {
	return s == store.JobStatusCompleted ||
		s == store.JobStatusFailed ||
		s == store.JobStatusCanceled
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
func (h *jobHandler) applyStatusChange(
	w http.ResponseWriter,
	r *http.Request,
	id string,
	newStatus store.JobStatus,
	job store.Job,
) (store.Job, error) {
	if err := h.store.UpdateJobStatus(r.Context(), id, newStatus); err != nil {
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

// ── DELETE /api/v1/jobs/{id} ──────────────────────────────────────────────────

// cancelJob cancels the job and propagates the cancellation to any assigned
// workers via the scheduler.
func (h *jobHandler) cancelJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	_, err := h.store.GetJob(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "job not found")
			return
		}
		h.logger.ErrorContext(ctx, "jobs: cancel get failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve job")
		return
	}

	// CancelJob handles task cancellation and NATS signal dispatch (task 54).
	if err = h.sched.CancelJob(ctx, id); err != nil {
		h.logger.ErrorContext(ctx, "jobs: cancel scheduler failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to cancel job tasks")
		return
	}

	if err = h.store.UpdateJobStatus(ctx, id, store.JobStatusCanceled); err != nil {
		h.logger.ErrorContext(ctx, "jobs: cancel status update failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to update job status")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Conversion helpers ────────────────────────────────────────────────────────

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
		Project:        j.Project,
		TemplateFormat: string(j.TemplateFormat),
		CreatedAt:      j.CreatedAt,
		UpdatedAt:      j.UpdatedAt,
		StartedAt:      j.StartedAt,
		CompletedAt:    j.CompletedAt,
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

// toTaskCountsResponse builds a [taskCountsResponse] from a status→count map.
func toTaskCountsResponse(counts map[store.TaskStatus]int) taskCountsResponse {
	var tc taskCountsResponse
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
// parser or validator, signaling that a 422 rather than a 500 is appropriate.
func isSubmitValidationError(err error) bool {
	msg := err.Error()
	return strings.HasPrefix(msg, "openjd: submit: parse") ||
		strings.HasPrefix(msg, "openjd: submit: validation") ||
		strings.HasPrefix(msg, "openjd: submit: storage location")
}

// writeProblem, writeJSON, and the problemDetail type are defined in errors.go.
