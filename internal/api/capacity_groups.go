// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Capacity-group REST handlers.
//
// Route summary:
//
//	GET    /api/v1/capacity-groups                              — list groups with their status
//	POST   /api/v1/capacity-groups                              — create a group
//	GET    /api/v1/capacity-groups/{id}                         — get a group with its status
//	PUT    /api/v1/capacity-groups/{id}                         — replace a group's settings
//	DELETE /api/v1/capacity-groups/{id}                         — delete a group with no members
//	GET    /api/v1/capacity-groups/{id}/members                 — list members
//	PUT    /api/v1/capacity-groups/{id}/members/{workerID}      — add a worker (named groups)
//	DELETE /api/v1/capacity-groups/{id}/members/{workerID}      — remove a worker
//	GET    /api/v1/capacity-groups/{id}/tokens                  — list the group's join tokens
//	POST   /api/v1/capacity-groups/{id}/tokens                  — mint a join token (sized groups)
//	DELETE /api/v1/capacity-groups/{id}/tokens/{tokenID}        — revoke a join token
//
// Reads need infra.read; everything else, token listing included, needs
// infra.manage.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth/policy"
	"github.com/uberware/sqi/internal/capacity"
	"github.com/uberware/sqi/internal/store"
)

// Group-token lifetime. A group token is reusable, so its expiry is the only
// bound on how long a leaked one stays useful.
const (
	defaultGroupTokenTTL = 720 * time.Hour  // 30 days
	maxGroupTokenTTL     = 2160 * time.Hour // 90 days
)

// Defaults for the fields a request may leave out.
const (
	defaultCapacityMaxStepUp = 1
	defaultCapacityCooldown  = 60
	defaultCapacityTimeout   = 600
)

// maxCapacityGroupQueues bounds queue_ids, each of which costs a store read to
// validate.
const maxCapacityGroupQueues = 1000

// maxCapacityDurationSeconds bounds the *_seconds fields at one year, well
// inside what a time.Duration holds.
const maxCapacityDurationSeconds = 365 * 24 * 3600

// capacityBodyLimit bounds a request body.
const capacityBodyLimit = 1 << 20

// errEphemeralMemberRemoval is the 409 detail for removing an ephemeral member
// from its group.
const errEphemeralMemberRemoval = "an ephemeral member rejoins its group at its next registration, " +
	"so it cannot be removed from it; stop the worker and remove it (DELETE /api/v1/workers/{id}), " +
	"which also revokes its credential"

// errCapacityGroupNameTaken is the 409 detail for a create or rename onto a
// name another group holds.
const errCapacityGroupNameTaken = "a capacity group with that name already exists"

// errGroupTokensNeedBrokerAuth is the 409 detail for minting a group token on a
// farm whose broker does not authenticate workers.
const errGroupTokensNeedBrokerAuth = "group tokens need nats.auth.enabled; workers on an auth-off farm join with worker.capacity_group" //nolint:gosec // G101: a static response string, not a credential

// CapacityStatusReader supplies a group's last evaluation. The scheduler
// satisfies it. The returned status shares its slices with the evaluation that
// produced it: treat them as read-only.
type CapacityStatusReader interface {
	// CapacityStatus returns the last evaluation of the group, and false when
	// there is none: before the first sweep, or when the group is disabled.
	CapacityStatus(id string) (capacity.Status, bool)
}

// capacityGroupHandler handles all capacity-group REST endpoints.
type capacityGroupHandler struct {
	store  store.Store
	status CapacityStatusReader // nil: no status is ever reported
	logger *slog.Logger

	// natsAuth is whether the broker requires per-worker credentials, which a
	// group token needs.
	natsAuth bool
}

func newCapacityGroupHandler(st store.Store, status CapacityStatusReader, natsAuth bool, logger *slog.Logger) *capacityGroupHandler {
	return &capacityGroupHandler{store: st, status: status, natsAuth: natsAuth, logger: logger}
}

