// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Worker broker-credential REST handlers: self-service enrollment, join-token
// minting, and credential revocation.
//
//	POST   /api/v1/workers/enroll             — unauthenticated; the join
//	                                             token itself is the credential
//	POST   /api/v1/workers/join-tokens         — workers.enroll
//	DELETE /api/v1/workers/{id}/credential     — workers.enroll
//
// The revoke handler here performs the store write only. It does not reach
// the broker to disconnect a live worker — that requires an in-process handle
// this package does not have — so revocation here takes effect at the
// broker's next credential reload rather than synchronously. A synchronous,
// in-process revoke path is exposed elsewhere for the case where one is
// available.

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

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/store"
)

// errInvalidJoinToken is returned for every way a join token can fail to
// authorize an enrollment — unknown, expired, or already used when single-use
// is on. The endpoint is unauthenticated and may be internet-reachable, so
// distinguishing those cases in the response would let a caller enumerate
// which join tokens exist, or whether one has already been claimed.
const errInvalidJoinToken = "invalid or expired join token" //nolint:gosec // G101: a static response string, not a credential

// workerEnrollHandler implements the worker broker-credential REST surface.
type workerEnrollHandler struct {
	store  store.Store
	logger *slog.Logger

	// singleUse mirrors config.NATSAuthConfig.JoinTokenSingleUse: whether an
	// already-used join token is rejected on a second enrollment attempt.
	singleUse bool

	// joinTokenTTL mirrors config.NATSAuthConfig.JoinTokenTTL: how long a
	// token minted by createJoinToken remains valid. Already bounds-checked
	// at config load, so it is not re-validated here.
	joinTokenTTL time.Duration
}

// newWorkerEnrollHandler returns a workerEnrollHandler wired to the given
// store.
func newWorkerEnrollHandler(st store.Store, logger *slog.Logger, singleUse bool, joinTokenTTL time.Duration) *workerEnrollHandler {
	return &workerEnrollHandler{
		store:        st,
		logger:       logger,
		singleUse:    singleUse,
		joinTokenTTL: joinTokenTTL,
	}
}

// ── Wire-format types ───────────────────────────────────────────────────────

// workerEnrollRequest is the body of POST /api/v1/workers/enroll.
type workerEnrollRequest struct {
	JoinToken string `json:"join_token"`
	WorkerID  string `json:"worker_id"`
	PublicKey string `json:"public_key"`
	Name      string `json:"name,omitempty"`
}

// workerCredentialResponse is the JSON representation of a
// [store.WorkerCredential]. It never carries the seed or any other secret —
// only the public key the credential was enrolled with.
type workerCredentialResponse struct {
	ID         string     `json:"id"`
	WorkerID   string     `json:"worker_id"`
	PublicKey  string     `json:"public_key"`
	Name       string     `json:"name,omitempty"`
	EnrolledAt time.Time  `json:"enrolled_at"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

func toWorkerCredentialResponse(c store.WorkerCredential) workerCredentialResponse {
	return workerCredentialResponse{
		ID: c.ID, WorkerID: c.WorkerID, PublicKey: c.PublicKey, Name: c.Name,
		EnrolledAt: c.EnrolledAt, LastSeenAt: c.LastSeenAt, RevokedAt: c.RevokedAt,
	}
}

// workerJoinTokenCreateRequest is the body of POST /api/v1/workers/join-tokens.
// The body itself is optional — an empty POST mints an unnamed token with the
// operator-configured default TTL.
type workerJoinTokenCreateRequest struct {
	Name string `json:"name,omitempty"`
}

// workerJoinTokenCreatedResponse is the create-only shape carrying the raw
// token. This is the only place the raw value is ever returned; only its hash
// is stored, so it cannot be recovered or displayed again.
type workerJoinTokenCreatedResponse struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	Prefix    string    `json:"prefix"`
	Name      string    `json:"name,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

// ── POST /api/v1/workers/enroll ─────────────────────────────────────────────

// enroll exchanges a valid join token for a broker credential. Unauthenticated
// by design: the join token supplied in the body is itself the credential
// that authorizes this call.
func (h *workerEnrollHandler) enroll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// enroll is the only route on this branch reachable by an unauthenticated,
	// potentially internet-facing caller: the per-IP rate limiter bounds
	// request RATE, not per-request body SIZE, so an oversized join_token or
	// public_key would otherwise be read fully into memory before any
	// validation runs. Matches the jobs.go/queues.go precedent.
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4 MiB cap
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req workerEnrollRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.JoinToken = strings.TrimSpace(req.JoinToken)
	req.WorkerID = strings.TrimSpace(req.WorkerID)
	req.PublicKey = strings.TrimSpace(req.PublicKey)
	req.Name = strings.TrimSpace(req.Name)
	if req.JoinToken == "" || req.WorkerID == "" || req.PublicKey == "" {
		writeProblem(w, r, http.StatusBadRequest, "join_token, worker_id, and public_key are required")
		return
	}

	token, err := h.store.GetWorkerJoinTokenByHash(ctx, brokerauth.HashJoinToken(req.JoinToken))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			h.logger.ErrorContext(ctx, "workerenroll: join token lookup failed", slog.Any("error", err))
		}
		// An unknown token and a lookup failure both deny identically — see
		// errInvalidJoinToken.
		writeProblem(w, r, http.StatusUnauthorized, errInvalidJoinToken)
		return
	}
	now := time.Now().UTC()
	if now.After(token.ExpiresAt) {
		writeProblem(w, r, http.StatusUnauthorized, errInvalidJoinToken)
		return
	}
	if h.singleUse && token.UsedAt != nil {
		writeProblem(w, r, http.StatusUnauthorized, errInvalidJoinToken)
		return
	}

	if err := brokerauth.ValidatePublicKey(req.PublicKey); err != nil {
		writeProblem(w, r, http.StatusBadRequest, err.Error())
		return
	}

	created, err := h.store.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID:         uuid.NewString(),
		WorkerID:   req.WorkerID,
		PublicKey:  req.PublicKey,
		Name:       req.Name,
		EnrolledAt: now,
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict,
				"worker already has an active credential, or this public key is already enrolled to another worker")
			return
		}
		h.logger.ErrorContext(ctx, "workerenroll: create credential failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to enroll worker")
		return
	}

	// Mark the token used only after the credential exists: a conflict above
	// means nothing was enrolled, so a single-use token that failed to enroll
	// is not spent. A failure here is logged rather than turned into an error
	// response — the worker already holds a real credential at this point,
	// and telling it enrollment failed would be false.
	if err := h.store.MarkWorkerJoinTokenUsed(ctx, token.ID, now); err != nil {
		h.logger.ErrorContext(ctx, "workerenroll: mark join token used failed",
			slog.String("token_id", token.ID), slog.Any("error", err))
	}

	writeJSON(w, http.StatusCreated, toWorkerCredentialResponse(created))
}

