// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// HTTP-boundary tests for EXPR sub-project H1's wall-clock submission
// deadline: how a deadline breach is reported, and that the absolute instant
// handed to the submitter is computed per REQUEST from the configured
// duration.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/ws"
)

// ── stubSubmitter ───────────────────────────────────────────────────────────

// stubSubmitter implements [JobSubmitter] so these tests can drive the
// handlers' error mapping and their deadline arithmetic directly.
//
// A stub is used rather than the real pipeline because the real pipeline
// cannot produce a deadline breach today: EXPR is StatusInProgress, so
// validateExtensions rejects every EXPR-declaring template before any
// expression is evaluated, and a template that declares no extensions never
// touches the meter the deadline lives in. The end-to-end proof — that
// openjd.Submitter really returns expr.ErrDeadlineExceeded, unwrapped by any
// SubmitValidationError — is TestSubmit_DeadlineIsNotASubmitValidationError in
// internal/openjd, which flips the registry entry the way that package's other
// submit tests do. What is tested HERE is the half that lives here: which
// status code that error becomes.
type stubSubmitter struct {
	err  error
	opts openjd.SubmitOptions // the options of the most recent call
	seen bool
}

func (s *stubSubmitter) Submit(
	_ context.Context, _ string, _ store.TemplateFormat, opts openjd.SubmitOptions,
) (*openjd.SubmitResult, error) {
	s.opts = opts
	s.seen = true
	return nil, s.err
}

// deadlineErr is what the submission pipeline returns when the wall-clock
// backstop trips: the sentinel, wrapped for context, and deliberately NOT a
// *openjd.SubmitValidationError.
func deadlineErr() error {
	return fmt.Errorf("openjd: submit: validation: %w", expr.ErrDeadlineExceeded)
}

// ── the contract ────────────────────────────────────────────────────────────

// TestSubmitJob_DeadlineIsA503NotA422 is sub-project H1's central contract at
// the HTTP boundary: a wall-clock stop is the server giving up, and must not
// be reported as an invalid template.
//
// 422 tells a submitter their template is wrong and that retrying is
// pointless. 503 tells them the server could not finish and that retrying may
// work. A wall-clock verdict is non-deterministic — the same body would
// validate on an idle machine — so returning the first would make acceptance
// depend on machine load, which no client can reason about.
//
// 500 is checked too, because that is what this endpoint returns for any error
// it does not recognize: a test that only asserted "not 422" would pass with
// the mapping deleted.
func TestSubmitJob_DeadlineIsA503NotA422(t *testing.T) {
	st := fake.New()
	sub := &stubSubmitter{err: deadlineErr()}
	h := newJobHandler(st, sub, &fakeScheduler{}, ws.NoopNotifier{},
		newTestLogger(), testRetryDefaults, false, 5*time.Second)

	req := newReq(t, http.MethodPost,
		"/api/v1/jobs?farm_id=farm-1&queue_id=queue-1", strings.NewReader(minimalOpenJDJSON("DeadlineTest")))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.submitJob(rr, req)

	if rr.Code == http.StatusUnprocessableEntity {
		t.Fatalf("a wall-clock deadline was reported as 422 (invalid template); "+
			"the same body would validate on an idle machine — body: %s", rr.Body)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (service unavailable) — body: %s",
			rr.Code, http.StatusServiceUnavailable, rr.Body)
	}
}

