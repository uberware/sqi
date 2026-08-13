// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/store"
)

type productHandler struct {
	catalog   *product.Catalog
	submitter jobSubmitter
	sched     *scheduler.Scheduler
	store     store.Store
	logger    *slog.Logger
	// ownerLookup validates a submit-as owner override against known users.
	// Nil disables validation (auth.validate_job_owner = false).
	ownerLookup ownerLookup
	// exprDeadline is the wall-clock allowance for one submission's expression
	// evaluation (config openjd.expr_submission_deadline); zero disables it.
	//
	// This route submits an operator-installed template rather than a
	// client-supplied one, so the 4 MiB arbitrary-template exposure POST
	// /api/v1/jobs carries is not the same here — but the PARAMETERS are the
	// client's, and phase 2 re-evaluates every expression with them bound. The
	// backstop therefore applies to both routes, on the same terms.
	exprDeadline time.Duration
}

// newProductHandler returns a productHandler wired to the given catalog,
// submitter, scheduler, and store. validateOwner controls whether a submit-as
// owner override is checked against known users (config.AuthConfig.ValidateJobOwner).
// exprDeadline is the wall-clock allowance for one submission's expression
// evaluation; zero disables the backstop.
func newProductHandler(
	catalog *product.Catalog,
	submitter jobSubmitter,
	sched *scheduler.Scheduler,
	st store.Store,
	logger *slog.Logger,
	validateOwner bool,
	exprDeadline time.Duration,
) *productHandler {
	return &productHandler{
		catalog: catalog, submitter: submitter, sched: sched, store: st, logger: logger,
		ownerLookup:  newOwnerLookup(st, validateOwner),
		exprDeadline: exprDeadline,
	}
}

// submitDeadline is [jobHandler.submitDeadline] for this route: an absolute
// instant taken per request from the configured duration, zero when none is
// configured. See that method for why the conversion is per call.
func (h *productHandler) submitDeadline() time.Time {
	if h.exprDeadline <= 0 {
		return time.Time{}
	}
	return time.Now().Add(h.exprDeadline)
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
	Control    string `json:"control"`
	Label      string `json:"label"`
	GroupLabel string `json:"group_label"`
	Decimals   *int   `json:"decimals"`
}

// pathFileFilterResponse mirrors openjd.PathFileFilter on the wire.
type pathFileFilterResponse struct {
	Label    string   `json:"label"`
	Patterns []string `json:"patterns"`
}

// itemConstraintResponse mirrors [openjd.ItemConstraint] for the wire. Item
// recurses exactly once: RFC 0007 caps list nesting at list[list[T]], so a
// third level cannot describe anything.
type itemConstraintResponse struct {
	AllowedValues []string                `json:"allowed_values"`
	MinValue      *string                 `json:"min_value"`
	MaxValue      *string                 `json:"max_value"`
	MinLength     *int                    `json:"min_length"`
	MaxLength     *int                    `json:"max_length"`
	Item          *itemConstraintResponse `json:"item"`
}

// toItemConstraintResponse converts one item: level, recursing into the inner
// one. Returns nil for a nil input so a scalar parameter serializes item as
// null rather than an empty object.
func toItemConstraintResponse(c *openjd.ItemConstraint) *itemConstraintResponse {
	if c == nil {
		return nil
	}
	return &itemConstraintResponse{
		AllowedValues: c.AllowedValues,
		MinValue:      c.MinValue,
		MaxValue:      c.MaxValue,
		MinLength:     c.MinLength,
		MaxLength:     c.MaxLength,
		Item:          toItemConstraintResponse(c.Item),
	}
}