// ── POST /api/v1/workers/join-tokens ────────────────────────────────────────

// createJoinToken mints a new join token an operator hands to a worker for
// self-service enrollment over POST /api/v1/workers/enroll. The raw token is
// returned exactly once, in this response; only its hash is ever stored.
func (h *workerEnrollHandler) createJoinToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Same body-size cap as enroll, for consistency across the two handlers
	// on this route group — see enroll's comment for why it matters there.
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4 MiB cap
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req workerJoinTokenCreateRequest
	// The body is optional (an empty POST mints an unnamed token), so an
	// empty body is not itself an error — only malformed JSON is.
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	req.Name = strings.TrimSpace(req.Name)

	rawToken, hash, prefix, err := brokerauth.GenerateJoinToken()
	if err != nil {
		h.logger.ErrorContext(ctx, "workerenroll: generate join token failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create join token")
		return
	}
	now := time.Now().UTC()
	createdBy := ""
	if p, ok := auth.FromContext(ctx); ok {
		createdBy = p.Subject
	}
	created, err := h.store.CreateWorkerJoinToken(ctx, store.WorkerJoinToken{
		ID:        uuid.NewString(),
		TokenHash: hash,
		Prefix:    prefix,
		Name:      req.Name,
		ExpiresAt: now.Add(h.joinTokenTTL),
		CreatedBy: createdBy,
		CreatedAt: now,
	})
	if err != nil {
		h.logger.ErrorContext(ctx, "workerenroll: create join token failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create join token")
		return
	}

	writeJSON(w, http.StatusCreated, workerJoinTokenCreatedResponse{
		ID:        created.ID,
		Token:     rawToken,
		Prefix:    created.Prefix,
		Name:      created.Name,
		ExpiresAt: created.ExpiresAt,
		CreatedAt: created.CreatedAt,
	})
}

// ── DELETE /api/v1/workers/{id}/credential ──────────────────────────────────

// revokeCredential soft-revokes the active credential for the worker named by
// {id}. RevokeWorkerCredential's underlying write only matches a row with a
// nil RevokedAt, so a worker that was never enrolled and a worker whose
// credential is already revoked are indistinguishable here — the response
// says so rather than claiming the worker does not exist.
func (h *workerEnrollHandler) revokeCredential(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workerID := chi.URLParam(r, "id")
	if err := h.store.RevokeWorkerCredential(ctx, workerID, time.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, fmt.Sprintf(
				"no active credential for worker %q — it may never have been enrolled, or its credential may already be revoked",
				workerID,
			))
			return
		}
		h.logger.ErrorContext(ctx, "workerenroll: revoke credential failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to revoke worker credential")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