// mount registers the routes on rest behind their permission gates. Minting a
// group token is mounted only with REST auth on, for the reason POST
// /workers/join-tokens is (see router.go): it attaches compute, so an
// anonymous superuser must not reach it.
func (h *capacityGroupHandler) mount(rest chi.Router, az *authz, authEnabled bool) {
	rest.Group(func(g chi.Router) {
		g.Use(az.require(policy.InfraRead))
		g.Get("/capacity-groups", h.list)
		g.Get("/capacity-groups/{id}", h.get)
		g.Get("/capacity-groups/{id}/members", h.listMembers)
	})
	rest.Group(func(g chi.Router) {
		g.Use(az.require(policy.InfraManage))
		g.Post("/capacity-groups", h.create)
		g.Put("/capacity-groups/{id}", h.update)
		g.Delete("/capacity-groups/{id}", h.delete)
		g.Put("/capacity-groups/{id}/members/{workerID}", h.addMember)
		g.Delete("/capacity-groups/{id}/members/{workerID}", h.removeMember)
		g.Get("/capacity-groups/{id}/tokens", h.listTokens)
		g.Delete("/capacity-groups/{id}/tokens/{tokenID}", h.revokeToken)
		if authEnabled {
			g.Post("/capacity-groups/{id}/tokens", h.createToken)
		}
	})
}

// ── Wire-format types ─────────────────────────────────────────────────────────

// capacityGroupInput is the body of POST and PUT /capacity-groups.
type capacityGroupInput struct {
	FarmID                 string                  `json:"farm_id"`
	Name                   string                  `json:"name"`
	Kind                   store.CapacityGroupKind `json:"kind"`
	Provider               store.CapacityProvider  `json:"provider"`
	Enabled                *bool                   `json:"enabled"`
	QueueIDs               []string                `json:"queue_ids"`
	MinMachines            int                     `json:"min_machines"`
	MaxMachines            int                     `json:"max_machines"`
	Priority               int                     `json:"priority"`
	MaxStepUp              int                     `json:"max_step_up"`
	ScaleUpCooldownSeconds *int64                  `json:"scale_up_cooldown_seconds"`
	IdleTimeoutSeconds     int64                   `json:"idle_timeout_seconds"`
	BootTimeoutSeconds     int64                   `json:"boot_timeout_seconds"`
	RetireAction           string                  `json:"retire_action"`
	MachineProfile         store.MachineProfile    `json:"machine_profile"`
}

// capacityStatusResponse is a group's last evaluation.
type capacityStatusResponse struct {
	DemandCores      int                        `json:"demand_cores"`
	DesiredMachines  int                        `json:"desired_machines"`
	OnlineMembers    int                        `json:"online_members"`
	OfflineMembers   int                        `json:"offline_members"`
	RetireCandidates []string                   `json:"retire_candidates"`
	Mismatches       []capacityMismatchResponse `json:"mismatches"`
	EvaluatedAt      time.Time                  `json:"evaluated_at"`
}

type capacityMismatchResponse struct {
	WorkerID string `json:"worker_id"`
	Reason   string `json:"reason"`
}

// capacityGroupResponse is the JSON representation of a capacity group. Status
// is null until the scheduler has evaluated the group, and while it is
// disabled.
type capacityGroupResponse struct {
	ID                     string                  `json:"id"`
	FarmID                 string                  `json:"farm_id"`
	Name                   string                  `json:"name"`
	Kind                   store.CapacityGroupKind `json:"kind"`
	Provider               store.CapacityProvider  `json:"provider"`
	Enabled                bool                    `json:"enabled"`
	QueueIDs               []string                `json:"queue_ids"`
	MinMachines            int                     `json:"min_machines"`
	MaxMachines            int                     `json:"max_machines"`
	Priority               int                     `json:"priority"`
	MaxStepUp              int                     `json:"max_step_up"`
	ScaleUpCooldownSeconds int64                   `json:"scale_up_cooldown_seconds"`
	IdleTimeoutSeconds     int64                   `json:"idle_timeout_seconds"`
	BootTimeoutSeconds     int64                   `json:"boot_timeout_seconds"`
	RetireAction           string                  `json:"retire_action"`
	MachineProfile         store.MachineProfile    `json:"machine_profile"`
	CreatedAt              time.Time               `json:"created_at"`
	UpdatedAt              time.Time               `json:"updated_at"`
	Status                 *capacityStatusResponse `json:"status"`
}

