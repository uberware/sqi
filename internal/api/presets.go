// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/presetlib"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
)

// PresetLibrary is the subset of *presetlib.Service the preset handlers use.
// Nil (or an unconfigured library) makes every preset endpoint respond 503.
type PresetLibrary interface {
	Configured() bool
	FetchIndex(ctx context.Context, forceRefresh bool) ([]presetlib.IndexEntry, error)
	FetchDefinition(ctx context.Context, entry presetlib.IndexEntry) (store.Product, error)
}

type presetHandler struct {
	lib     PresetLibrary
	catalog *product.Catalog
	store   store.ProductStore
	logger  *slog.Logger
}

func newPresetHandler(lib PresetLibrary, catalog *product.Catalog, st store.ProductStore, logger *slog.Logger) *presetHandler {
	return &presetHandler{lib: lib, catalog: catalog, store: st, logger: logger}
}

type presetResponse struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Version     string `json:"version"`
	Status      string `json:"status"` // not_installed | installed | update_available
}

type presetDetailResponse struct {
	presetResponse

	Template string `json:"template"`
	Format   string `json:"format"`
}

// available reports whether the preset library is wired up and configured.
func (h *presetHandler) available() bool { return h.lib != nil && h.lib.Configured() }

// installStatus reports the installation status of a preset given an index of
// installed products keyed by their OriginRef.
func installStatus(entry presetlib.IndexEntry, byRef map[string]store.Product) string {
	p, ok := byRef[entry.Name]
	if !ok {
		return "not_installed"
	}
	if p.OriginFingerprint == entry.Sha256 {
		return "installed"
	}
	return "update_available"
}

// installedByRef fetches all stored products and returns those with
// SourceInstalled keyed by their OriginRef.
func (h *presetHandler) installedByRef(ctx context.Context) (map[string]store.Product, error) {
	all, err := h.store.ListProducts(ctx)
	if err != nil {
		return nil, err
	}
	byRef := make(map[string]store.Product, len(all))
	for _, p := range all {
		if p.Source == store.SourceInstalled && p.OriginRef != "" {
			byRef[p.OriginRef] = p
		}
	}
	return byRef, nil
}

// findEntry returns the index entry with the given name, or (zero, false, nil)
// when the name is not in the index.
func (h *presetHandler) findEntry(ctx context.Context, name string) (presetlib.IndexEntry, bool, error) {
	entries, err := h.lib.FetchIndex(ctx, false)
	if err != nil {
		return presetlib.IndexEntry{}, false, err
	}
	for _, e := range entries {
		if e.Name == name {
			return e, true, nil
		}
	}
	return presetlib.IndexEntry{}, false, nil
}

func (h *presetHandler) listPresets(w http.ResponseWriter, r *http.Request) {
	if !h.available() {
		writeProblem(w, r, http.StatusServiceUnavailable, "preset library not configured")
		return
	}
	ctx := r.Context()
	entries, err := h.lib.FetchIndex(ctx, r.URL.Query().Get("refresh") == "true")
	if err != nil {
		writeProblem(w, r, http.StatusBadGateway, "failed to fetch preset library index")
		return
	}
	byRef, err := h.installedByRef(ctx)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "failed to read installed products")
		return
	}
	out := make([]presetResponse, len(entries))
	for i, e := range entries {
		out[i] = presetResponse{
			Name: e.Name, Title: e.Title, Description: e.Description,
			Category: e.Category, Version: e.Version, Status: installStatus(e, byRef),
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *presetHandler) getPreset(w http.ResponseWriter, r *http.Request) {
	if !h.available() {
		writeProblem(w, r, http.StatusServiceUnavailable, "preset library not configured")
		return
	}
	ctx := r.Context()
	name := chi.URLParam(r, "name")
	entry, ok, err := h.findEntry(ctx, name)
	if err != nil {
		writeProblem(w, r, http.StatusBadGateway, "failed to fetch preset library index")
		return
	}
	if !ok {
		writeProblem(w, r, http.StatusNotFound, "preset not found")
		return
	}
	def, err := h.lib.FetchDefinition(ctx, entry)
	if err != nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, "failed to load preset definition: "+err.Error())
		return
	}
	byRef, err := h.installedByRef(ctx)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "failed to read installed products")
		return
	}
	writeJSON(w, http.StatusOK, presetDetailResponse{
		presetResponse: presetResponse{
			Name: entry.Name, Title: def.Title, Description: def.Description,
			Category: def.Category, Version: def.Version, Status: installStatus(entry, byRef),
		},
		Template: def.Template,
		Format:   string(def.Format),
	})
}

func (h *presetHandler) installPreset(w http.ResponseWriter, r *http.Request) {
	if !h.available() {
		writeProblem(w, r, http.StatusServiceUnavailable, "preset library not configured")
		return
	}
	ctx := r.Context()
	name := chi.URLParam(r, "name")
	entry, ok, err := h.findEntry(ctx, name)
	if err != nil {
		writeProblem(w, r, http.StatusBadGateway, "failed to fetch preset library index")
		return
	}
	if !ok {
		writeProblem(w, r, http.StatusNotFound, "preset not found")
		return
	}
	def, err := h.lib.FetchDefinition(ctx, entry)
	if err != nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, "failed to load preset definition: "+err.Error())
		return
	}
	installed, created, err := h.catalog.Install(ctx, def, entry.Name, entry.Sha256)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict, "a built-in or custom product already uses that name")
			return
		}
		h.logger.ErrorContext(ctx, "presets: install failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to install preset")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, toProductResponse(installed))
}
