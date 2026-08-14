// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for the preset REST handlers.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/presetlib"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// fakeLib implements api.PresetLibrary with in-memory entries + a definition map.
type fakeLib struct {
	configured bool
	entries    []presetlib.IndexEntry
	defs       map[string]store.Product // keyed by entry.Name
	defErr     error
	// lastOpts records the ValidateOptions of the most recent FetchDefinition
	// call, so a test can assert the operator's EXPR limits and this request's
	// deadline actually reach the validator rather than being dropped.
	lastOpts product.ValidateOptions
}

func (f *fakeLib) Configured() bool { return f.configured }

func (f *fakeLib) FetchIndex(_ context.Context, _ bool) ([]presetlib.IndexEntry, error) {
	if !f.configured {
		return nil, presetlib.ErrNotConfigured
	}
	return f.entries, nil
}

func (f *fakeLib) FetchDefinition(
	_ context.Context, e presetlib.IndexEntry, opts product.ValidateOptions,
) (store.Product, error) {
	f.lastOpts = opts
	if f.defErr != nil {
		return store.Product{}, f.defErr
	}
	p, ok := f.defs[e.Name]
	if !ok {
		return store.Product{}, presetlib.ErrFingerprintMismatch
	}
	return p, nil
}

// presetTestSrv bundles the HTTP router, the fake store, and the fake library.
type presetTestSrv struct {
	chi.Router

	st  *fake.Store
	lib PresetLibrary
}

// newPresetTestServer builds a chi router with the preset routes mounted.
// Pass nil for lib to simulate a deployment where no preset library is configured.
func newPresetTestServer(t *testing.T, lib PresetLibrary) *presetTestSrv {
	t.Helper()
	return newPresetTestServerWith(t, lib, openjd.ExprLimits{}, 0)
}

// newPresetTestServerWith is [newPresetTestServer] with the operator's EXPR
// budget and wall-clock allowance spelled out, for the tests that assert those
// reach the validator.
func newPresetTestServerWith(
	t *testing.T, lib PresetLibrary, limits openjd.ExprLimits, deadline time.Duration,
) *presetTestSrv {
	t.Helper()
	st := fake.New()
	catalog := product.NewCatalog(st)
	h := newPresetHandler(lib, catalog, st, newTestLogger(), limits, deadline)
	r := chi.NewRouter()
	r.Get("/api/v1/presets", h.listPresets)
	r.Get("/api/v1/presets/{name}", h.getPreset)
	r.Post("/api/v1/presets/{name}/install", h.installPreset)
	return &presetTestSrv{Router: r, st: st, lib: lib}
}

// sampleEntries returns two index entries used across tests.
func sampleEntries() []presetlib.IndexEntry {
	return []presetlib.IndexEntry{
		{
			Name: "maya-render", Title: "Maya Render", Description: "Render with Maya",
			Category: "render", Version: "1.0.0", Sha256: "aabbcc",
		},
		{
			Name: "nuke-comp", Title: "Nuke Comp", Description: "Compositing with Nuke",
			Category: "comp", Version: "2.0.0", Sha256: "ddeeff",
		},
	}
}

// sampleDef returns a minimal store.Product definition for the given entry name.
func sampleDef(name string) store.Product {
	return store.Product{
		Name:     name,
		Title:    "Test Product",
		Template: validTemplate,
		Format:   store.TemplateFormatYAML,
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestPresets_ServiceUnavailableWhenNilLib checks that all three endpoints
// return 503 when PresetLib is nil (not configured in deps).
func TestPresets_ServiceUnavailableWhenNilLib(t *testing.T) {
	srv := newPresetTestServer(t, nil)
	ctx := t.Context()

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/presets"},
		{http.MethodGet, "/api/v1/presets/some-preset"},
		{http.MethodPost, "/api/v1/presets/some-preset/install"},
	}

	for _, tc := range routes {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequestWithContext(ctx, tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", rec.Code)
			}
		})
	}
}