// capacityTokenResponse is a group token as listed. It carries no secret: the
// raw token is shown once, at creation, and only its hash is stored.
type capacityTokenResponse struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
}

// capacityTokenCreatedResponse is the create-only shape carrying the raw
// token, the one place it is ever returned.
type capacityTokenCreatedResponse struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	Prefix    string    `json:"prefix"`
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

// capacityTokenRequest is the body of POST /capacity-groups/{id}/tokens; the
// body itself is optional.
type capacityTokenRequest struct {
	Name      string `json:"name"`
	ExpiresIn string `json:"expires_in"`
}

// ── Conversions ───────────────────────────────────────────────────────────────

func toCapacityStatusResponse(s capacity.Status) *capacityStatusResponse {
	r := &capacityStatusResponse{
		DemandCores:      s.DemandCores,
		DesiredMachines:  s.DesiredMachines,
		OnlineMembers:    s.OnlineMembers,
		OfflineMembers:   s.OfflineMembers,
		RetireCandidates: make([]string, len(s.RetireCandidates)),
		Mismatches:       make([]capacityMismatchResponse, len(s.Mismatches)),
		EvaluatedAt:      s.EvaluatedAt,
	}
	// Copies, not aliases: the evaluation's slices are shared with the
	// scheduler.
	copy(r.RetireCandidates, s.RetireCandidates)
	for i, m := range s.Mismatches {
		r.Mismatches[i] = capacityMismatchResponse{WorkerID: m.WorkerID, Reason: m.Reason}
	}
	return r
}

func (h *capacityGroupHandler) toResponse(g store.CapacityGroup) capacityGroupResponse {
	var status *capacityStatusResponse
	if h.status != nil {
		if s, ok := h.status.CapacityStatus(g.ID); ok {
			status = toCapacityStatusResponse(s)
		}
	}
	queues := g.QueueIDs
	if queues == nil {
		queues = []string{}
	}
	return capacityGroupResponse{
		ID: g.ID, FarmID: g.FarmID, Name: g.Name, Kind: g.Kind, Provider: g.Provider,
		Enabled: g.Enabled, QueueIDs: queues,
		MinMachines: g.MinMachines, MaxMachines: g.MaxMachines, Priority: g.Priority, MaxStepUp: g.MaxStepUp,
		ScaleUpCooldownSeconds: int64(g.ScaleUpCooldown / time.Second),
		IdleTimeoutSeconds:     int64(g.IdleTimeout / time.Second),
		BootTimeoutSeconds:     int64(g.BootTimeout / time.Second),
		RetireAction:           g.RetireAction,
		MachineProfile:         g.Profile,
		CreatedAt:              g.CreatedAt, UpdatedAt: g.UpdatedAt,
		Status: status,
	}
}

// secondsToDuration converts a *_seconds field, refusing a value no sensible
// setting needs (and one a time.Duration could not hold).
func secondsToDuration(field string, seconds int64) (d time.Duration, problem string) {
	if seconds < 0 || seconds > maxCapacityDurationSeconds {
		return 0, fmt.Sprintf("%s must be between 0 and %d", field, maxCapacityDurationSeconds)
	}
	return time.Duration(seconds) * time.Second, ""
}

// ── Building a group from a request ───────────────────────────────────────────

