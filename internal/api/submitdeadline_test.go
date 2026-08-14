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
// It is no longer the only proof. Sub-project H2 made EXPR StatusSupported, so
// a request really can reach an expression evaluation, and the end-to-end tests
// at the bottom of this file drive the REAL openjd.Submitter through these same
// handlers with a deadline that really expires. The stub tests are kept as the
// narrow unit-level guard on one link: given an error of that shape, which
// status code does this handler produce. Keeping both is what localizes a
// failure — both failing means the mapping broke, only the end-to-end failing
// means the pipeline stopped producing the sentinel.
//
// The stub also remains the only way to inject error shapes the real pipeline
// cannot be made to produce on demand, which is what the deadline-arithmetic
// tests below need: a submitter that records its options and stops.
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
	h := newProductHandler(product.NewCatalog(st), sub, nil, st, newTestLogger(), false, 5*time.Second, openjd.ExprLimits{})
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

// ── the product create/update route ─────────────────────────────────────────

// TestProductTemplateValidation_DeadlineIsA503 covers the THIRD route that
// accepts an arbitrary client-supplied OpenJD template, alongside the two
// submission routes: POST /api/v1/products and PUT /api/v1/products/{name}.
//
// Before EXPR sub-project H1's task 5 it reached the full validator with no
// operator limits and no time bound at all — anonymously, since auth is off by
// default. The mapping is asserted on the shared helper here, which keeps the
// unit-level guard on the status choice alone;
// TestCreateProduct_RealValidationDeadlineIsA503NotA400 below drives the same
// mapping through the route with the real validator, which it can now that EXPR
// is StatusSupported.
func TestProductTemplateValidation_DeadlineIsA503(t *testing.T) {
	st := fake.New()
	h := newProductHandler(product.NewCatalog(st), &stubSubmitter{}, nil, st,
		newTestLogger(), false, 5*time.Second, openjd.ExprLimits{})

	rr := httptest.NewRecorder()
	h.writeTemplateProblem(rr, newReq(t, http.MethodPost, "/api/v1/products", strings.NewReader("{}")),
		fmt.Errorf("product: template validation: %w", expr.ErrDeadlineExceeded))

	if rr.Code == http.StatusBadRequest {
		t.Fatalf("a wall-clock deadline was reported as 400 (bad template); the same body "+
			"would validate on an idle machine — body: %s", rr.Body)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d — body: %s", rr.Code, http.StatusServiceUnavailable, rr.Body)
	}
}

// TestProductTemplateValidation_BadTemplateStaysA400 is the other half: the 503
// branch must be narrow, and an invalid template is still the client's fault.
func TestProductTemplateValidation_BadTemplateStaysA400(t *testing.T) {
	st := fake.New()
	h := newProductHandler(product.NewCatalog(st), &stubSubmitter{}, nil, st,
		newTestLogger(), false, 5*time.Second, openjd.ExprLimits{})

	rr := httptest.NewRecorder()
	h.writeTemplateProblem(rr, newReq(t, http.MethodPost, "/api/v1/products", strings.NewReader("{}")),
		errors.New("product: template validation: /steps: at least one step is required"))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d — body: %s", rr.Code, http.StatusBadRequest, rr.Body)
	}
}

// TestProductTemplateValidation_OptionsCarryLimitsAndDeadline pins what bounds
// one validation of a client-supplied product template.
//
// The limits half is the defect the design spec named: this route never went
// through a Submitter, so it silently validated on openjd.DefaultExprLimits()
// however the operator had configured openjd.expr_*. The deadline half is the
// scope addition — it is the only bound on elapsed time this route has.
func TestProductTemplateValidation_OptionsCarryLimitsAndDeadline(t *testing.T) {
	const configured = 5 * time.Second
	limits := openjd.ExprLimits{
		SubmissionOperations:  4321,
		SubmissionMemoryBytes: 654_321,
		TemplatePositions:     321,
		TemplateRetainedBytes: 21_000,
	}
	st := fake.New()
	h := newProductHandler(product.NewCatalog(st), &stubSubmitter{}, nil, st,
		newTestLogger(), false, configured, limits)

	before := time.Now()
	got := h.templateValidateOptions()

	if !got.EnforceLimits {
		t.Error("EnforceLimits = false; this route has always enforced them")
	}
	if got.ExprLimits != limits {
		t.Errorf("ExprLimits = %+v, want the operator's %+v", got.ExprLimits, limits)
	}
	if got.Deadline.Before(before.Add(configured)) || got.Deadline.After(time.Now().Add(configured)) {
		t.Fatalf("deadline = %v, want an instant the configured %s into this request's future",
			got.Deadline, configured)
	}
}

