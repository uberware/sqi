// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for the diagnostics REST handler.
//
// Route coverage:
//   GET /api/v1/diagnostics/logs — getDiagnosticsLogs

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/diag"
)

// newDiagnosticsRouter mounts the diagnostics handler with the given reader.
func newDiagnosticsRouter(reader DiagReader) chi.Router {
	h := newDiagnosticsHandler(reader, newTestLogger())
	r := chi.NewRouter()
	r.Get("/api/v1/diagnostics/logs", h.getDiagnosticsLogs)
	return r
}

func TestDiagnosticsLogs_FiltersByComponent(t *testing.T) {
	buf := diag.NewBuffer(100, nil)
	buf.Append(diag.Record{Ts: time.Now().UTC(), Component: "server", Level: "INFO", Msg: "s"})
	buf.Append(diag.Record{
		Ts:        time.Now().UTC(),
		Component: "worker:w1",
		Level:     "ERROR",
		Msg:       "e",
		Attrs:     map[string]string{"task_id": "t1"},
	})

	r := newDiagnosticsRouter(buf)
	req := newReq(t, http.MethodGet, "/api/v1/diagnostics/logs?component=worker:w1", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
	}
	var resp diagnosticLogsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Records) != 1 {
		t.Fatalf("records len = %d, want 1", len(resp.Records))
	}
	if resp.Records[0].Msg != "e" {
		t.Errorf("msg = %q, want %q", resp.Records[0].Msg, "e")
	}
}

func TestDiagnosticsLogs_TaskIDFilter(t *testing.T) {
	buf := diag.NewBuffer(100, nil)
	buf.Append(diag.Record{
		Ts:        time.Now().UTC(),
		Component: "worker:w1",
		Level:     "INFO",
		Msg:       "one",
		Attrs:     map[string]string{"task_id": "t1"},
	})
	buf.Append(diag.Record{
		Ts:        time.Now().UTC(),
		Component: "worker:w2",
		Level:     "INFO",
		Msg:       "two",
		Attrs:     map[string]string{"task_id": "t2"},
	})

	r := newDiagnosticsRouter(buf)
	req := newReq(t, http.MethodGet, "/api/v1/diagnostics/logs?task_id=t1", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
	}
	var resp diagnosticLogsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Records) != 1 {
		t.Fatalf("records len = %d, want 1", len(resp.Records))
	}
	if resp.Records[0].Msg != "one" {
		t.Errorf("msg = %q, want %q", resp.Records[0].Msg, "one")
	}
}

func TestDiagnosticsLogs_DisabledReturns503(t *testing.T) {
	r := newDiagnosticsRouter(nil)
	req := newReq(t, http.MethodGet, "/api/v1/diagnostics/logs", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}
}

func TestDiagnosticsLogs_EmptyNeverNull(t *testing.T) {
	buf := diag.NewBuffer(100, nil)
	r := newDiagnosticsRouter(buf)
	req := newReq(t, http.MethodGet, "/api/v1/diagnostics/logs", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(raw["records"]) != "[]" {
		t.Errorf("records = %s, want []", raw["records"])
	}
}