// decodeCapacityGroupInput reads the request body. Unknown fields are refused:
// a misspelled setting (idle_timeout for idle_timeout_seconds) would otherwise
// fall back to its default silently.
func decodeCapacityGroupInput(r *http.Request) (in capacityGroupInput, problem string) {
	dec := json.NewDecoder(io.LimitReader(r.Body, capacityBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		if strings.HasPrefix(err.Error(), "json: unknown field") {
			return in, "invalid JSON body: " + err.Error()
		}
		return in, "invalid JSON body"
	}
	in.Name = strings.TrimSpace(in.Name)
	return in, ""
}

// groupFromInput applies the defaults and the unit conversions, validates the
// result with [validateGroup] and returns the group, without its identity.
// The second result is a 400 detail, or "".
func groupFromInput(in capacityGroupInput) (g store.CapacityGroup, problem string) {
	g = store.CapacityGroup{
		FarmID: in.FarmID, Name: in.Name, Kind: in.Kind, Provider: in.Provider,
		Enabled: true, QueueIDs: in.QueueIDs,
		MinMachines: in.MinMachines, MaxMachines: in.MaxMachines, Priority: in.Priority,
		MaxStepUp: in.MaxStepUp, RetireAction: in.RetireAction, Profile: in.MachineProfile,
	}
	if in.Enabled != nil {
		g.Enabled = *in.Enabled
	}
	if g.Provider == "" {
		g.Provider = store.CapacityProviderExternal
	}
	if g.MaxStepUp == 0 {
		g.MaxStepUp = defaultCapacityMaxStepUp
	}
	if g.RetireAction == "" {
		g.RetireAction = store.CapacityRetireSleep
	}
	cooldown, idle, boot := int64(defaultCapacityCooldown), in.IdleTimeoutSeconds, in.BootTimeoutSeconds
	if in.ScaleUpCooldownSeconds != nil {
		cooldown = *in.ScaleUpCooldownSeconds
	}
	if idle == 0 {
		idle = defaultCapacityTimeout
	}
	if boot == 0 {
		boot = defaultCapacityTimeout
	}
	if g.ScaleUpCooldown, problem = secondsToDuration("scale_up_cooldown_seconds", cooldown); problem != "" {
		return g, problem
	}
	if g.IdleTimeout, problem = secondsToDuration("idle_timeout_seconds", idle); problem != "" {
		return g, problem
	}
	if g.BootTimeout, problem = secondsToDuration("boot_timeout_seconds", boot); problem != "" {
		return g, problem
	}
	return g, validateGroup(g)
}

// validateGroup checks g against the shared rules and against the providers
// the API accepts. The result is a 400 detail, or "".
func validateGroup(g store.CapacityGroup) string {
	if err := g.Validate(); err != nil {
		return err.Error()
	}
	// The record and its validation admit every provider; the API accepts
	// only the external one.
	if g.Provider != store.CapacityProviderExternal {
		return fmt.Sprintf("provider %q is not available yet", g.Provider)
	}
	return ""
}

// checkQueues verifies every queue exists and belongs to the group's farm. The
// second result is a 400 detail, or "".
func (h *capacityGroupHandler) checkQueues(r *http.Request, g store.CapacityGroup) (string, error) {
	if len(g.QueueIDs) > maxCapacityGroupQueues {
		return fmt.Sprintf("queue_ids may hold at most %d entries", maxCapacityGroupQueues), nil
	}
	for _, id := range g.QueueIDs {
		q, err := h.store.GetQueue(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Sprintf("queue %q not found", id), nil
		}
		if err != nil {
			return "", err
		}
		if q.FarmID != g.FarmID {
			return fmt.Sprintf("queue %q belongs to another farm", id), nil
		}
	}
	return "", nil
}

// ── GET /capacity-groups ──────────────────────────────────────────────────────

func (h *capacityGroupHandler) list(w http.ResponseWriter, r *http.Request) {
	groups, err := h.store.ListCapacityGroups(r.Context())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "capacity-groups: list failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to list capacity groups")
		return
	}
	resp := make([]capacityGroupResponse, len(groups))
	for i, g := range groups {
		resp[i] = h.toResponse(g)
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── GET /capacity-groups/{id} ─────────────────────────────────────────────────

func (h *capacityGroupHandler) get(w http.ResponseWriter, r *http.Request) {
	g, ok := h.loadGroup(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, h.toResponse(g))
}

// loadGroup reads the group named by {id}, answering 404 or 500 itself.
func (h *capacityGroupHandler) loadGroup(w http.ResponseWriter, r *http.Request) (store.CapacityGroup, bool) {
	id := chi.URLParam(r, "id")
	g, err := h.store.GetCapacityGroup(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "capacity group not found")
			return g, false
		}
		h.logger.ErrorContext(r.Context(), "capacity-groups: get failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve capacity group")
		return g, false
	}
	return g, true
}

