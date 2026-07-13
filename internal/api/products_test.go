// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for product REST handlers.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

const validTemplate = `specificationVersion: jobtemplate-2023-09
name: Demo
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: echo
          args: ["hi"]`

// productTestSrv bundles the HTTP handler and the underlying fake store so
// tests can both serve requests and seed the store directly.
type productTestSrv struct {
	chi.Router

	st *fake.Store
}

// newProductTestServer creates a fake-backed product test server with the
// Submitter wired in (needed for submit-by-product tests).
func newProductTestServer(t *testing.T) *productTestSrv {
	t.Helper()
	st := fake.New()
	h := newProductHandler(product.NewCatalog(st), openjd.NewSubmitter(st), nil, st, newTestLogger())
	r := chi.NewRouter()
	r.Get("/api/v1/products", h.listProducts)
	r.Post("/api/v1/products", h.createProduct)
	r.Get("/api/v1/products/{name}", h.getProduct)
	r.Put("/api/v1/products/{name}", h.updateProduct)
	r.Delete("/api/v1/products/{name}", h.deleteProduct)
	r.Post("/api/v1/products/{name}/jobs", h.submitProductJob)
	r.Get("/api/v1/products/{name}/parameters", h.getProductParameters)
	return &productTestSrv{Router: r, st: st}
}

// newProductRouter builds a product router for CRUD-only tests (no Submitter
// needed). Also registers the submit route so route resolution is consistent.
func newProductRouter(st store.Store) chi.Router {
	h := newProductHandler(product.NewCatalog(st), openjd.NewSubmitter(st), nil, st, newTestLogger())
	r := chi.NewRouter()
	r.Get("/api/v1/products", h.listProducts)
	r.Post("/api/v1/products", h.createProduct)
	r.Get("/api/v1/products/{name}", h.getProduct)
	r.Put("/api/v1/products/{name}", h.updateProduct)
	r.Delete("/api/v1/products/{name}", h.deleteProduct)
	r.Post("/api/v1/products/{name}/jobs", h.submitProductJob)
	r.Get("/api/v1/products/{name}/parameters", h.getProductParameters)
	return r
}

// seedProductSubmitPrereqs inserts a farm and queue into the test server's
// fake store and returns their IDs.
func seedProductSubmitPrereqs(t *testing.T, srv *productTestSrv) (farmID, queueID string) {
	t.Helper()
	ctx := t.Context()

	farm, err := srv.st.CreateFarm(ctx, store.Farm{
		ID:   uuid.NewString(),
		Name: "test-farm-" + uuid.NewString()[:8],
	})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := srv.st.CreateQueue(ctx, store.Queue{
		ID:     uuid.NewString(),
		FarmID: farm.ID,
		Name:   "test-queue-" + uuid.NewString()[:8],
	})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	return farm.ID, queue.ID
}

func TestProducts_ListIncludesBuiltins(t *testing.T) {
	srv := newProductRouter(fake.New())
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/products", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) < 3 {
		t.Fatalf("expected >=3 built-ins, got %d", len(got))
	}
}

func TestProducts_CreateThenGet(t *testing.T) {
	srv := newProductRouter(fake.New())
	req := newReq(t, http.MethodPost, "/api/v1/products", jsonBody(t, map[string]any{
		"name": "mine", "title": "Mine", "version": "1.0.0",
		"template": validTemplate, "format": "yaml",
	}))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body)
	}
	req = newReq(t, http.MethodGet, "/api/v1/products/mine", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
}

