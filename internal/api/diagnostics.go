// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Diagnostics REST handler.
//
// Route summary:
//
//	GET /api/v1/diagnostics/logs — query the in-memory diagnostic log buffer

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/uberware/sqi/internal/diag"
)

// DiagReader is the read-only view of the diagnostic log buffer the handler
// depends on. *diag.Buffer satisfies it. When diagnostics are disabled the
// server injects a nil reader, in which case the endpoint returns 503.
type DiagReader interface {
	Query(diag.Filter) []diag.Record
}

// maxDiagLimit caps the number of records returned by a single request. It is
// also used as the default when the caller omits the limit query parameter.
const maxDiagLimit = 1000

// diagnosticRecordResponse is the JSON representation of [diag.Record]. It
// mirrors the diag.Record JSON tags so the wire shape is stable independently
// of the internal type.
type diagnosticRecordResponse struct {
	Ts        time.Time         `json:"ts"`
	Component string            `json:"component"`
	Level     string            `json:"level"`
	Msg       string            `json:"msg"`
	Attrs     map[string]string `json:"attrs,omitempty"`
}

// diagnosticLogsResponse is returned by GET /api/v1/diagnostics/logs.
type diagnosticLogsResponse struct {
	Records []diagnosticRecordResponse `json:"records"`
}

// diagnosticsHandler handles the diagnostics REST endpoints.
type diagnosticsHandler struct {
	reader DiagReader
	logger *slog.Logger
}

// newDiagnosticsHandler returns a diagnosticsHandler wired to the given reader.
// A nil reader is tolerated: it signals that diagnostics are disabled, and the
// handler responds with 503 Service Unavailable.
func newDiagnosticsHandler(reader DiagReader, logger *slog.Logger) *diagnosticsHandler {
	return &diagnosticsHandler{reader: reader, logger: logger}
}

// ── GET /api/v1/diagnostics/logs ──────────────────────────────────────────────

// getDiagnosticsLogs queries the in-memory diagnostic log buffer.
//
// Query parameters:
//   - component — exact component match ("server" | "worker:<id>")
//   - level     — minimum level (DEBUG | INFO | WARN | ERROR)
//   - task_id   — match records whose attrs["task_id"] equals this
//   - since     — RFC3339Nano lower bound (exclusive); ignored if unparseable
//   - limit     — max records (newest kept), clamped to maxDiagLimit
//
// Returns 503 when diagnostics are disabled (nil reader).
func (h *diagnosticsHandler) getDiagnosticsLogs(w http.ResponseWriter, r *http.Request) {
	if h.reader == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "diagnostics are disabled")
		return
	}

	q := r.URL.Query()

	f := diag.Filter{
		Component: q.Get("component"),
		MinLevel:  q.Get("level"),
		TaskID:    q.Get("task_id"),
	}

	if s := q.Get("since"); s != "" {
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			f.Since = ts
		}
		// Unparseable since is ignored (no lower bound).
	}

	// Default and clamp the limit to maxDiagLimit. parseIntQuery falls back to
	// maxDiagLimit when the parameter is absent or non-numeric.
	limit := parseIntQuery(q.Get("limit"), maxDiagLimit)
	if limit <= 0 || limit > maxDiagLimit {
		limit = maxDiagLimit
	}
	f.Limit = limit

	records := h.reader.Query(f)

	resp := diagnosticLogsResponse{
		Records: make([]diagnosticRecordResponse, len(records)),
	}
	for i, rec := range records {
		resp.Records[i] = diagnosticRecordResponse{
			Ts:        rec.Ts,
			Component: rec.Component,
			Level:     rec.Level,
			Msg:       rec.Msg,
			Attrs:     rec.Attrs,
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