// TestSubmitProductJob_DeadlineIsA503NotA422 pins the same contract on the
// other submission route. It runs the identical pipeline with a template from
// the catalog and client-supplied parameters, so a deadline is reachable there
// for the same reasons — and would otherwise fall through to a 500.
func TestSubmitProductJob_DeadlineIsA503NotA422(t *testing.T) {
	st := fake.New()
	if _, err := st.CreateProduct(t.Context(), store.Product{
		Name:     "p",
		Title:    "P",
		Template: minimalOpenJDJSON("ProductDeadlineTest"),
		Format:   store.TemplateFormatJSON,
	}); err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}
	sub := &stubSubmitter{err: deadlineErr()}
	h := newProductHandler(product.NewCatalog(st), sub, nil, st, newTestLogger(), false, 5*time.Second)
	r := chi.NewRouter()
	r.Post("/api/v1/products/{name}/jobs", h.submitProductJob)

	req := newReq(t, http.MethodPost, "/api/v1/products/p/jobs",
		strings.NewReader(`{"farm_id":"farm-1","queue_id":"queue-1"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code == http.StatusUnprocessableEntity {
		t.Fatalf("a wall-clock deadline was reported as 422 — body: %s", rr.Body)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d — body: %s", rr.Code, http.StatusServiceUnavailable, rr.Body)
	}
}

// TestSubmitJob_ValidationErrorStaysA422 is the other half of the contract: the
// 503 branch must be narrow. A template the checker rejects is still the
// submitter's fault and still a 422.
func TestSubmitJob_ValidationErrorStaysA422(t *testing.T) {
	st := fake.New()
	sub := &stubSubmitter{err: &openjd.SubmitValidationError{Cause: errors.New("bad template")}}
	h := newJobHandler(st, sub, &fakeScheduler{}, ws.NoopNotifier{},
		newTestLogger(), testRetryDefaults, false, 5*time.Second)

	req := newReq(t, http.MethodPost,
		"/api/v1/jobs?farm_id=farm-1&queue_id=queue-1", strings.NewReader(minimalOpenJDJSON("StillA422")))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.submitJob(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d — body: %s", rr.Code, http.StatusUnprocessableEntity, rr.Body)
	}
}

// ── the deadline is computed per request ────────────────────────────────────

// TestSubmitJob_DeadlineIsComputedPerRequest pins the trap this wiring exists
// to avoid.
//
// The configured value is a DURATION; what the pipeline needs is an ABSOLUTE
// time. Computing that instant anywhere that runs once — on the Submitter
// built at server boot, say — would give every submission the same deadline,
// so every request arriving after that instant would fail forever and every
// one before it would get a shrinking allowance. So the instant must be taken
// inside the handler, per call, and it must land in the future.
func TestSubmitJob_DeadlineIsComputedPerRequest(t *testing.T) {
	st := fake.New()
	sub := &stubSubmitter{err: errors.New("stop after recording the options")}
	const configured = 5 * time.Second
	h := newJobHandler(st, sub, &fakeScheduler{}, ws.NoopNotifier{},
		newTestLogger(), testRetryDefaults, false, configured)

	submit := func(t *testing.T) time.Time {
		t.Helper()
		before := time.Now()
		req := newReq(t, http.MethodPost,
			"/api/v1/jobs?farm_id=farm-1&queue_id=queue-1", strings.NewReader(minimalOpenJDJSON("PerRequest")))
		req.Header.Set("Content-Type", "application/json")
		h.submitJob(httptest.NewRecorder(), req)
		if !sub.seen {
			t.Fatal("the submitter was never called")
		}
		got := sub.opts.Deadline
		if got.Before(before.Add(configured)) || got.After(time.Now().Add(configured)) {
			t.Fatalf("deadline = %v, want an instant configured (%s) into this request's future",
				got, configured)
		}
		return got
	}

	first := submit(t)
	time.Sleep(2 * time.Millisecond)
	second := submit(t)
	if !second.After(first) {
		t.Fatalf("two submissions produced deadlines %v and %v; the second must be later, "+
			"or the instant is being computed once rather than per request", first, second)
	}
}

// TestNewRouter_SubmissionDeadlineReachesBothHandlers covers the THIRD and last
// hop that carries the operator's deadline to where it is spent:
// api.Config -> the two submission handlers, inside [NewRouter].
//
// The tests above build handlers directly, so they would all still pass with
// cfg.ExprSubmissionDeadline dropped from either constructor call in router.go
// — the handlers would simply be built with a zero duration and every
// submission would run unbounded. Nothing else in this repository looks at that
// wiring, so this is the only place it can fail.
//
// Both routes are checked in one test because they are two independent call
// sites of the same value, and the product route was added second: a fix that
// wires one and forgets the other is the realistic mistake.
func TestNewRouter_SubmissionDeadlineReachesBothHandlers(t *testing.T) {
	const configured = 37 * time.Second // non-default; a stale default cannot pass

	st := fake.New()
	if _, err := st.CreateProduct(t.Context(), store.Product{
		Name:     "p",
		Title:    "P",
		Template: minimalOpenJDJSON("RouterDeadlineTest"),
		Format:   store.TemplateFormatJSON,
	}); err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}
	sub := &stubSubmitter{err: errors.New("stop after recording the options")}
	r := NewRouter(
		Config{DisableRateLimit: true, ExprSubmissionDeadline: configured},
		Deps{Store: st, Submitter: sub, Products: product.NewCatalog(st)},
		newTestLogger(), metrics.New(), health.NewRegistry(),
	)

	routes := []struct {
		name        string
		target      string
		body        string
		contentType string
	}{
		{
			name:        "POST /api/v1/jobs",
			target:      "/api/v1/jobs?farm_id=farm-1&queue_id=queue-1",
			body:        minimalOpenJDJSON("RouterDeadlineTest"),
			contentType: "application/json",
		},
		{
			name:        "POST /api/v1/products/{name}/jobs",
			target:      "/api/v1/products/p/jobs",
			body:        `{"farm_id":"farm-1","queue_id":"queue-1"}`,
			contentType: "application/json",
		},
	}
	for _, tc := range routes {
		t.Run(tc.name, func(t *testing.T) {
			sub.seen, sub.opts = false, openjd.SubmitOptions{}

			before := time.Now()
			req := newReq(t, http.MethodPost, tc.target, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.contentType)
			r.ServeHTTP(httptest.NewRecorder(), req)

			if !sub.seen {
				t.Fatal("the submitter was never called; this test cannot see the wiring it exists to check")
			}
			got := sub.opts.Deadline
			if got.IsZero() {
				t.Fatal("the submitter got the zero deadline: Config.ExprSubmissionDeadline is not " +
					"reaching this handler, so submissions on this route run with no wall-clock bound")
			}
			if got.Before(before.Add(configured)) || got.After(time.Now().Add(configured)) {
				t.Fatalf("deadline = %v, want an instant the configured %s into this request's future "+
					"(a different duration reached the handler)", got, configured)
			}
		})
	}
}

// TestSubmitJob_NoConfiguredDeadlineMeansNoDeadline pins the zero value's
// meaning at this layer. Handlers built without a duration — every test in this
// package that does not care, and any embedder wiring the router directly —
// must pass the zero time, which the evaluator treats as "no deadline" and
// which costs it nothing.
func TestSubmitJob_NoConfiguredDeadlineMeansNoDeadline(t *testing.T) {
	st := fake.New()
	sub := &stubSubmitter{err: errors.New("stop after recording the options")}
	h := newJobHandler(st, sub, &fakeScheduler{}, ws.NoopNotifier{},
		newTestLogger(), testRetryDefaults, false, 0)

	req := newReq(t, http.MethodPost,
		"/api/v1/jobs?farm_id=farm-1&queue_id=queue-1", strings.NewReader(minimalOpenJDJSON("NoDeadline")))
	req.Header.Set("Content-Type", "application/json")
	h.submitJob(httptest.NewRecorder(), req)

	if !sub.seen {
		t.Fatal("the submitter was never called")
	}
	if !sub.opts.Deadline.IsZero() {
		t.Fatalf("deadline = %v, want the zero time when none is configured", sub.opts.Deadline)
	}
}