// TestProducts_NamespacedNameRoundTrip checks that a product whose name has a
// '/' namespace (e.g. an installed preset "testing/render-simulator") is
// retrievable when the browser URL-encodes the slash as %2F. Regression test:
// chi does not decode path params, so the handler must unescape the name.
func TestProducts_NamespacedNameRoundTrip(t *testing.T) {
	srv := newProductRouter(fake.New())
	req := newReq(t, http.MethodPost, "/api/v1/products", jsonBody(t, map[string]any{
		"name": "testing/render-simulator", "title": "Render Simulator", "version": "1.0.0",
		"template": validTemplate, "format": "yaml",
	}))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body)
	}

	// encodeURIComponent("testing/render-simulator") => testing%2Frender-simulator
	for _, path := range []string{
		"/api/v1/products/testing%2Frender-simulator",
		"/api/v1/products/testing%2Frender-simulator/parameters",
	} {
		rec = httptest.NewRecorder()
		srv.ServeHTTP(rec, newReq(t, http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200; body=%s", path, rec.Code, rec.Body)
		}
	}
}

func TestProducts_CreateBadTemplateIs400(t *testing.T) {
	srv := newProductRouter(fake.New())
	req := newReq(t, http.MethodPost, "/api/v1/products", jsonBody(t, map[string]any{
		"name": "bad", "title": "Bad",
		"template": "specificationVersion: nope\nsteps: []", "format": "yaml",
	}))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestProducts_GetUnknownIs404(t *testing.T) {
	srv := newProductRouter(fake.New())
	req := newReq(t, http.MethodGet, "/api/v1/products/ghost", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestProducts_MutateBuiltinIs403(t *testing.T) {
	srv := newProductRouter(fake.New())
	req := newReq(t, http.MethodDelete, "/api/v1/products/script", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestProducts_SubmitBuiltin(t *testing.T) {
	srv := newProductTestServer(t)
	farmID, queueID := seedProductSubmitPrereqs(t, srv)

	body := jsonBody(t, map[string]any{
		"farm_id": farmID, "queue_id": queueID,
		"parameters": map[string]string{"Command": "echo hi"},
	})
	req := newReq(t, http.MethodPost, "/api/v1/products/script/jobs", bytes.NewReader(body.Bytes()))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("submit status = %d body=%s", rec.Code, rec.Body)
	}
}

func TestProducts_SubmitUnknownProductIs404(t *testing.T) {
	srv := newProductTestServer(t)
	body := jsonBody(t, map[string]any{"farm_id": "f", "queue_id": "q"})
	req := newReq(t, http.MethodPost, "/api/v1/products/ghost/jobs", bytes.NewReader(body.Bytes()))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestProducts_UpdatePreservesSource(t *testing.T) {
	srv := newProductRouter(fake.New())

	// Create a custom product — source is stamped as "custom" by the catalog.
	createBody := jsonBody(t, map[string]any{
		"name": "my-product", "title": "Original", "version": "1.0.0",
		"template": validTemplate, "format": "yaml",
	})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newReq(t, http.MethodPost, "/api/v1/products", createBody))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body)
	}

	// PUT with a changed title — source must not change.
	putBody := jsonBody(t, map[string]any{
		"title": "Updated", "version": "1.0.0",
		"template": validTemplate, "format": "yaml",
	})
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, newReq(t, http.MethodPut, "/api/v1/products/my-product", putBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("put status = %d body=%s", rec.Code, rec.Body)
	}

	// GET and verify: title changed, source still "custom".
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, newReq(t, http.MethodGet, "/api/v1/products/my-product", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["source"] != "custom" {
		t.Errorf("source = %q, want %q", got["source"], "custom")
	}
	if got["title"] != "Updated" {
		t.Errorf("title = %q, want %q", got["title"], "Updated")
	}
}

func TestProducts_UpdateUnknownIs404(t *testing.T) {
	srv := newProductRouter(fake.New())
	putBody := jsonBody(t, map[string]any{
		"title": "X", "version": "1.0.0",
		"template": validTemplate, "format": "yaml",
	})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newReq(t, http.MethodPut, "/api/v1/products/ghost", putBody))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestProducts_ParametersForBuiltin(t *testing.T) {
	srv := newProductTestServer(t)
	// "python" built-in declares job parameters; assert the endpoint parses them.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/products/python/parameters", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []productParameterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("expected at least one parameter for the python built-in")
	}
}

func TestProducts_ParametersRoundTripUserInterface(t *testing.T) {
	srv := newProductTestServer(t)
	tmpl := `specificationVersion: jobtemplate-2023-09
name: UIDemo
parameterDefinitions:
  - name: Quality
    type: STRING
    default: final
    allowedValues: [draft, final]
    userInterface:
      control: DROPDOWN_LIST
      label: Render quality
      groupLabel: Output
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: echo
          args: ["{{Param.Quality}}"]`
	body, err := json.Marshal(map[string]string{"name": "ui-demo", "title": "UI Demo", "template": tmpl, "format": "yaml"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/products", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d; body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/products/ui-demo/parameters", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var got []productParameterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	p := got[0]
	if p.Name != "Quality" || p.Type != "STRING" || p.Default == nil || *p.Default != "final" {
		t.Fatalf("param fields wrong: %+v", p)
	}
	if p.UserInterface == nil || p.UserInterface.Control != "DROPDOWN_LIST" ||
		p.UserInterface.Label != "Render quality" || p.UserInterface.GroupLabel != "Output" {
		t.Fatalf("userInterface wrong: %+v", p.UserInterface)
	}
	if len(p.AllowedValues) != 2 {
		t.Fatalf("allowedValues = %v", p.AllowedValues)
	}
}

func TestProducts_ParametersUnknownIs404(t *testing.T) {
	srv := newProductTestServer(t)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/products/nope/parameters", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestProducts_ParametersInvalidTemplateIs422(t *testing.T) {
	srv := newProductTestServer(t)
	// Seed a product whose template is stored but unparseable. createProduct
	// validates, so insert directly via the fake store to bypass validation.
	if _, err := srv.st.CreateProduct(t.Context(), store.Product{
		Name: "broken", Title: "Broken", Source: store.SourceCustom,
		Template: "::: not yaml :::\n\tbad", Format: store.TemplateFormatYAML,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/products/broken/parameters", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestProducts_SubmitWithNameOverride(t *testing.T) {
	srv := newProductTestServer(t)
	farmID, queueID := seedProductSubmitPrereqs(t, srv)
	// python built-in requires Script; supply it so submission succeeds.
	body, err := json.Marshal(map[string]any{
		"farm_id":    farmID,
		"queue_id":   queueID,
		"name":       "Shot010 v3",
		"parameters": map[string]string{"Script": "print('hello')"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/products/python/jobs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var job jobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if job.Name != "Shot010 v3" {
		t.Fatalf("job name = %q, want override", job.Name)
	}
}

func TestProducts_SubmitWithRetryOverrides(t *testing.T) {
	srv := newProductTestServer(t)
	farmID, queueID := seedProductSubmitPrereqs(t, srv)
	body := jsonBody(t, map[string]any{
		"farm_id":             farmID,
		"queue_id":            queueID,
		"parameters":          map[string]string{"Script": "print('hi')"},
		"max_attempts":        5,
		"retry_delay_seconds": 30,
		"failure_limit":       7,
	})
	req := newReq(t, http.MethodPost, "/api/v1/products/python/jobs", bytes.NewReader(body.Bytes()))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var job jobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if job.MaxAttempts == nil || *job.MaxAttempts != 5 {
		t.Errorf("max_attempts = %v, want 5", job.MaxAttempts)
	}
	if job.RetryDelaySeconds == nil || *job.RetryDelaySeconds != 30 {
		t.Errorf("retry_delay_seconds = %v, want 30", job.RetryDelaySeconds)
	}
	if job.FailureLimit == nil || *job.FailureLimit != 7 {
		t.Errorf("failure_limit = %v, want 7", job.FailureLimit)
	}
}

// TestProducts_SubmitWithDependsOn_ReturnsBlocked verifies that submitting a
// product job with depends_on pointing at a still-pending upstream job
// creates the new job in the "blocked" status and echoes depends_on in the
// create response, mirroring TestSubmitJob_DependsOn_ReturnsBlocked for raw
// job submission.
func TestProducts_SubmitWithDependsOn_ReturnsBlocked(t *testing.T) {
	srv := newProductTestServer(t)
	farmID, queueID := seedProductSubmitPrereqs(t, srv)

	// Submit the upstream job and leave it pending (no worker completes it).
	upBody := jsonBody(t, map[string]any{
		"farm_id": farmID, "queue_id": queueID,
		"parameters": map[string]string{"Command": "echo up"},
	})
	upReq := newReq(t, http.MethodPost, "/api/v1/products/script/jobs", bytes.NewReader(upBody.Bytes()))
	upRR := httptest.NewRecorder()
	srv.ServeHTTP(upRR, upReq)
	if upRR.Code != http.StatusCreated {
		t.Fatalf("upstream submit: expected 201, got %d — body: %s", upRR.Code, upRR.Body)
	}
	var up jobResponse
	if err := json.Unmarshal(upRR.Body.Bytes(), &up); err != nil {
		t.Fatalf("decode upstream: %v", err)
	}

	depBody := jsonBody(t, map[string]any{
		"farm_id": farmID, "queue_id": queueID,
		"parameters": map[string]string{"Command": "echo dep"},
		"depends_on": []string{up.ID},
	})
	depReq := newReq(t, http.MethodPost, "/api/v1/products/script/jobs", bytes.NewReader(depBody.Bytes()))
	depRR := httptest.NewRecorder()
	srv.ServeHTTP(depRR, depReq)
	if depRR.Code != http.StatusCreated {
		t.Fatalf("dependent submit: expected 201, got %d — body: %s", depRR.Code, depRR.Body)
	}
	var dep jobResponse
	if err := json.Unmarshal(depRR.Body.Bytes(), &dep); err != nil {
		t.Fatalf("decode dependent: %v", err)
	}
	if dep.Status != "blocked" {
		t.Errorf("status = %q, want blocked", dep.Status)
	}
	if len(dep.DependsOn) != 1 || dep.DependsOn[0] != up.ID {
		t.Errorf("depends_on = %v, want [%s]", dep.DependsOn, up.ID)
	}
}

// TestProducts_SubmitWithDependsOn_MissingUpstreamIs422 verifies that
// depends_on referencing a nonexistent job is rejected with 422 (validation
// error), same as raw job submission.
func TestProducts_SubmitWithDependsOn_MissingUpstreamIs422(t *testing.T) {
	srv := newProductTestServer(t)
	farmID, queueID := seedProductSubmitPrereqs(t, srv)

	body := jsonBody(t, map[string]any{
		"farm_id": farmID, "queue_id": queueID,
		"parameters": map[string]string{"Command": "echo hi"},
		"depends_on": []string{"nope"},
	})
	req := newReq(t, http.MethodPost, "/api/v1/products/script/jobs", bytes.NewReader(body.Bytes()))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 — body: %s", rec.Code, rec.Body)
	}
}

// TestProducts_SubmitRejectsInvalidRetryOverrides asserts the product submit
// endpoint applies the same retry-override bounds as direct job submission.
func TestProducts_SubmitRejectsInvalidRetryOverrides(t *testing.T) {
	srv := newProductTestServer(t)
	farmID, queueID := seedProductSubmitPrereqs(t, srv)
	body := jsonBody(t, map[string]any{
		"farm_id":      farmID,
		"queue_id":     queueID,
		"parameters":   map[string]string{"Script": "print('hi')"},
		"max_attempts": 0,
	})
	req := newReq(t, http.MethodPost, "/api/v1/products/python/jobs", bytes.NewReader(body.Bytes()))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 — body: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "max_attempts must be >= 1") {
		t.Errorf("body %q missing bound message", rec.Body.String())
	}
}
