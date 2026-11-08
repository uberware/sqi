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
// The revoke handler delegates to an injected [WorkerRevoker] rather than
// writing the store directly. internal/api holds no broker handle: the
// process that does (internal/server) supplies an implementation that writes
// the store and then disconnects the worker from the broker, inside the same
// request. Enrollment needs no broker call: the broker reads the credential
// from the store when the worker connects. This package depends only on that
// interface, never on internal/bus or internal/server, so it is testable
// without a live broker.

import (
	"context"
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
	"github.com/uberware/sqi/internal/auth/jointoken"
	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/store"
)

// WorkerRevoker revokes a worker's broker credential and, when the
// implementation holds a live broker handle, disconnects it and lets the
// heartbeat-sweep/reclaim path return its in-flight work to ready.
// internal/api depends only on this interface so that DELETE
// /api/v1/workers/{id}/credential can be synchronous where a broker handle
// is available (internal/server, which runs in the same process) without
// this package importing internal/bus or internal/server.
type WorkerRevoker interface {
	// RevokeWorker revokes workerID's active credential. It returns
	// [store.ErrNotFound] if the worker has no active credential — never
	// enrolled, or already revoked; those two cases are indistinguishable
	// here for the same reason store.RevokeWorkerCredential collapses them.
	RevokeWorker(ctx context.Context, workerID string) error
}

// errInvalidJoinToken is returned for every way a join token can fail to
// authorize an enrollment: unknown, expired, or already used when single-use
// is on. The endpoint is unauthenticated and may be internet-reachable, so
// distinguishing those cases in the response would let a caller enumerate
// which join tokens exist, or whether one has been claimed.
const errInvalidJoinToken = "invalid or expired join token" //nolint:gosec // G101: a static response string, not a credential

// workerEnrollHandler implements the worker broker-credential REST surface.
type workerEnrollHandler struct {
	store   store.Store
	revoker WorkerRevoker
	logger  *slog.Logger

	// singleUse mirrors config.NATSAuthConfig.JoinTokenSingleUse: whether an
	// already-used join token is rejected on a second enrollment attempt.
	singleUse bool

	// joinTokenTTL mirrors config.NATSAuthConfig.JoinTokenTTL: how long a
	// token minted by createJoinToken remains valid. Config load bounds-checks
	// it, so it is not re-validated here.
	joinTokenTTL time.Duration
}