// TestPresets_ServiceUnavailableWhenNotConfigured checks that all three endpoints
// return 503 when PresetLib.Configured() == false.
func TestPresets_ServiceUnavailableWhenNotConfigured(t *testing.T) {
	lib := &fakeLib{configured: false}
	srv := newPresetTestServer(t, lib)
	ctx := t.Context()

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/presets"},
		{http.MethodGet, "/api/v1/presets/some-preset"},
		{http.MethodPost, "/api/v1/presets/some-preset/install"},
	}

	for _, tc := range routes {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequestWithContext(ctx, tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", rec.Code)
			}
		})
	}
}

// TestPresets_ListPresetsStatuses checks that GET /presets returns the correct
// status for not_installed, installed, and update_available presets.
func TestPresets_ListPresetsStatuses(t *testing.T) {
	entries := sampleEntries()
	lib := &fakeLib{
		configured: true,
		entries:    entries,
		defs:       map[string]store.Product{},
	}
	srv := newPresetTestServer(t, lib)
	ctx := t.Context()

	// Seed "maya-render" as installed with matching fingerprint → "installed".
	_, err := srv.st.CreateProduct(ctx, store.Product{
		ID: "1", Name: "maya-render", Source: store.SourceInstalled,
		OriginRef: "maya-render", OriginFingerprint: "aabbcc",
		Template: validTemplate, Format: store.TemplateFormatYAML,
	})
	if err != nil {
		t.Fatalf("seed maya-render: %v", err)
	}

	// Seed "nuke-comp" as installed but with a different fingerprint → "update_available".
	_, err = srv.st.CreateProduct(ctx, store.Product{
		ID: "2", Name: "nuke-comp", Source: store.SourceInstalled,
		OriginRef: "nuke-comp", OriginFingerprint: "000000",
		Template: validTemplate, Format: store.TemplateFormatYAML,
	})
	if err != nil {
		t.Fatalf("seed nuke-comp: %v", err)
	}

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/presets", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}

	byName := make(map[string]map[string]any, len(got))
	for _, p := range got {
		name, ok := p["name"].(string)
		if !ok {
			t.Fatalf("preset entry missing string 'name': %v", p)
		}
		byName[name] = p
	}

	if byName["maya-render"]["status"] != "installed" {
		t.Errorf("maya-render status = %q, want installed", byName["maya-render"]["status"])
	}
	if byName["nuke-comp"]["status"] != "update_available" {
		t.Errorf("nuke-comp status = %q, want update_available", byName["nuke-comp"]["status"])
	}
}

// TestPresets_ListPresetsNotInstalled checks that a preset without any
// installed product appears as "not_installed".
func TestPresets_ListPresetsNotInstalled(t *testing.T) {
	entries := []presetlib.IndexEntry{
		{Name: "new-preset", Title: "New", Sha256: "abc123"},
	}
	lib := &fakeLib{configured: true, entries: entries, defs: map[string]store.Product{}}
	srv := newPresetTestServer(t, lib)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/presets", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0]["status"] != "not_installed" {
		t.Errorf("status = %q, want not_installed", got[0]["status"])
	}
}

// TestPresets_GetPresetNotFound checks that GET /presets/{name} returns 404
// when the name is not in the index.
func TestPresets_GetPresetNotFound(t *testing.T) {
	lib := &fakeLib{configured: true, entries: sampleEntries(), defs: map[string]store.Product{}}
	srv := newPresetTestServer(t, lib)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/presets/ghost", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestPresets_GetPresetReturnsDetail checks that GET /presets/{name} for a
// known entry returns 200 with template and status.
func TestPresets_GetPresetReturnsDetail(t *testing.T) {
	entries := sampleEntries()
	lib := &fakeLib{
		configured: true,
		entries:    entries,
		defs:       map[string]store.Product{"maya-render": sampleDef("maya-render")},
	}
	srv := newPresetTestServer(t, lib)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/presets/maya-render", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["name"] != "maya-render" {
		t.Errorf("name = %q", got["name"])
	}
	if got["template"] == nil || got["template"] == "" {
		t.Errorf("template missing in response")
	}
	if got["status"] != "not_installed" {
		t.Errorf("status = %q, want not_installed", got["status"])
	}
}

