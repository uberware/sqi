// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Fuzz targets for REST endpoint payload decoding.
//
// Running:
//
//	go test -fuzz=FuzzSubmitJobPayload ./internal/api/
//	go test -fuzz=FuzzPatchJobPayload  ./internal/api/
//
// Invariants asserted on every corpus entry:
//  1. The handler must never panic on arbitrary input.
//  2. The handler must always return a well-formed HTTP response (status ≥ 100).
//  3. Error responses (4xx/5xx) must have a valid JSON body.
//
// Note: a 5xx response is NOT treated as a fuzz failure here.  The handlers are
// designed to return 5xx only for genuine internal errors (storage failures,
// unexpected state); triggering one via a crafted payload would indicate a real
// bug, but crafted payloads that slip past validation are expected to produce
// 4xx responses.  If the fuzzer discovers a 5xx path, investigate — it likely
// reveals a missing validation gate, but it is not an invariant violation in
// the same sense as a panic.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// ── POST /api/v1/jobs — fuzz the request body ─────────────────────────────────

func FuzzSubmitJobPayload(f *testing.F) {
	// Seed: minimal valid JSON template (should produce 201).
	f.Add([]byte(minimalOpenJDJSON("FuzzSeed")), "application/json")

	// Seed: minimal valid YAML template.
	f.Add([]byte(`specificationVersion: jobtemplate-2023-09
name: FuzzSeedYAML
steps:
  - name: S1
    script:
      actions:
        onRun:
          command: echo
`), "application/yaml")

	// Seed: empty body (should produce 4xx).
	f.Add([]byte{}, "application/json")

	// Seed: random JSON object that is not a valid template (should produce 4xx).
	f.Add([]byte(`{"hello": "world"}`), "application/json")

	// Seed: PATCH body accidentally sent to POST (wrong shape, should produce 4xx).
	f.Add([]byte(`{"priority": 10, "action": "pause"}`), "application/json")

	// Seed: valid JSON syntax but empty object (should produce 4xx/422).
	f.Add([]byte(`{}`), "application/json")

	f.Fuzz(func(t *testing.T, body []byte, contentType string) {
		st := fake.New()
		ctx := t.Context()

		// Pre-seed farm and queue so storage-location validation passes for
		// templates that don't reference any locations.
		now := time.Now()
		if _, err := st.CreateFarm(ctx, store.Farm{
			ID:        "fuzz-farm",
			Name:      "fuzz-farm",
			CreatedAt: now,
			UpdatedAt: now,
		}); err != nil {
			t.Fatalf("setup CreateFarm: %v", err)
		}
		if _, err := st.CreateQueue(ctx, store.Queue{
			ID:        "fuzz-queue",
			FarmID:    "fuzz-farm",
			Name:      "fuzz-queue",
			CreatedAt: now,
			UpdatedAt: now,
		}); err != nil {
			t.Fatalf("setup CreateQueue: %v", err)
		}

		r := newJobRouter(st, &fakeScheduler{})

		req := newReq(t, http.MethodPost,
			"/api/v1/jobs?farm_id=fuzz-farm&queue_id=fuzz-queue",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", contentType)

		rr := httptest.NewRecorder()

		// The handler must not panic — the test harness catches panics as
		// failures automatically.
		r.ServeHTTP(rr, req)

		// Must always return a valid HTTP status code.
		if rr.Code < 100 || rr.Code > 599 {
			t.Errorf("invalid status code %d", rr.Code)
		}

		// For non-2xx responses, the body must be valid JSON (problem+json).
		if rr.Code >= 400 {
			if !json.Valid(rr.Body.Bytes()) {
				t.Errorf("status %d: response body is not valid JSON: %q",
					rr.Code, rr.Body.String())
			}
		}
	})
}

// ── PATCH /api/v1/jobs/{id} — fuzz the patch body ────────────────────────────

func FuzzPatchJobPayload(f *testing.F) {
	// Seed: valid priority update.
	f.Add([]byte(`{"priority": 75}`))
	// Seed: valid pause action.
	f.Add([]byte(`{"action": "pause"}`))
	// Seed: valid resume action.
	f.Add([]byte(`{"action": "resume"}`))
	// Seed: queue move.
	f.Add([]byte(`{"queue_id": "queue-1"}`))
	// Seed: empty object (no-op patch).
	f.Add([]byte(`{}`))
	// Seed: empty body (should produce 400).
	f.Add([]byte{})
	// Seed: invalid JSON (should produce 400).
	f.Add([]byte(`{bad json`))
	// Seed: deeply nested object (should produce 400 after decode attempt).
	f.Add([]byte(`{"priority": {"nested": 1}}`))

	f.Fuzz(func(t *testing.T, body []byte) {
		st := fake.New()
		ctx := t.Context()

		now := time.Now()
		if _, err := st.CreateFarm(ctx, store.Farm{ID: "fuzz-farm", Name: "f", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("setup CreateFarm: %v", err)
		}
		if _, err := st.CreateQueue(ctx, store.Queue{ID: "fuzz-queue", FarmID: "fuzz-farm", Name: "q", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("setup CreateQueue: %v", err)
		}

		// Seed a pending job to patch.
		job := store.Job{
			ID:             "fuzz-job",
			FarmID:         "fuzz-farm",
			QueueID:        "fuzz-queue",
			Name:           "fuzz-job",
			Priority:       50,
			Status:         store.JobStatusPending,
			TemplateFormat: store.TemplateFormatJSON,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if _, err := st.CreateJob(ctx, job); err != nil {
			t.Fatalf("setup CreateJob: %v", err)
		}

		r := newJobRouter(st, &fakeScheduler{})

		req := newReq(t, http.MethodPatch, "/api/v1/jobs/fuzz-job", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code < 100 || rr.Code > 599 {
			t.Errorf("invalid status code %d", rr.Code)
		}

		// Error responses must be valid JSON.
		if rr.Code >= 400 {
			if !json.Valid(rr.Body.Bytes()) {
				t.Errorf("status %d: response body is not valid JSON: %q",
					rr.Code, rr.Body.String())
			}
		}
	})
}