// TestProductHandlerFor_CarriesTheConfiguredBounds covers the last hop of both
// EXPR bounds, api.Config -> the handler [NewRouter] builds.
//
// Every other test here constructs the handler directly, so they would all
// still pass with cfg.ExprLimits dropped from router.go — product template
// validation would simply run on openjd's defaults, which is the exact defect
// this task exists to fix and which no behavior can reveal while the expression
// walk is gated. [productHandlerFor] exists to make that call site reachable;
// NewRouter's single use of it is what this test stands in for.
func TestProductHandlerFor_CarriesTheConfiguredBounds(t *testing.T) {
	const configured = 37 * time.Second // non-default, so a stale default cannot pass
	want := openjd.ExprLimits{
		SubmissionOperations:  4321,
		SubmissionMemoryBytes: 654_321,
		TemplatePositions:     321,
		TemplateRetainedBytes: 21_000,
	}
	st := fake.New()

	h := productHandlerFor(
		Config{ExprLimits: want, ExprSubmissionDeadline: configured},
		Deps{Store: st, Submitter: &stubSubmitter{}, Products: product.NewCatalog(st)},
		newTestLogger(),
	)

	if got := h.templateValidateOptions().ExprLimits; got != want {
		t.Errorf("ExprLimits = %+v, want the operator's %+v -- product template validation "+
			"would run on the defaults whatever openjd.expr_* said", got, want)
	}
	if h.exprDeadline != configured {
		t.Errorf("exprDeadline = %s, want the configured %s", h.exprDeadline, configured)
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

// ── end to end: a real pipeline, a real deadline, a real 503 ────────────────
//
// Everything above this line stops at one link of the chain. These tests are
// the bridge sub-project H1 could not build: while EXPR was StatusInProgress
// validateExtensions rejected every EXPR-declaring template before a single
// expression was evaluated, so no HTTP request could reach the meter the
// deadline lives in, and the two halves of the proof — "the pipeline returns
// the sentinel" (internal/openjd) and "this handler maps the sentinel to 503"
// (above) — were joined only by a hand-copied error string. H2 flipped the
// status, so the whole chain is now reachable from a request.

// exprDeadlineE2ETemplate is a valid EXPR job template whose one expression is
// a CALL.
//
// THE CALL IS THE POINT, and it is the trap H1 documented. A bare literal such
// as "{{ [1, 2, 3] }}" performs no meter.charge at all, so the meter never
// samples the clock, the position resolves however long ago the deadline
// passed, and a test built on one submits happily and proves nothing while
// looking like it proves everything. len() charges, and the meter checks the
// deadline on the very first charge (see expr/meter.go's checkDeadline).
//
// Every test below pairs the expired-deadline run with a generous-deadline
// control that must be ACCEPTED. That pairing is what makes the fixture's
// meter-tripping observable rather than assumed: a fixture that never charged
// would be accepted in both runs, and the control would still pass while the
// real assertion silently stopped testing anything.
const exprDeadlineE2ETemplate = `
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: ExprDeadlineEndToEnd
steps:
- name: S
  script:
    actions:
      onRun:
        command: echo
        args: ["{{ len([1, 2, 3]) }}"]
`

// expiredDeadline is the configured duration used to force a breach.
//
// It is a duration, not an instant, because that is all an operator configures:
// the handlers turn it into an absolute time at the top of each request
// (see TestSubmitJob_DeadlineIsComputedPerRequest), so a test cannot hand them
// one already in the past. A single nanosecond is spent many times over by the
// YAML parse that runs before the first charge, so the breach is not a race:
// the alternative would need the whole parse and walk to complete inside one
// nanosecond.
const expiredDeadline = time.Nanosecond

// generousDeadline is the control's configured duration: far more than the
// fixture needs, so the only difference between the two runs is the clock.
const generousDeadline = time.Minute

// seedExprSubmitPrereqs inserts the farm and queue an EXPR submission needs,
// under the fixed IDs the requests below use.
func seedExprSubmitPrereqs(t *testing.T, st *fake.Store) {
	t.Helper()
	ctx := t.Context()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "expr-farm"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "expr-queue"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
}

// TestSubmitJob_RealSubmitterDeadlineIsA503NotA422 drives POST /api/v1/jobs
// with the real [openjd.Submitter] and an EXPR template that really reaches the
// meter.
//
// This is the assertion the stub cannot make. If the pipeline ever wrapped a
// deadline breach in a *SubmitValidationError — the client-fault type — the
// stub test and internal/openjd's would both keep passing and every deadline
// would quietly become a 422, telling submitters their perfectly valid template
// is wrong because the server happened to be busy.
func TestSubmitJob_RealSubmitterDeadlineIsA503NotA422(t *testing.T) {
	submit := func(t *testing.T, deadline time.Duration) *httptest.ResponseRecorder {
		t.Helper()
		st := fake.New()
		seedExprSubmitPrereqs(t, st)
		h := newJobHandler(st, openjd.NewSubmitter(st), &fakeScheduler{}, ws.NoopNotifier{},
			newTestLogger(), testRetryDefaults, false, deadline)

		req := newReq(t, http.MethodPost,
			"/api/v1/jobs?farm_id=farm-1&queue_id=queue-1", strings.NewReader(exprDeadlineE2ETemplate))
		req.Header.Set("Content-Type", "application/yaml")
		rr := httptest.NewRecorder()
		h.submitJob(rr, req)
		return rr
	}

	if rr := submit(t, generousDeadline); rr.Code != http.StatusCreated {
		t.Fatalf("the control submission was not accepted: status = %d, want %d — body: %s.\n"+
			"The expired-deadline assertion below is only meaningful if this fixture is "+
			"otherwise submittable; a template rejected for its own reasons would produce "+
			"a failure whatever the clock said", rr.Code, http.StatusCreated, rr.Body)
	}

	rr := submit(t, expiredDeadline)
	if rr.Code == http.StatusCreated {
		t.Fatalf("an expired deadline still accepted the submission; the fixture's expression "+
			"is probably not charging the meter (a literal charges nothing) — body: %s", rr.Body)
	}
	if rr.Code == http.StatusUnprocessableEntity {
		t.Fatalf("a real wall-clock deadline was reported as 422 (invalid template); the same "+
			"body was accepted moments ago with a longer deadline — body: %s", rr.Body)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (service unavailable) — body: %s",
			rr.Code, http.StatusServiceUnavailable, rr.Body)
	}
}

// TestSubmitProductJob_RealSubmitterDeadlineIsA503NotA422 is the same proof on
// the other submission route, whose 503 branch is a separate copy of the same
// mapping in a different handler.
func TestSubmitProductJob_RealSubmitterDeadlineIsA503NotA422(t *testing.T) {
	submit := func(t *testing.T, deadline time.Duration) *httptest.ResponseRecorder {
		t.Helper()
		st := fake.New()
		seedExprSubmitPrereqs(t, st)
		if _, err := st.CreateProduct(t.Context(), store.Product{
			Name:     "expr-product",
			Title:    "EXPR Product",
			Template: exprDeadlineE2ETemplate,
			Format:   store.TemplateFormatYAML,
		}); err != nil {
			t.Fatalf("CreateProduct: %v", err)
		}
		h := newProductHandler(product.NewCatalog(st), openjd.NewSubmitter(st), nil, st,
			newTestLogger(), false, deadline, openjd.ExprLimits{})
		r := chi.NewRouter()
		r.Post("/api/v1/products/{name}/jobs", h.submitProductJob)

		req := newReq(t, http.MethodPost, "/api/v1/products/expr-product/jobs",
			strings.NewReader(`{"farm_id":"farm-1","queue_id":"queue-1"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}

	if rr := submit(t, generousDeadline); rr.Code != http.StatusCreated {
		t.Fatalf("the control submission was not accepted: status = %d, want %d — body: %s",
			rr.Code, http.StatusCreated, rr.Body)
	}

	rr := submit(t, expiredDeadline)
	if rr.Code == http.StatusCreated {
		t.Fatalf("an expired deadline still accepted the submission — body: %s", rr.Body)
	}
	if rr.Code == http.StatusUnprocessableEntity {
		t.Fatalf("a real wall-clock deadline was reported as 422 — body: %s", rr.Body)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d — body: %s", rr.Code, http.StatusServiceUnavailable, rr.Body)
	}
}

// TestCreateProduct_RealValidationDeadlineIsA503NotA400 covers the third route
// that walks a client-supplied template, POST /api/v1/products.
//
// It does not go through a Submitter at all — [product.ValidateTemplate] runs
// the walk directly — so its 503 is a third, independent copy of the mapping,
// and its non-deadline answer is 400 rather than 422. The stub-level half is
// TestProductTemplateValidation_DeadlineIsA503 above.
func TestCreateProduct_RealValidationDeadlineIsA503NotA400(t *testing.T) {
	create := func(t *testing.T, deadline time.Duration) *httptest.ResponseRecorder {
		t.Helper()
		st := fake.New()
		h := newProductHandler(product.NewCatalog(st), openjd.NewSubmitter(st), nil, st,
			newTestLogger(), false, deadline, openjd.ExprLimits{})
		r := chi.NewRouter()
		r.Post("/api/v1/products", h.createProduct)

		req := newReq(t, http.MethodPost, "/api/v1/products", jsonBody(t, map[string]any{
			"name": "expr-product", "title": "EXPR Product", "version": "1.0.0",
			"template": exprDeadlineE2ETemplate, "format": "yaml",
		}))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}

	if rr := create(t, generousDeadline); rr.Code != http.StatusCreated {
		t.Fatalf("the control create was not accepted: status = %d, want %d — body: %s",
			rr.Code, http.StatusCreated, rr.Body)
	}

	rr := create(t, expiredDeadline)
	if rr.Code == http.StatusCreated {
		t.Fatalf("an expired deadline still accepted the template — body: %s", rr.Body)
	}
	if rr.Code == http.StatusBadRequest {
		t.Fatalf("a real wall-clock deadline was reported as 400 (bad template); the same "+
			"body was accepted moments ago with a longer deadline — body: %s", rr.Body)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d — body: %s", rr.Code, http.StatusServiceUnavailable, rr.Body)
	}
}