// TestPresets_GetPresetNamespacedName checks that a preset whose name contains
// a '/' namespace separator (e.g. "testing/render-simulator") is resolvable when
// the browser URL-encodes the slash as %2F in the path. Regression test: chi
// does not decode path params, so the handler must unescape the name itself.
func TestPresets_GetPresetNamespacedName(t *testing.T) {
	entries := []presetlib.IndexEntry{
		{Name: "testing/render-simulator", Title: "Render Simulator", Category: "Testing", Version: "1.0.0", Sha256: "abc123"},
	}
	lib := &fakeLib{
		configured: true,
		entries:    entries,
		defs:       map[string]store.Product{"testing/render-simulator": sampleDef("testing/render-simulator")},
	}
	srv := newPresetTestServer(t, lib)

	// The web UI issues encodeURIComponent(name), so '/' arrives as %2F.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/presets/testing%2Frender-simulator", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["name"] != "testing/render-simulator" {
		t.Errorf("name = %q, want testing/render-simulator", got["name"])
	}
}

// TestPresets_InstallNamespacedName is the install-path counterpart to
// TestPresets_GetPresetNamespacedName: a %2F-encoded namespaced name must
// resolve and install.
func TestPresets_InstallNamespacedName(t *testing.T) {
	entries := []presetlib.IndexEntry{
		{Name: "testing/render-simulator", Sha256: "abc123"},
	}
	lib := &fakeLib{
		configured: true,
		entries:    entries,
		defs:       map[string]store.Product{"testing/render-simulator": sampleDef("testing/render-simulator")},
	}
	srv := newPresetTestServer(t, lib)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/presets/testing%2Frender-simulator/install", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body)
	}
}

// TestPresets_InstallCreates checks that POST /presets/{name}/install creates
// a new product (201) and sets the correct source/origin fields.
func TestPresets_InstallCreates(t *testing.T) {
	entries := sampleEntries()
	def := sampleDef("maya-render")
	lib := &fakeLib{
		configured: true,
		entries:    entries,
		defs:       map[string]store.Product{"maya-render": def},
	}
	srv := newPresetTestServer(t, lib)
	ctx := t.Context()

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/presets/maya-render/install", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["name"] != "maya-render" {
		t.Errorf("name = %q, want maya-render", got["name"])
	}
	if got["source"] != "installed" {
		t.Errorf("source = %q, want installed", got["source"])
	}

	// Verify the product is persisted.
	p, err := srv.st.GetProductByName(ctx, "maya-render")
	if err != nil {
		t.Fatalf("GetProductByName: %v", err)
	}
	if p.Source != store.SourceInstalled {
		t.Errorf("persisted source = %q, want installed", p.Source)
	}
	if p.OriginRef != "maya-render" {
		t.Errorf("origin_ref = %q, want maya-render", p.OriginRef)
	}
	if p.OriginFingerprint != "aabbcc" {
		t.Errorf("origin_fingerprint = %q, want aabbcc", p.OriginFingerprint)
	}
}

// TestPresets_InstallUpdatesExisting checks that installing an already-installed
// preset a second time (with a new fingerprint) returns 200, not 201.
func TestPresets_InstallUpdatesExisting(t *testing.T) {
	entry := presetlib.IndexEntry{
		Name: "maya-render", Sha256: "newfingerprint",
	}
	def := sampleDef("maya-render")
	lib := &fakeLib{
		configured: true,
		entries:    []presetlib.IndexEntry{entry},
		defs:       map[string]store.Product{"maya-render": def},
	}
	srv := newPresetTestServer(t, lib)
	ctx := t.Context()

	// Pre-seed an installed version.
	_, err := srv.st.CreateProduct(ctx, store.Product{
		ID: "existing", Name: "maya-render", Source: store.SourceInstalled,
		OriginRef: "maya-render", OriginFingerprint: "oldfingerprint",
		Template: validTemplate, Format: store.TemplateFormatYAML,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/presets/maya-render/install", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (update); body=%s", rec.Code, rec.Body)
	}
}