// newWorkerEnrollHandler returns a workerEnrollHandler wired to the given
// store and revoker.
func newWorkerEnrollHandler(st store.Store, revoker WorkerRevoker, logger *slog.Logger, singleUse bool, joinTokenTTL time.Duration) *workerEnrollHandler {
	return &workerEnrollHandler{
		store:        st,
		revoker:      revoker,
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
// [store.WorkerCredential]. It carries no seed or other secret, only the
// public key the credential was enrolled with.
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
// The body is optional: an empty POST mints an unnamed token with the
// configured default TTL.
type workerJoinTokenCreateRequest struct {
	Name string `json:"name,omitempty"`
}

// workerJoinTokenCreatedResponse is the create-only shape carrying the raw
// token. It is the only place the raw value is returned; only its hash is
// stored, so the value cannot be recovered.
type workerJoinTokenCreatedResponse struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	Prefix    string    `json:"prefix"`
	Name      string    `json:"name,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

// ── POST /api/v1/workers/enroll ─────────────────────────────────────────────

// enroll exchanges a valid join token for a broker credential. The call is
// unauthenticated: the join token in the body is the credential that
// authorizes it.
func (h *workerEnrollHandler) enroll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// enroll is reachable by an unauthenticated, potentially internet-facing
	// caller. The per-IP rate limiter bounds the request rate, not the body
	// size, so the body is capped to keep an oversized join_token or public_key
	// from being read fully into memory before validation.
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

	// Validate the key and the worker ID before the token is claimed, so a
	// malformed request costs a 400 and not the operator's single-use token.
	if err := brokerauth.ValidatePublicKey(req.PublicKey); err != nil {
		writeProblem(w, r, http.StatusBadRequest, err.Error())
		return
	}
	// The recorded worker ID is what this credential's broker grants are
	// built from (brokerauth.WorkerPermissions), and those grants are NATS
	// subject patterns. A worker_id of "*" would give the credential
	// "task.status.*.*", "worker.deregister.*", "work.lease.*.*" and the rest,
	// which match the subjects of any worker: it could forge status and logs,
	// deregister other workers, and lease work as another worker and receive
	// that worker's assignment batch. The scheduler's provenance checks cannot
	// catch that, because the subject the broker vouches for names the victim.
	// A worker_id of ">" would put the malformed "task.status.>.*" into the
	// broker's key set, which nats-server rejects.
	if !brokerauth.ValidWorkerIDToken(req.WorkerID) {
		writeProblem(w, r, http.StatusBadRequest,
			"worker_id must be a single NATS subject token: non-empty, and containing no '.', whitespace, '*' or '>'")
		return
	}

	now := time.Now().UTC()
	cred := store.WorkerCredential{
		ID:         uuid.NewString(),
		WorkerID:   req.WorkerID,
		PublicKey:  req.PublicKey,
		Name:       req.Name,
		EnrolledAt: now,
	}

	created, err := h.redeem(ctx, jointoken.Hash(req.JoinToken), now, cred)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict,
				"worker already has an active credential, or this public key is already enrolled to another worker")
			return
		}
		// Unknown, expired, already claimed, and a store failure all deny
		// identically; see errInvalidJoinToken.
		writeProblem(w, r, http.StatusUnauthorized, errInvalidJoinToken)
		return
	}

	writeJSON(w, http.StatusCreated, toWorkerCredentialResponse(created))
}

// redeem authorizes the enrollment against the join token hashed to hash and
// creates cred. A token bound to a capacity group is always reusable, whatever
// the single-use setting says: it is how a pool of machines joins one group
// with one token. Every other token follows the setting.
//
// The token is looked up first only to learn which kind it is; the single-use
// path claims it atomically in the store, so two requests presenting one
// single-use token cannot both succeed.
func (h *workerEnrollHandler) redeem(ctx context.Context, hash string, now time.Time, cred store.WorkerCredential) (store.WorkerCredential, error) {
	token, err := h.store.GetWorkerJoinTokenByHash(ctx, hash)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			h.logger.ErrorContext(ctx, "workerenroll: join token lookup failed", slog.Any("error", err))
		}
		return store.WorkerCredential{}, err
	}
	if token.CapacityGroupID == "" && h.singleUse {
		return h.redeemSingleUse(ctx, hash, now, cred)
	}
	cred.CapacityGroupID = token.CapacityGroupID
	return h.redeemReusable(ctx, token, now, cred)
}

// redeemSingleUse claims the join token hashed to hash and creates cred in one
// transaction ([store.WorkerCredentialStore.RedeemWorkerJoinToken]).
//
// The claim and the credential creation must commit together: as two separate
// store calls, a request failing on a conflicting worker ID or public key
// would already have used up the single-use token. In one transaction that
// failure rolls back the claim, so the token stays redeemable by a later,
// non-conflicting enrollment.
//
// [store.ErrConflict] is returned as-is, distinct from every other failure, so
// enroll answers 409 rather than errInvalidJoinToken's 401, as it does for
// CreateWorkerCredential's conflict on the reusable-token path.
func (h *workerEnrollHandler) redeemSingleUse(ctx context.Context, hash string, now time.Time, cred store.WorkerCredential) (store.WorkerCredential, error) {
	created, err := h.store.RedeemWorkerJoinToken(ctx, hash, now, cred)
	if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrConflict) {
		h.logger.ErrorContext(ctx, "workerenroll: redeem join token failed", slog.Any("error", err))
	}
	return created, err
}

// redeemReusable authorizes this enrollment against a reusable join token
// (single-use disabled, or a capacity-group token) and then creates cred as a
// separate call. UsedAt is a "last redeemed" marker, not a claim, so the two
// writes need not share a transaction. A failure creating the credential
// leaves the token as redeemable as it was.
//
// An expired or revoked token is [store.ErrNotFound], the same as an unknown
// one. A revoke that lands between the caller's lookup and the credential
// write does not stop that one enrollment.
func (h *workerEnrollHandler) redeemReusable(ctx context.Context, token store.WorkerJoinToken, now time.Time, cred store.WorkerCredential) (store.WorkerCredential, error) {
	// Strictly after, matching RedeemWorkerJoinToken's "expires_at > ?".
	if !token.ExpiresAt.After(now) || token.RevokedAt != nil {
		return store.WorkerCredential{}, store.ErrNotFound
	}
	if err := h.store.MarkWorkerJoinTokenUsed(ctx, token.ID, now); err != nil {
		h.logger.ErrorContext(ctx, "workerenroll: mark join token used failed",
			slog.String("token_id", token.ID), slog.Any("error", err))
	}

	created, err := h.store.CreateWorkerCredential(ctx, cred)
	if err != nil {
		if !errors.Is(err, store.ErrConflict) {
			h.logger.ErrorContext(ctx, "workerenroll: create credential failed", slog.Any("error", err))
		}
		return store.WorkerCredential{}, err
	}
	return created, nil
}

// ── POST /api/v1/workers/join-tokens ────────────────────────────────────────

// createJoinToken mints a join token an operator hands to a worker for
// self-service enrollment over POST /api/v1/workers/enroll. The raw token is
// returned once, in this response; only its hash is stored.
func (h *workerEnrollHandler) createJoinToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req workerJoinTokenCreateRequest
	// Same body-size cap as enroll.
	if !readOptionalJSON(w, r, 4<<20, &req) {
		return
	}
	raw, created, err := mintJoinToken(ctx, h.store, store.WorkerJoinToken{Name: strings.TrimSpace(req.Name)}, h.joinTokenTTL)
	if err != nil {
		h.logger.ErrorContext(ctx, "workerenroll: create join token failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create join token")
		return
	}

	writeJSON(w, http.StatusCreated, workerJoinTokenCreatedResponse{
		ID:        created.ID,
		Token:     raw,
		Prefix:    created.Prefix,
		Name:      created.Name,
		ExpiresAt: created.ExpiresAt,
		CreatedAt: created.CreatedAt,
	})
}

// readOptionalJSON decodes an optional JSON body of at most limit bytes into
// dst. An empty body is not an error; malformed JSON is. On failure it writes
// the 400 and returns false.
func readOptionalJSON(w http.ResponseWriter, r *http.Request, limit int64, dst any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "failed to read request body")
		return false
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, dst); err != nil {
			writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
			return false
		}
	}
	return true
}

// mintJoinToken generates a join token, stores its hash and returns the raw
// token with the stored row. The raw value is not stored, so the caller's
// response is the only place it appears. tok supplies Name and
// CapacityGroupID; the identity, hash, timestamps and creator are filled here.
func mintJoinToken(ctx context.Context, st store.Store, tok store.WorkerJoinToken, ttl time.Duration) (string, store.WorkerJoinToken, error) {
	raw, hash, prefix, err := jointoken.Generate()
	if err != nil {
		return "", store.WorkerJoinToken{}, fmt.Errorf("generate join token: %w", err)
	}
	now := time.Now().UTC()
	tok.ID, tok.TokenHash, tok.Prefix = uuid.NewString(), hash, prefix
	tok.CreatedAt, tok.ExpiresAt = now, now.Add(ttl)
	if p, ok := auth.FromContext(ctx); ok {
		tok.CreatedBy = p.Subject
	}
	created, err := st.CreateWorkerJoinToken(ctx, tok)
	if err != nil {
		return "", store.WorkerJoinToken{}, err
	}
	return raw, created, nil
}

// ── DELETE /api/v1/workers/{id}/credential ──────────────────────────────────

// revokeCredential soft-revokes the active credential for the worker named by
// {id} and, via the injected [WorkerRevoker], disconnects it from the broker
// where a live handle is available. The store write matches only a row with a
// nil RevokedAt, so a worker that was never enrolled and one whose credential
// is already revoked are indistinguishable; the response names both
// possibilities.
func (h *workerEnrollHandler) revokeCredential(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workerID := chi.URLParam(r, "id")
	if err := h.revoker.RevokeWorker(ctx, workerID); err != nil {
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
