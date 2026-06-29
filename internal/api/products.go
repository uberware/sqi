// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/store"
)

type productHandler struct {
	catalog   *product.Catalog
	submitter *openjd.Submitter
	sched     *scheduler.Scheduler
	logger    *slog.Logger
}

func newProductHandler(
	catalog *product.Catalog,
	submitter *openjd.Submitter,
	sched *scheduler.Scheduler,
	logger *slog.Logger,
) *productHandler {
	return &productHandler{catalog: catalog, submitter: submitter, sched: sched, logger: logger}
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

// parameterUserInterfaceResponse mirrors openjd.ParameterUserInterface for the
// GET /products/{name}/parameters response.
type parameterUserInterfaceResponse struct {
	Control           string `json:"control"`
	Label             string `json:"label"`
	GroupLabel        string `json:"group_label"`
	Decimals          *int   `json:"decimals"`
	SingleStepRemoval *bool  `json:"single_step_removal"`
}

// productParameterResponse is one parsed job parameter, including userInterface
// hints, returned by GET /products/{name}/parameters.
type productParameterResponse struct {
	Name          string                          `json:"name"`
	Type          string                          `json:"type"`
	Description   string                          `json:"description"`
	Default       *string                         `json:"default"`
	AllowedValues []string                        `json:"allowed_values"`
	MinValue      *string                         `json:"min_value"`
	MaxValue      *string                         `json:"max_value"`
	MinLength     *int                            `json:"min_length"`
	MaxLength     *int                            `json:"max_length"`
	ObjectType    string                          `json:"object_type"`
	DataFlow      string                          `json:"data_flow"`
	UserInterface *parameterUserInterfaceResponse `json:"user_interface"`
}

func toProductParameterResponse(p openjd.JobParameter) productParameterResponse {
	out := productParameterResponse{
		Name:          p.Name,
		Type:          string(p.Type),
		Description:   p.Description,
		Default:       p.Default,
		AllowedValues: p.AllowedValues,
		MinValue:      p.MinValue,
		MaxValue:      p.MaxValue,
		MinLength:     p.MinLength,
		MaxLength:     p.MaxLength,
		ObjectType:    string(p.ObjectType),
		DataFlow:      string(p.DataFlow),
	}
	if p.UserInterface != nil {
		out.UserInterface = &parameterUserInterfaceResponse{
			Control:           string(p.UserInterface.Control),
			Label:             p.UserInterface.Label,
			GroupLabel:        p.UserInterface.GroupLabel,
			Decimals:          p.UserInterface.Decimals,
			SingleStepRemoval: p.UserInterface.SingleStepRemoval,
		}
	}
	return out
}

// getProductParameters parses the named product's template and returns its job
// parameters (with userInterface hints) for the submission form. The template is
// stored verbatim and may be invalid; an unparseable template yields 422.
func (h *productHandler) getProductParameters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := h.catalog.GetByName(ctx, chi.URLParam(r, "name"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "product not found")
			return
		}
		writeProblem(w, r, http.StatusInternalServerError, "failed to load product")
		return
	}

	parseFormat := openjd.FormatYAML
	if p.Format == store.TemplateFormatJSON {
		parseFormat = openjd.FormatJSON
	}
	tmpl, parseErr := openjd.Parse([]byte(p.Template), parseFormat)
	if parseErr != nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, "product template is invalid: "+parseErr.Error())
		return
	}

	out := make([]productParameterResponse, len(tmpl.ParameterDefinitions))
	for i, param := range tmpl.ParameterDefinitions {
		out[i] = toProductParameterResponse(param)
	}
	writeJSON(w, http.StatusOK, out)
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

// submitProductRequest is the JSON body accepted by POST /api/v1/products/{name}/jobs.
type submitProductRequest struct {
	FarmID     string            `json:"farm_id"`
	QueueID    string            `json:"queue_id"`
	Owner      string            `json:"owner"`
	Submitter  string            `json:"submitter"`
	Priority   int               `json:"priority"`
	Project    string            `json:"project"`
	Name       string            `json:"name"`
	Parameters map[string]string `json:"parameters"`
}

// submitProductJob loads the named product's template and submits a job via
// the existing openjd.Submitter pipeline.
func (h *productHandler) submitProductJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := h.catalog.GetByName(ctx, chi.URLParam(r, "name"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "product not found")
			return
		}
		writeProblem(w, r, http.StatusInternalServerError, "failed to load product")
		return
	}

	var req submitProductRequest
	if decErr := json.NewDecoder(r.Body).Decode(&req); decErr != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.FarmID == "" || req.QueueID == "" {
		writeProblem(w, r, http.StatusBadRequest, "farm_id and queue_id are required")
		return
	}

	result, err := h.submitter.Submit(ctx, p.Template, p.Format, openjd.SubmitOptions{
		FarmID:     req.FarmID,
		QueueID:    req.QueueID,
		Owner:      req.Owner,
		Submitter:  req.Submitter,
		Priority:   req.Priority,
		Project:    req.Project,
		Name:       req.Name,
		Parameters: req.Parameters,
	})
	if err != nil {
		if isSubmitValidationError(err) {
			writeProblem(w, r, http.StatusUnprocessableEntity, err.Error())
			return
		}
		h.logger.ErrorContext(ctx, "products: submit failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create job")
		return
	}
	if h.sched != nil {
		h.sched.WakeQueue(result.Job.QueueID)
	}
	writeJSON(w, http.StatusCreated, toJobResponse(result.Job))
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
	if name == "" {
		writeProblem(w, r, http.StatusBadRequest, "name is required")
		return store.Product{}, false
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