// TestPresets_InstallNotFound checks that POST /presets/{name}/install returns
// 404 when the name is not in the index.
func TestPresets_InstallNotFound(t *testing.T) {
	lib := &fakeLib{configured: true, entries: sampleEntries(), defs: map[string]store.Product{}}
	srv := newPresetTestServer(t, lib)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/presets/ghost/install", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestPresets_InstallFetchDefinitionError checks that a FetchDefinition error
// results in 422.
func TestPresets_InstallFetchDefinitionError(t *testing.T) {
	lib := &fakeLib{
		configured: true,
		entries:    sampleEntries(),
		defs:       map[string]store.Product{},
		defErr:     presetlib.ErrFingerprintMismatch,
	}
	srv := newPresetTestServer(t, lib)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/presets/maya-render/install", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
}

// TestPresets_GetPresetFetchDefinitionError checks that a FetchDefinition error
// on GET /presets/{name} results in 422.
func TestPresets_GetPresetFetchDefinitionError(t *testing.T) {
	lib := &fakeLib{
		configured: true,
		entries:    sampleEntries(),
		defs:       map[string]store.Product{},
		defErr:     presetlib.ErrFingerprintMismatch,
	}
	srv := newPresetTestServer(t, lib)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/presets/maya-render", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
}

// TestPresets_InstallConflict checks that POST /presets/{name}/install returns
// 409 when a custom (non-installed) product already exists with the same name,
// because the catalog refuses to overwrite a custom product with a preset.
func TestPresets_InstallConflict(t *testing.T) {
	entries := sampleEntries()
	def := sampleDef("maya-render")
	lib := &fakeLib{
		configured: true,
		entries:    entries,
		defs:       map[string]store.Product{"maya-render": def},
	}
	srv := newPresetTestServer(t, lib)
	ctx := t.Context()

	// Seed a CUSTOM product with the same name as the preset.
	_, err := srv.st.CreateProduct(ctx, store.Product{
		ID: "custom-1", Name: "maya-render", Title: "My Custom Maya",
		Source:   store.SourceCustom,
		Template: validTemplate, Format: store.TemplateFormatYAML,
	})
	if err != nil {
		t.Fatalf("seed custom product: %v", err)
	}

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/presets/maya-render/install", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

// ── EXPR bounds on the preset routes (sub-project H1, whole-wave review) ─────

// TestPresets_ValidationCarriesOperatorLimitsAndDeadline pins that a preset
// definition is validated under the OPERATOR's EXPR budget and this request's
// wall-clock allowance, not openjd.DefaultExprLimits() with no deadline.
//
// H1 fixed POST/PUT /api/v1/products and left these routes on the defaults, on
// the argument that a preset body is sha256-pinned against an operator's index
// and therefore not attacker-chosen. That argument covers the DEADLINE at best:
// the limits are operator configuration, and an operator who tightened a knob
// asked for it to be enforced wherever a template is validated. Both routes sit
// behind the same permission as POST /api/v1/products (policy.ProductsManage),
// which policy.Can grants to everyone while auth is off.
//
// Asserting on the options the handler passes — rather than on an observable
// verdict — was originally forced: while EXPR was StatusInProgress the walk
// never ran, so no limit and no deadline could change any response body, and
// there was nothing else to observe. Sub-project H2 flipped the status, so an
// observable-verdict test is now possible for these two routes (it would need
// a fake preset library serving an EXPR definition expensive enough to breach
// a limit). This test is kept as-is regardless: it pins the hop that carries
// the operator's configuration, which a verdict test would only cover
// incidentally.
func TestPresets_ValidationCarriesOperatorLimitsAndDeadline(t *testing.T) {
	limits := openjd.ExprLimits{
		SubmissionOperations:  1_234,
		SubmissionMemoryBytes: 5_678,
		TemplatePositions:     91,
		TemplateRetainedBytes: 234_567,
	}
	lib := &fakeLib{
		configured: true,
		entries:    sampleEntries(),
		defs:       map[string]store.Product{"maya-render": sampleDef("maya-render")},
	}
	srv := newPresetTestServerWith(t, lib, limits, 7*time.Second)

	for _, tt := range []struct {
		name   string
		method string
		path   string
	}{
		{"get", http.MethodGet, "/api/v1/presets/maya-render"},
		{"install", http.MethodPost, "/api/v1/presets/maya-render/install"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lib.lastOpts = product.ValidateOptions{}
			before := time.Now()
			req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil)
			srv.ServeHTTP(httptest.NewRecorder(), req)

			got := lib.lastOpts
			if got.ExprLimits != limits {
				t.Errorf("ExprLimits = %+v, want %+v (the operator's configuration "+
					"is not reaching preset validation)", got.ExprLimits, limits)
			}
			if got.Deadline.IsZero() {
				t.Fatal("Deadline is zero; the configured wall-clock allowance is " +
					"not reaching preset validation")
			}
			if got.Deadline.Before(before) || got.Deadline.After(before.Add(8*time.Second)) {
				t.Errorf("Deadline = %v, want ~7s after %v: it must be computed per "+
					"request from the configured DURATION, never stored", got.Deadline, before)
			}
			if !got.EnforceLimits {
				t.Error("EnforceLimits = false; preset validation must keep enforcing " +
					"OpenJD's quantitative limits")
			}
		})
	}
}

