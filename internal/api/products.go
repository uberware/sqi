// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
)

type productHandler struct {
	catalog *product.Catalog
	logger  *slog.Logger
}

func newProductHandler(catalog *product.Catalog, logger *slog.Logger) *productHandler {
	return &productHandler{catalog: catalog, logger: logger}
}

type productResponse struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Version     string `json:"version"`
	Source      string `json:"source"`
	Template    string `json:"template"`
	Format      string `json:"format"`
}

type createProductRequest struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Version     string `json:"version"`
	Template    string `json:"template"`
	Format      string `json:"format"`
}

func toProductResponse(p store.Product) productResponse {
	return productResponse{
		Name: p.Name, Title: p.Title, Description: p.Description,
		Category: p.Category, Version: p.Version, Source: string(p.Source),
		Template: p.Template, Format: string(p.Format),
	}
}

func (h *productHandler) listProducts(w http.ResponseWriter, r *http.Request) {
	list, err := h.catalog.List(r.Context())
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "failed to list products")
		return
	}
	out := make([]productResponse, len(list))
	for i, p := range list {
		out[i] = toProductResponse(p)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *productHandler) getProduct(w http.ResponseWriter, r *http.Request) {
	p, err := h.catalog.GetByName(r.Context(), chi.URLParam(r, "name"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "product not found")
			return
		}
		writeProblem(w, r, http.StatusInternalServerError, "failed to get product")
		return
	}
	writeJSON(w, http.StatusOK, toProductResponse(p))
}

func (h *productHandler) createProduct(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeProductBody(w, r, "")
	if !ok {
		return
	}
	created, err := h.catalog.Create(r.Context(), req)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict, "a product with that name already exists")
			return
		}
		writeProblem(w, r, http.StatusInternalServerError, "failed to create product")
		return
	}
	writeJSON(w, http.StatusCreated, toProductResponse(created))
}

func (h *productHandler) updateProduct(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	req, ok := decodeProductBody(w, r, name)
	if !ok {
		return
	}
	updated, err := h.catalog.Update(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, product.ErrReadOnly):
			writeProblem(w, r, http.StatusForbidden, "built-in products are read-only")
		case errors.Is(err, store.ErrNotFound):
			writeProblem(w, r, http.StatusNotFound, "product not found")
		default:
			writeProblem(w, r, http.StatusInternalServerError, "failed to update product")
		}
		return
	}
	writeJSON(w, http.StatusOK, toProductResponse(updated))
}

func (h *productHandler) deleteProduct(w http.ResponseWriter, r *http.Request) {
	err := h.catalog.Delete(r.Context(), chi.URLParam(r, "name"))
	if err != nil {
		switch {
		case errors.Is(err, product.ErrReadOnly):
			writeProblem(w, r, http.StatusForbidden, "built-in products are read-only")
		case errors.Is(err, store.ErrNotFound):
			writeProblem(w, r, http.StatusNotFound, "product not found")
		default:
			writeProblem(w, r, http.StatusInternalServerError, "failed to delete product")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeProductBody decodes and validates a product create/update body. When
// nameOverride is non-empty (update), it replaces the body's name with the path
// name. It writes the error response and returns ok=false on failure.
func decodeProductBody(w http.ResponseWriter, r *http.Request, nameOverride string) (store.Product, bool) {
	var req createProductRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return store.Product{}, false
	}
	name := req.Name
	if nameOverride != "" {
		name = nameOverride
	}
	format := store.TemplateFormat(req.Format)
	if format == "" {
		format = store.TemplateFormatYAML
	}
	if format != store.TemplateFormatYAML && format != store.TemplateFormatJSON {
		writeProblem(w, r, http.StatusBadRequest, `format must be "yaml" or "json"`)
		return store.Product{}, false
	}
	if req.Template == "" {
		writeProblem(w, r, http.StatusBadRequest, "template is required")
		return store.Product{}, false
	}
	if err := product.ValidateTemplate(req.Template, format, true); err != nil {
		writeProblem(w, r, http.StatusBadRequest, err.Error())
		return store.Product{}, false
	}
	return store.Product{
		Name: name, Title: req.Title, Description: req.Description,
		Category: req.Category, Version: req.Version,
		Template: req.Template, Format: format,
	}, true
}