// productParameterResponse is one parsed job parameter, including userInterface
// hints, returned by GET /products/{name}/parameters.
type productParameterResponse struct {
	Name              string                          `json:"name"`
	Type              string                          `json:"type"`
	Description       string                          `json:"description"`
	Default           *string                         `json:"default"`
	AllowedValues     []string                        `json:"allowed_values"`
	MinValue          *string                         `json:"min_value"`
	MaxValue          *string                         `json:"max_value"`
	MinLength         *int                            `json:"min_length"`
	MaxLength         *int                            `json:"max_length"`
	ObjectType        string                          `json:"object_type"`
	DataFlow          string                          `json:"data_flow"`
	UserInterface     *parameterUserInterfaceResponse `json:"user_interface"`
	FileFilters       []pathFileFilterResponse        `json:"file_filters"`
	FileFilterDefault *pathFileFilterResponse         `json:"file_filter_default"`
	// Item carries a LIST[*] parameter's per-element constraints (RFC 0007's
	// nested item: block). Nil for a scalar parameter. The web form validates
	// each element against these, so omitting them would leave half the
	// constraint model unenforceable client-side.
	Item *itemConstraintResponse `json:"item"`
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
		Item:          toItemConstraintResponse(p.Item),
	}
	if p.UserInterface != nil {
		out.UserInterface = &parameterUserInterfaceResponse{
			Control:    string(p.UserInterface.Control),
			Label:      p.UserInterface.Label,
			GroupLabel: p.UserInterface.GroupLabel,
			Decimals:   p.UserInterface.Decimals,
		}
	}
	for _, f := range p.FileFilters {
		out.FileFilters = append(out.FileFilters, pathFileFilterResponse{Label: f.Label, Patterns: f.Patterns})
	}
	if p.FileFilterDefault != nil {
		out.FileFilterDefault = &pathFileFilterResponse{
			Label:    p.FileFilterDefault.Label,
			Patterns: p.FileFilterDefault.Patterns,
		}
	}
	return out
}

// getProductParameters parses the named product's template and returns its job
// parameters (with userInterface hints) for the submission form. The template is
// stored verbatim and may be invalid; an unparseable template yields 422.
func (h *productHandler) getProductParameters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := h.catalog.GetByName(ctx, nameParam(r))
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
	p, err := h.catalog.GetByName(r.Context(), nameParam(r))
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
	name := nameParam(r)
	req, ok := decodeProductBody(w, r, name)
	if !ok {
		return
	}
	updated, err := h.catalog.Update(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, product.ErrReadOnly):
			writeProblem(w, r, http.StatusForbidden, "this product is read-only")
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
	err := h.catalog.Delete(r.Context(), nameParam(r))
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
	FarmID            string            `json:"farm_id"`
	QueueID           string            `json:"queue_id"`
	Owner             string            `json:"owner"`
	Submitter         string            `json:"submitter"`
	Priority          int               `json:"priority"`
	Project           string            `json:"project"`
	Name              string            `json:"name"`
	Parameters        map[string]string `json:"parameters"`
	MaxAttempts       *int              `json:"max_attempts"`
	RetryDelaySeconds *int              `json:"retry_delay_seconds"`
	FailureLimit      *int              `json:"failure_limit"`
	DependsOn         []string          `json:"depends_on"`
}

// submitProductJob loads the named product's template and submits a job via
// the existing openjd.Submitter pipeline.
func (h *productHandler) submitProductJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := h.catalog.GetByName(ctx, nameParam(r))
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
	if problem := validateRetryOverrides(req.MaxAttempts, req.RetryDelaySeconds, req.FailureLimit); problem != "" {
		writeProblem(w, r, http.StatusBadRequest, problem)
		return
	}

	owner, submitter, identityProblem, identityStatus := bindSubmitIdentity(ctx, h.ownerLookup, req.Owner, req.Submitter)
	if identityStatus != 0 {
		writeProblem(w, r, identityStatus, identityProblem)
		return
	}

	result, err := h.submitter.Submit(ctx, p.Template, p.Format, openjd.SubmitOptions{
		FarmID:            req.FarmID,
		QueueID:           req.QueueID,
		Owner:             owner,
		Submitter:         submitter,
		Priority:          req.Priority,
		Project:           req.Project,
		Name:              req.Name,
		Parameters:        req.Parameters,
		MaxAttempts:       req.MaxAttempts,
		RetryDelaySeconds: req.RetryDelaySeconds,
		FailureLimit:      req.FailureLimit,
		DependsOn:         req.DependsOn,
		Deadline:          h.submitDeadline(),
	})
	if err != nil {
		// The wall-clock backstop first, as a 503 — see submitJob for why this
		// outcome must not be reported as an invalid template.
		if isSubmitDeadlineError(err) {
			h.logger.ErrorContext(ctx, "products: submit exceeded its expression deadline",
				slog.Duration("deadline", h.exprDeadline), slog.Any("error", err))
			writeProblem(w, r, http.StatusServiceUnavailable,
				"template validation exceeded its time budget on this server; retry, "+
					"or ask the operator about openjd.expr_submission_deadline")
			return
		}
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

	respJob := refetchDependsOn(ctx, h.store, h.logger, result.Job, req.DependsOn, "products")
	writeJSON(w, http.StatusCreated, toJobResponse(respJob))
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