// TestPresets_DeadlineIsA503NotA422 pins the same 503-vs-4xx split H1 applied
// to every other route that validates a template: a wall-clock stop means this
// server gave up on a body that would validate on an idle machine, so it must
// not be reported as an unprocessable definition.
//
// The fingerprint-mismatch tests above pin the other direction — a genuine
// definition fault stays 422 — so the mapping cannot be satisfied by answering
// 503 for everything.
func TestPresets_DeadlineIsA503NotA422(t *testing.T) {
	for _, tt := range []struct {
		name   string
		method string
		path   string
	}{
		{"get", http.MethodGet, "/api/v1/presets/maya-render"},
		{"install", http.MethodPost, "/api/v1/presets/maya-render/install"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lib := &fakeLib{
				configured: true,
				entries:    sampleEntries(),
				defs:       map[string]store.Product{},
				defErr: fmt.Errorf("presetlib: product: template validation: %w",
					expr.ErrDeadlineExceeded),
			}
			srv := newPresetTestServerWith(t, lib, openjd.ExprLimits{}, 5*time.Second)

			req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", rec.Code)
			}
			if rec.Code == http.StatusUnprocessableEntity {
				t.Error("a wall-clock stop was reported as an invalid definition")
			}
		})
	}
}

// TestPresets_DeadlineErrorMatchesTheRealSentinel guards the hand-copied error
// shape the two tests above depend on.
//
// They synthesize what the pipeline returns rather than driving a real
// evaluation, which was forced while EXPR was StatusInProgress (no preset
// definition could reach an expression evaluation) and is now a choice: the
// two routes reach a real walk since sub-project H2, but driving one needs a
// fake preset library. errors.Is is what the handler uses, so the
// wrapping in defErr must actually satisfy it — if a future refactor stopped
// wrapping the sentinel, both tests above would go on passing against a shape
// production no longer produces.
func TestPresets_DeadlineErrorMatchesTheRealSentinel(t *testing.T) {
	err := fmt.Errorf("presetlib: product: template validation: %w", expr.ErrDeadlineExceeded)
	if !errors.Is(err, expr.ErrDeadlineExceeded) {
		t.Fatal("the synthesized deadline error does not match the sentinel")
	}
	if !isSubmitDeadlineError(err) {
		t.Fatal("isSubmitDeadlineError does not recognize the synthesized deadline error")
	}
}
