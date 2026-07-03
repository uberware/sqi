// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for the preset REST handlers.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

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
}

func (f *fakeLib) Configured() bool { return f.configured }

func (f *fakeLib) FetchIndex(_ context.Context, _ bool) ([]presetlib.IndexEntry, error) {
	if !f.configured {
		return nil, presetlib.ErrNotConfigured
	}
	return f.entries, nil
}

func (f *fakeLib) FetchDefinition(_ context.Context, e presetlib.IndexEntry) (store.Product, error) {
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
	st := fake.New()
	catalog := product.NewCatalog(st)
	h := newPresetHandler(lib, catalog, st, newTestLogger())
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
