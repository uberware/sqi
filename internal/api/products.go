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
	submitter JobSubmitter
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
	// exprLimits is the operator's configured EXPR budget
	// (config openjd.expr_*), applied when this handler validates a
	// client-supplied product template on POST/PUT /api/v1/products.
	//
	// The job routes get theirs from the Submitter built at boot; this route
	// does not go through a Submitter at all, so before H1 it silently
	// validated on openjd.DefaultExprLimits() whatever the operator had
	// configured. The zero value still means "the defaults".
	//
	// NEVER SET [openjd.ExprLimits.Deadline] ON THIS FIELD. It is built once by
	// productHandlerFor and reused for every request, so a deadline stored here
	// would be a single absolute instant that refuses everything once it
	// passed — the same trap [openjd.SubmitterOptions] carries, and the reason
	// exprDeadline above is a DURATION. It is inert today only because
	// [openjd.ValidateWithBudget] overwrites the field from
	// ValidateOptions.Deadline, which is a property of that call site rather
	// than a guarantee. templateValidateOptions computes the instant per
	// request.
	exprLimits openjd.ExprLimits
}

// newProductHandler returns a productHandler wired to the given catalog,
// submitter, scheduler, and store. validateOwner controls whether a submit-as
// owner override is checked against known users (config.AuthConfig.ValidateJobOwner).
// exprDeadline is the wall-clock allowance for one submission's expression
// evaluation; zero disables the backstop. exprLimits is the operator's EXPR
// budget for the client-supplied templates this route's create/update handlers
// validate; the zero value means the defaults.
func newProductHandler(
	catalog *product.Catalog,
	submitter JobSubmitter,
	sched *scheduler.Scheduler,
	st store.Store,
	logger *slog.Logger,
	validateOwner bool,
	exprDeadline time.Duration,
	exprLimits openjd.ExprLimits,
) *productHandler {
	return &productHandler{
		catalog: catalog, submitter: submitter, sched: sched, store: st, logger: logger,
		ownerLookup:  newOwnerLookup(st, validateOwner),
		exprDeadline: exprDeadline,
		exprLimits:   exprLimits,
	}
}

// productHandlerFor builds the product handler from cfg and deps.
//
// Extracted from [NewRouter] so the Config -> handler mapping is reachable from
// a test. NewRouter returns a chi.Mux and nothing else, so a cfg field dropped
// from that call site is invisible to every test in this package — and two of
// the fields it passes, ExprSubmissionDeadline and ExprLimits, control bounds
// whose absence changes no observable behavior until sub-project H2 flips EXPR
// to StatusSupported. Same reasoning as internal/server's routerConfig.
func productHandlerFor(cfg Config, deps Deps, logger *slog.Logger) *productHandler {
	return newProductHandler(deps.Products, deps.Submitter, deps.Scheduler, deps.Store, logger,
		cfg.ValidateJobOwner, cfg.ExprSubmissionDeadline, cfg.ExprLimits)
}

// templateValidateOptions bounds ONE validation of a client-supplied product
// template: the operator's EXPR budget, plus this request's share of wall-clock
// time.
//
// POST /api/v1/products and PUT /api/v1/products/{name} are the third route
// that accepts an arbitrary template body, alongside the two job-submission
// routes — and with auth off (the default) they are anonymous. Without the
// deadline this route would be the one place a template could be walked with no
// bound on elapsed time at all, which is precisely the exposure sub-project H1
// exists to close.
func (h *productHandler) templateValidateOptions() product.ValidateOptions {
	return product.ValidateOptions{
		EnforceLimits: true,
		ExprLimits:    h.exprLimits,
		Deadline:      exprDeadlineAt(h.exprDeadline),
	}
}

// writeTemplateProblem reports a [product.ValidateTemplate] failure.
//
// The wall-clock backstop is a 503 and everything else is a 400, on the same
// terms as the submission routes' 503-vs-422 split: a deadline means this
// server gave up, and the same body would validate on an idle machine, so
// reporting it as a malformed template would make acceptance depend on machine
// load. The status differs from the submission routes' 422 only because this
// endpoint has always answered 400 for a bad template.
func (h *productHandler) writeTemplateProblem(w http.ResponseWriter, r *http.Request, err error) {
	if writeExprDeadlineProblem(w, r, h.logger, err,
		"products: template validation exceeded its expression deadline",
		exprDeadlineProblemDetail, h.exprDeadline) {
		return
	}
	writeProblem(w, r, http.StatusBadRequest, err.Error())
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
	req, ok := h.decodeProductBody(w, r, "")
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
	req, ok := h.decodeProductBody(w, r, name)
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
		Deadline:          exprDeadlineAt(h.exprDeadline),
	})
	if err != nil {
		// The wall-clock backstop first, as a 503 — see
		// writeExprDeadlineProblem for why this outcome must not be reported as
		// an invalid template.
		if writeExprDeadlineProblem(w, r, h.logger, err,
			"products: submit exceeded its expression deadline",
			exprDeadlineProblemDetail, h.exprDeadline) {
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
//
// A method since H1: the template it validates is client-supplied, so the
// validation is bounded by the handler's configured EXPR limits and this
// request's deadline.
func (h *productHandler) decodeProductBody(w http.ResponseWriter, r *http.Request, nameOverride string) (store.Product, bool) {
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
	if err := product.ValidateTemplate(req.Template, format, h.templateValidateOptions()); err != nil {
		h.writeTemplateProblem(w, r, err)
		return store.Product{}, false
	}
	return store.Product{
		Name: name, Title: req.Title, Description: req.Description,
		Category: req.Category, Version: req.Version,
		Template: req.Template, Format: format,
	}, true
}
