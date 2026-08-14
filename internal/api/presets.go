// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/presetlib"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
)

// PresetLibrary is the subset of *presetlib.Service the preset handlers use.
// Nil (or an unconfigured library) makes every preset endpoint respond 503.
type PresetLibrary interface {
	Configured() bool
	FetchIndex(ctx context.Context, forceRefresh bool) ([]presetlib.IndexEntry, error)
	FetchDefinition(
		ctx context.Context, entry presetlib.IndexEntry, opts product.ValidateOptions,
	) (store.Product, error)
}

type presetHandler struct {
	lib     PresetLibrary
	catalog *product.Catalog
	store   store.ProductStore
	logger  *slog.Logger
	// exprLimits is the operator's configured EXPR budget (config
	// openjd.expr_*), and exprDeadline this route's wall-clock allowance
	// (openjd.expr_submission_deadline; zero disables it).
	//
	// A preset body is sha256-pinned against an operator-configured index, so
	// unlike POST /api/v1/products it is not client-chosen content — which is
	// why EXPR sub-project H1 originally left this path on
	// openjd.DefaultExprLimits() with no deadline. H1's whole-branch review
	// rejected that: POST /api/v1/presets/{name}/install is behind the SAME
	// permission as POST /api/v1/products (policy.ProductsManage), both end in
	// a catalog write, and policy.Can grants everything with auth off — so the
	// two routes differed for no reason a reader could act on. The limits are
	// operator configuration and must be honored wherever validation happens;
	// the deadline bounds a walk that an anonymous caller can trigger as often
	// as it likes, GET /api/v1/presets/{name} included.
	exprLimits   openjd.ExprLimits
	exprDeadline time.Duration
}

func newPresetHandler(
	lib PresetLibrary,
	catalog *product.Catalog,
	st store.ProductStore,
	logger *slog.Logger,
	exprLimits openjd.ExprLimits,
	exprDeadline time.Duration,
) *presetHandler {
	return &presetHandler{
		lib: lib, catalog: catalog, store: st, logger: logger,
		exprLimits: exprLimits, exprDeadline: exprDeadline,
	}
}

// validateOptions bounds ONE preset definition's validation: the operator's
// EXPR budget, plus this request's share of wall-clock time taken per call via
// [exprDeadlineAt] and never stored (see [openjd.ExprLimits]' Deadline field
// for what storing one on a long-lived value does).
func (h *presetHandler) validateOptions() product.ValidateOptions {
	return product.ValidateOptions{
		EnforceLimits: true,
		ExprLimits:    h.exprLimits,
		Deadline:      exprDeadlineAt(h.exprDeadline),
	}
}

// writeDefinitionProblem reports a [PresetLibrary.FetchDefinition] failure.
//
// The wall-clock backstop is a 503 and everything else keeps this route's
// existing 422, on the same terms as every other template ingress: a deadline
// means this server gave up on a body that would validate on an idle machine,
// and reporting that as an unprocessable definition would make acceptance
// depend on machine load.
func (h *presetHandler) writeDefinitionProblem(w http.ResponseWriter, r *http.Request, err error) {
	// The detail differs from [exprDeadlineProblemDetail] on purpose: what the
	// caller asked this server to load is a preset definition, not a template
	// it supplied, so naming a template would misdescribe the thing that ran
	// out of time.
	const detail = "preset definition validation exceeded its time budget on this server; retry, " +
		"or ask the operator about openjd.expr_submission_deadline"
	if writeExprDeadlineProblem(w, r, h.logger, err,
		"presets: definition validation exceeded its expression deadline",
		detail, h.exprDeadline) {
		return
	}
	writeProblem(w, r, http.StatusUnprocessableEntity, "failed to load preset definition: "+err.Error())
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
	name := nameParam(r)
	entry, ok, err := h.findEntry(ctx, name)
	if err != nil {
		writeProblem(w, r, http.StatusBadGateway, "failed to fetch preset library index")
		return
	}
	if !ok {
		writeProblem(w, r, http.StatusNotFound, "preset not found")
		return
	}
	def, err := h.lib.FetchDefinition(ctx, entry, h.validateOptions())
	if err != nil {
		h.writeDefinitionProblem(w, r, err)
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
	name := nameParam(r)
	entry, ok, err := h.findEntry(ctx, name)
	if err != nil {
		writeProblem(w, r, http.StatusBadGateway, "failed to fetch preset library index")
		return
	}
	if !ok {
		writeProblem(w, r, http.StatusNotFound, "preset not found")
		return
	}
	def, err := h.lib.FetchDefinition(ctx, entry, h.validateOptions())
	if err != nil {
		h.writeDefinitionProblem(w, r, err)
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