// ── POST /capacity-groups ─────────────────────────────────────────────────────

func (h *capacityGroupHandler) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	in, problem := decodeCapacityGroupInput(r)
	if problem != "" {
		writeProblem(w, r, http.StatusBadRequest, problem)
		return
	}
	if in.FarmID == "" {
		writeProblem(w, r, http.StatusBadRequest, "farm_id is required")
		return
	}
	g, problem := groupFromInput(in)
	if problem != "" {
		writeProblem(w, r, http.StatusBadRequest, problem)
		return
	}
	if _, err := h.store.GetFarm(ctx, g.FarmID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusBadRequest, "farm not found")
			return
		}
		h.logger.ErrorContext(ctx, "capacity-groups: farm lookup failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to validate farm")
		return
	}
	if !h.queuesOK(w, r, g) {
		return
	}

	g.ID = uuid.NewString()
	created, err := h.store.CreateCapacityGroup(ctx, g)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict, errCapacityGroupNameTaken)
			return
		}
		h.logger.ErrorContext(ctx, "capacity-groups: create failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create capacity group")
		return
	}
	writeJSON(w, http.StatusCreated, h.toResponse(created))
}

// queuesOK checks g's queues, answering 400 or 500 itself.
func (h *capacityGroupHandler) queuesOK(w http.ResponseWriter, r *http.Request, g store.CapacityGroup) bool {
	problem, err := h.checkQueues(r, g)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "capacity-groups: queue lookup failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to validate queues")
		return false
	}
	if problem != "" {
		writeProblem(w, r, http.StatusBadRequest, problem)
		return false
	}
	return true
}

// ── PUT /capacity-groups/{id} ─────────────────────────────────────────────────

func (h *capacityGroupHandler) update(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	existing, ok := h.loadGroup(w, r)
	if !ok {
		return
	}
	in, problem := decodeCapacityGroupInput(r)
	if problem != "" {
		writeProblem(w, r, http.StatusBadRequest, problem)
		return
	}
	if in.FarmID != "" && in.FarmID != existing.FarmID {
		writeProblem(w, r, http.StatusBadRequest, "farm_id cannot be changed")
		return
	}
	in.FarmID = existing.FarmID
	g, problem := groupFromInput(in)
	if problem != "" {
		writeProblem(w, r, http.StatusBadRequest, problem)
		return
	}
	if !h.queuesOK(w, r, g) {
		return
	}
	if g.Kind != existing.Kind {
		members, err := h.store.ListCapacityGroupMembers(ctx, existing.ID)
		if err != nil {
			h.logger.ErrorContext(ctx, "capacity-groups: member lookup failed", slog.Any("error", err))
			writeProblem(w, r, http.StatusInternalServerError, "failed to update capacity group")
			return
		}
		if len(members) > 0 {
			writeProblem(w, r, http.StatusConflict, "a group's kind cannot change while it has members; remove them first")
			return
		}
	}

	g.ID = existing.ID
	updated, err := h.store.UpdateCapacityGroup(ctx, g)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeProblem(w, r, http.StatusNotFound, "capacity group not found")
		case errors.Is(err, store.ErrConflict):
			writeProblem(w, r, http.StatusConflict, errCapacityGroupNameTaken)
		default:
			h.logger.ErrorContext(ctx, "capacity-groups: update failed", slog.String("id", existing.ID), slog.Any("error", err))
			writeProblem(w, r, http.StatusInternalServerError, "failed to update capacity group")
		}
		return
	}
	writeJSON(w, http.StatusOK, h.toResponse(updated))
}

