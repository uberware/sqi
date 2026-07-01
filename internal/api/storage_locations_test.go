// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for storage-location REST handlers.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

func newStorageLocationRouter(st store.Store) chi.Router {
	h := newStorageLocationHandler(st, newTestLogger())
	r := chi.NewRouter()
	r.Post("/api/v1/storage-locations", h.createStorageLocation)
	r.Get("/api/v1/storage-locations", h.listStorageLocations)
	r.Get("/api/v1/storage-locations/{id}", h.getStorageLocation)
	r.Put("/api/v1/storage-locations/{id}", h.updateStorageLocation)
	r.Delete("/api/v1/storage-locations/{id}", h.deleteStorageLocation)
	return r
}

func seedStorageLoc(t *testing.T, st *fake.Store) store.StorageLocation {
	t.Helper()
	now := time.Now()
	loc := store.StorageLocation{
		ID:        uuid.NewString(),
		Name:      "loc-" + uuid.NewString(),
		Type:      store.StorageLocationTypeFilesystem,
		Roots:     map[string]string{"default": "/mnt/nas"},
		CreatedAt: now,
		UpdatedAt: now,
	}
	created, err := st.CreateStorageLocation(t.Context(), loc)
	if err != nil {
		t.Fatalf("seedStorageLoc: %v", err)
	}
	return created
}

// ── createStorageLocation ─────────────────────────────────────────────────────

func TestCreateStorageLocation_InvalidJSON(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/storage-locations", badJSON())
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateStorageLocation_MissingName(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/storage-locations", jsonBody(t, map[string]any{
		"type": "filesystem",
	}))
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateStorageLocation_InvalidType(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/storage-locations", jsonBody(t, map[string]any{
		"name": "loc1",
		"type": "nfs", // invalid
	}))
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateStorageLocation_InvalidName(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/storage-locations", jsonBody(t, map[string]any{
		"name": "nas shows", // space — unreferenceable via loc://
		"type": "filesystem",
	}))
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for name with a space, got %d", rr.Code)
	}
	// The bad name must not have been persisted.
	locs, err := st.ListStorageLocations(t.Context())
	if err != nil {
		t.Fatalf("ListStorageLocations: %v", err)
	}
	if len(locs) != 0 {
		t.Errorf("expected no locations persisted, got %d", len(locs))
	}
}

func TestUpdateStorageLocation_InvalidName(t *testing.T) {
	st := fake.New()
	defer st.Close()

	existing := seedStorageLoc(t, st)

	req := newReq(t, http.MethodPut, "/api/v1/storage-locations/"+existing.ID, jsonBody(t, map[string]any{
		"name": "nas/shows", // slash — unreferenceable via loc://
		"type": "filesystem",
	}))
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for name with a slash, got %d", rr.Code)
	}
}

func TestCreateStorageLocation_Conflict(t *testing.T) {
	st := fake.New()
	defer st.Close()
	loc := seedStorageLoc(t, st)

	req := newReq(t, http.MethodPost, "/api/v1/storage-locations", jsonBody(t, map[string]any{
		"name":  loc.Name,
		"roots": map[string]string{"default": "/mnt/nas"},
	}))
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
	}
}

func TestCreateStorageLocation_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/storage-locations", jsonBody(t, map[string]any{
		"name":  "new-loc",
		"roots": map[string]string{"default": "/mnt/nas"},
	}))
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateStorageLocation_S3Type(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/storage-locations", jsonBody(t, map[string]any{
		"name":  "s3-output",
		"roots": map[string]string{"default": "s3://bucket/shows"},
	}))
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── listStorageLocations ──────────────────────────────────────────────────────

func TestListStorageLocations_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	seedStorageLoc(t, st)

	req := newReq(t, http.MethodGet, "/api/v1/storage-locations", nil)
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

// ── getStorageLocation ────────────────────────────────────────────────────────