// ── DELETE /capacity-groups/{id} ──────────────────────────────────────────────

func (h *capacityGroupHandler) delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.store.DeleteCapacityGroup(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeProblem(w, r, http.StatusNotFound, "capacity group not found")
		case errors.Is(err, store.ErrConflict):
			writeProblem(w, r, http.StatusConflict, "capacity group still has members; remove them first")
		default:
			h.logger.ErrorContext(r.Context(), "capacity-groups: delete failed", slog.String("id", id), slog.Any("error", err))
			writeProblem(w, r, http.StatusInternalServerError, "failed to delete capacity group")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Members ───────────────────────────────────────────────────────────────────

// listMembers returns the group's workers in the shape GET /workers returns.
func (h *capacityGroupHandler) listMembers(w http.ResponseWriter, r *http.Request) {
	g, ok := h.loadGroup(w, r)
	if !ok {
		return
	}
	members, err := h.store.ListCapacityGroupMembers(r.Context(), g.ID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "capacity-groups: list members failed", slog.String("id", g.ID), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to list capacity group members")
		return
	}
	resp := make([]workerResponse, len(members))
	for i, m := range members {
		resp[i] = toWorkerResponse(m)
	}
	writeJSON(w, http.StatusOK, resp)
}

// addMember makes an existing worker a member of a named group. A sized
// group's members join by enrolling with its token (or by naming the group in
// their own configuration), never by being added here.
func (h *capacityGroupHandler) addMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	g, ok := h.loadGroup(w, r)
	if !ok {
		return
	}
	if g.Kind != store.CapacityGroupNamed {
		writeProblem(w, r, http.StatusConflict,
			"members of a sized group join by enrolling with a group token or by setting worker.capacity_group; only a named group takes members by name")
		return
	}
	workerID := chi.URLParam(r, "workerID")
	wk, err := h.store.GetWorker(ctx, workerID)
	if err != nil {
		h.workerLookupFailed(w, r, workerID, err)
		return
	}
	if wk.FarmID != g.FarmID {
		writeProblem(w, r, http.StatusConflict, "the worker is in a different farm than the group")
		return
	}

	updated, err := h.store.SetWorkerCapacityGroup(ctx, workerID, g.ID, false)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeProblem(w, r, http.StatusNotFound, "worker or capacity group not found")
		case errors.Is(err, store.ErrConflict):
			// The store reports two causes the same way.
			writeProblem(w, r, http.StatusConflict,
				"the worker could not be added: it is already in another group (remove it from that one first), "+
					"or the group was changed or removed concurrently")
		default:
			h.logger.ErrorContext(ctx, "capacity-groups: add member failed",
				slog.String("id", g.ID), slog.String("worker_id", workerID), slog.Any("error", err))
			writeProblem(w, r, http.StatusInternalServerError, "failed to add worker to capacity group")
		}
		return
	}
	writeJSON(w, http.StatusOK, toWorkerResponse(updated))
}

func (h *capacityGroupHandler) workerLookupFailed(w http.ResponseWriter, r *http.Request, workerID string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, r, http.StatusNotFound, "worker not found")
		return
	}
	h.logger.ErrorContext(r.Context(), "capacity-groups: worker lookup failed",
		slog.String("worker_id", workerID), slog.Any("error", err))
	writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve worker")
}

// removeMember clears a worker's membership of this group.
func (h *capacityGroupHandler) removeMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	groupID := chi.URLParam(r, "id")
	workerID := chi.URLParam(r, "workerID")
	wk, err := h.store.GetWorker(ctx, workerID)
	if err != nil {
		h.workerLookupFailed(w, r, workerID, err)
		return
	}
	if wk.CapacityGroupID != groupID {
		writeProblem(w, r, http.StatusNotFound, "the worker is not a member of this capacity group")
		return
	}
	// An ephemeral member's membership comes back from its credential or its
	// configuration at its next registration, so clearing it here would last
	// only until the worker reconnects.
	if wk.Ephemeral {
		writeProblem(w, r, http.StatusConflict, errEphemeralMemberRemoval)
		return
	}
	if _, err := h.store.SetWorkerCapacityGroup(ctx, workerID, "", false); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "worker not found")
			return
		}
		h.logger.ErrorContext(ctx, "capacity-groups: remove member failed",
			slog.String("id", groupID), slog.String("worker_id", workerID), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to remove worker from capacity group")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Tokens ────────────────────────────────────────────────────────────────────

// listTokens returns the group's join tokens, revoked and expired included.
func (h *capacityGroupHandler) listTokens(w http.ResponseWriter, r *http.Request) {
	g, ok := h.loadGroup(w, r)
	if !ok {
		return
	}
	tokens, err := h.store.ListCapacityGroupTokens(r.Context(), g.ID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "capacity-groups: list tokens failed", slog.String("id", g.ID), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to list group tokens")
		return
	}
	resp := make([]capacityTokenResponse, len(tokens))
	for i, t := range tokens {
		resp[i] = capacityTokenResponse{
			ID: t.ID, Name: t.Name, Prefix: t.Prefix, ExpiresAt: t.ExpiresAt,
			RevokedAt: t.RevokedAt, CreatedBy: t.CreatedBy, CreatedAt: t.CreatedAt,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseGroupTokenTTL turns expires_in into a lifetime. The second result is a
// 400 detail, or "".
func parseGroupTokenTTL(expiresIn string) (ttl time.Duration, problem string) {
	if expiresIn == "" {
		return defaultGroupTokenTTL, ""
	}
	d, err := time.ParseDuration(expiresIn)
	switch {
	case err != nil:
		return 0, fmt.Sprintf("expires_in %q is not a duration such as \"720h\"", expiresIn)
	case d <= 0:
		return 0, "expires_in must be positive"
	case d > maxGroupTokenTTL:
		return 0, fmt.Sprintf("expires_in must be at most %s", maxGroupTokenTTL)
	}
	return d, ""
}

// createToken mints a reusable join token bound to a sized group. The raw token
// is returned exactly once.
func (h *capacityGroupHandler) createToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	g, ok := h.loadGroup(w, r)
	if !ok {
		return
	}
	var req capacityTokenRequest
	if !readOptionalJSON(w, r, capacityBodyLimit, &req) {
		return
	}
	ttl, problem := parseGroupTokenTTL(strings.TrimSpace(req.ExpiresIn))
	if problem != "" {
		writeProblem(w, r, http.StatusBadRequest, problem)
		return
	}
	if g.Kind != store.CapacityGroupSized {
		writeProblem(w, r, http.StatusConflict,
			"a named group takes its members by name; group tokens are for sized groups")
		return
	}
	if !h.natsAuth {
		writeProblem(w, r, http.StatusConflict, errGroupTokensNeedBrokerAuth)
		return
	}

	raw, created, err := mintJoinToken(ctx, h.store,
		store.WorkerJoinToken{Name: strings.TrimSpace(req.Name), CapacityGroupID: g.ID}, ttl)
	if err != nil {
		h.logger.ErrorContext(ctx, "capacity-groups: create token failed", slog.String("id", g.ID), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create group token")
		return
	}
	writeJSON(w, http.StatusCreated, capacityTokenCreatedResponse{
		ID: created.ID, Token: raw, Prefix: created.Prefix, Name: created.Name,
		ExpiresAt: created.ExpiresAt, CreatedAt: created.CreatedAt,
	})
}

// revokeToken revokes one of the group's tokens. The store refuses a token
// that is not this group's, so the route cannot revoke an ordinary join token
// or another group's.
func (h *capacityGroupHandler) revokeToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	groupID, tokenID := chi.URLParam(r, "id"), chi.URLParam(r, "tokenID")
	if err := h.store.RevokeCapacityGroupToken(ctx, groupID, tokenID, time.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "group token not found")
			return
		}
		h.logger.ErrorContext(ctx, "capacity-groups: revoke token failed", slog.String("token_id", tokenID), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to revoke group token")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