func TestGetStorageLocation_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodGet, "/api/v1/storage-locations/missing", nil)
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestGetStorageLocation_Found(t *testing.T) {
	st := fake.New()
	defer st.Close()
	loc := seedStorageLoc(t, st)

	req := newReq(t, http.MethodGet, "/api/v1/storage-locations/"+loc.ID, nil)
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

// ── updateStorageLocation ─────────────────────────────────────────────────────

func TestUpdateStorageLocation_InvalidJSON(t *testing.T) {
	st := fake.New()
	defer st.Close()
	loc := seedStorageLoc(t, st)

	req := newReq(t, http.MethodPut, "/api/v1/storage-locations/"+loc.ID, badJSON())
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestUpdateStorageLocation_MissingName(t *testing.T) {
	st := fake.New()
	defer st.Close()
	loc := seedStorageLoc(t, st)

	req := newReq(t, http.MethodPut, "/api/v1/storage-locations/"+loc.ID, jsonBody(t, map[string]any{
		"type": "filesystem",
	}))
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestUpdateStorageLocation_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPut, "/api/v1/storage-locations/missing", jsonBody(t, map[string]any{
		"name": "foo",
	}))
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestUpdateStorageLocation_Conflict(t *testing.T) {
	st := fake.New()
	defer st.Close()
	loc1 := seedStorageLoc(t, st)
	loc2 := seedStorageLoc(t, st)

	req := newReq(t, http.MethodPut, "/api/v1/storage-locations/"+loc1.ID, jsonBody(t, map[string]any{
		"name": loc2.Name,
	}))
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
	}
}

func TestUpdateStorageLocation_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	loc := seedStorageLoc(t, st)

	req := newReq(t, http.MethodPut, "/api/v1/storage-locations/"+loc.ID, jsonBody(t, map[string]any{
		"name":  loc.Name,
		"roots": map[string]string{"default": "/mnt/nas"},
	}))
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── deleteStorageLocation ─────────────────────────────────────────────────────

func TestDeleteStorageLocation_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodDelete, "/api/v1/storage-locations/missing", nil)
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestDeleteStorageLocation_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	loc := seedStorageLoc(t, st)

	req := newReq(t, http.MethodDelete, "/api/v1/storage-locations/"+loc.ID, nil)
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", rr.Code)
	}
}

// ── derived type + root validation ───────────────────────────────────────────

func TestCreateStorageLocation_RejectsType(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/storage-locations", jsonBody(t, map[string]any{
		"name":  "nas",
		"type":  "s3",
		"roots": map[string]string{"default": "/mnt/nas"},
	}))
	rr := httptest.NewRecorder()
	newStorageLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (type must not be supplied)", rr.Code)
	}
}

func TestCreateStorageLocation_DerivesType(t *testing.T) {
	cases := []struct {
		name  string
		roots map[string]string
		want  string
	}{
		{"fs", map[string]string{"default": "/mnt/nas"}, "filesystem"},
		{"s3", map[string]string{"default": "s3://bucket/shows"}, "s3"},
		{"mixed", map[string]string{"default": "/mnt/nas", "aws": "s3://bucket/shows"}, "mixed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := fake.New()
			defer st.Close()

			req := newReq(t, http.MethodPost, "/api/v1/storage-locations", jsonBody(t, map[string]any{
				"name":  "loc-" + c.name,
				"roots": c.roots,
			}))
			rr := httptest.NewRecorder()
			newStorageLocationRouter(st).ServeHTTP(rr, req)

			if rr.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
			}
			var got struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Type != c.want {
				t.Errorf("derived type = %q, want %q", got.Type, c.want)
			}
		})
	}
}

func TestCreateStorageLocation_RejectsMalformedS3Root(t *testing.T) {
	bad := map[string]string{
		"no-bucket":    "s3://",
		"single-slash": "s3:/bucket",
		"space":        "s3://bad bucket/x",
		"empty-value":  "",
	}
	for label, root := range bad {
		t.Run(label, func(t *testing.T) {
			st := fake.New()
			defer st.Close()

			req := newReq(t, http.MethodPost, "/api/v1/storage-locations", jsonBody(t, map[string]any{
				"name":  "loc",
				"roots": map[string]string{"default": root},
			}))
			rr := httptest.NewRecorder()
			newStorageLocationRouter(st).ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Errorf("root %q: status = %d, want 400", root, rr.Code)
			}
		})
	}
}
