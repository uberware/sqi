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
// writing the store directly, and enroll delegates to an injected
// [BrokerCredentialReloader] after it writes the credential. internal/api
// never holds a live broker handle — the process that does (internal/server)
// supplies implementations that write the store and then reload the
// broker's authorized-key set, so a worker that loses (or gains) a
// credential is disconnected (or made connectable) synchronously, inside
// the same request. This package depends only on the two narrow interfaces,
// never on internal/bus or internal/server, so it cannot import either and
// stays testable without a live broker.

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
	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/store"
)

// WorkerRevoker revokes a worker's broker credential and, when the
// implementation holds a live broker handle, disconnects it and lets the
// existing heartbeat-sweep/reclaim path return its in-flight work to ready.
// internal/api depends only on this interface so that DELETE
// /api/v1/workers/{id}/credential can be synchronous where a broker handle
// is available (internal/server, which runs in the same process) without
// this package importing internal/bus or internal/server itself.
type WorkerRevoker interface {
	// RevokeWorker revokes workerID's active credential. It returns
	// [store.ErrNotFound] if the worker has no active credential — never
	// enrolled, or already revoked; those two cases are indistinguishable
	// here for the same reason store.RevokeWorkerCredential collapses them.
	RevokeWorker(ctx context.Context, workerID string) error
}

// BrokerCredentialReloader re-syncs a running broker's authorized-key set
// with the store's active worker_credentials rows. Separate from
// [WorkerRevoker] on purpose: it is the enrollment side of the same
// underlying operation, triggered by a different event (a credential
// created, not revoked) and with a different failure posture — see enroll's
// use of it below. internal/api depends only on this interface, never on
// internal/bus or internal/server, for the same reason WorkerRevoker does.
type BrokerCredentialReloader interface {
	// ReloadBrokerCredentials re-reads the active credential set from the
	// store and reloads it into the broker's authorized-key set, so a
	// worker just enrolled can connect to a RUNNING broker without an
	// operator restarting it.
	ReloadBrokerCredentials(ctx context.Context) error
}

// errInvalidJoinToken is returned for every way a join token can fail to
// authorize an enrollment — unknown, expired, or already used when single-use
// is on. The endpoint is unauthenticated and may be internet-reachable, so
// distinguishing those cases in the response would let a caller enumerate
// which join tokens exist, or whether one has already been claimed.
const errInvalidJoinToken = "invalid or expired join token" //nolint:gosec // G101: a static response string, not a credential

// workerEnrollHandler implements the worker broker-credential REST surface.
type workerEnrollHandler struct {
	store    store.Store
	revoker  WorkerRevoker
	reloader BrokerCredentialReloader
	logger   *slog.Logger

	// singleUse mirrors config.NATSAuthConfig.JoinTokenSingleUse: whether an
	// already-used join token is rejected on a second enrollment attempt.
	singleUse bool

	// joinTokenTTL mirrors config.NATSAuthConfig.JoinTokenTTL: how long a
	// token minted by createJoinToken remains valid. Already bounds-checked
	// at config load, so it is not re-validated here.
	joinTokenTTL time.Duration
}

// newWorkerEnrollHandler returns a workerEnrollHandler wired to the given
// store, revoker, and reloader.
func newWorkerEnrollHandler(st store.Store, revoker WorkerRevoker, reloader BrokerCredentialReloader, logger *slog.Logger, singleUse bool, joinTokenTTL time.Duration) *workerEnrollHandler {
	return &workerEnrollHandler{
		store:        st,
		revoker:      revoker,
		reloader:     reloader,
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
	// enroll is the only route in this API reachable by an unauthenticated,
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

	// Validate the key and the worker ID BEFORE the token is claimed, so a
	// malformed request costs a 400 and not the operator's single-use token.
	if err := brokerauth.ValidatePublicKey(req.PublicKey); err != nil {
		writeProblem(w, r, http.StatusBadRequest, err.Error())
		return
	}
	// The recorded worker ID is what this credential's broker grants are
	// built from (brokerauth.WorkerPermissions), and those grants are NATS
	// subject PATTERNS. A worker_id of "*" would mint a credential allowed
	// to publish "task.status.*.*", "worker.deregister.*", "work.lease.*.*"
	// and the rest — concrete subjects belonging to ANY worker, so it could
	// forge status and logs, deregister the farm, and lease work as another
	// worker and receive that worker's assignment batch. The scheduler's
	// provenance checks cannot catch that: the subject NATS vouches for
	// genuinely names the victim. A worker_id of ">" is worse-shaped still,
	// putting the malformed "task.status.>.*" into the broker's key set,
	// which nats-server rejects outright.
	if !brokerauth.ValidWorkerIDToken(req.WorkerID) {
		writeProblem(w, r, http.StatusBadRequest,
			"worker_id must be a single NATS subject token: non-empty, and containing no '.', whitespace, '*' or '>'")
		return
	}

	now := time.Now().UTC()
	if err := h.claimJoinToken(ctx, brokerauth.HashJoinToken(req.JoinToken), now); err != nil {
		// Unknown, expired, already claimed, and a store failure all deny
		// identically — see errInvalidJoinToken.
		writeProblem(w, r, http.StatusUnauthorized, errInvalidJoinToken)
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

	h.finishEnrollment(ctx, req.WorkerID)

	writeJSON(w, http.StatusCreated, toWorkerCredentialResponse(created))
}

// claimJoinToken authorizes this enrollment against the join token hashed to
// hash and, when tokens are single-use, redeems it in the same operation.
//
// The single-use path is deliberately ONE store statement
// ([store.ConsumeWorkerJoinToken]): reading the token, inspecting UsedAt and
// marking it used separately is a check-then-act race in which two
// simultaneous enrollments with one token both succeed.
//
// The price is that the token is spent BEFORE the credential row is created,
// so an enrollment that then fails on a conflicting worker ID or public key
// burns it and the operator issues a new one. That is the unavoidable side
// of an atomic claim, and it is the safe side: the alternative spends the
// token only on success and therefore cannot be atomic at all. The cases
// that cost nothing — a malformed body, a missing field, a malformed public
// key — are all rejected before this is called.
//
// With single-use disabled the token stays redeemable by design, so there is
// nothing to claim atomically: UsedAt becomes a "last redeemed" marker whose
// write failure is logged and ignored rather than denying an enrollment the
// token does authorize.
func (h *workerEnrollHandler) claimJoinToken(ctx context.Context, hash string, now time.Time) error {
	if h.singleUse {
		_, err := h.store.ConsumeWorkerJoinToken(ctx, hash, now)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			h.logger.ErrorContext(ctx, "workerenroll: join token claim failed", slog.Any("error", err))
		}
		return err
	}

	token, err := h.store.GetWorkerJoinTokenByHash(ctx, hash)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			h.logger.ErrorContext(ctx, "workerenroll: join token lookup failed", slog.Any("error", err))
		}
		return err
	}
	// Strictly after, matching ConsumeWorkerJoinToken's "expires_at > ?".
	if !token.ExpiresAt.After(now) {
		return store.ErrNotFound
	}
	if err := h.store.MarkWorkerJoinTokenUsed(ctx, token.ID, now); err != nil {
		h.logger.ErrorContext(ctx, "workerenroll: mark join token used failed",
			slog.String("token_id", token.ID), slog.Any("error", err))
	}
	return nil
}

// finishEnrollment performs the side effect that follows a successful
// credential creation: reloading the broker's authorized-key set. A failure
// is logged and swallowed rather than turned into an error response — the
// credential itself is already created and durable by the time this runs,
// so telling the caller enrollment failed would be false. Split out of
// enroll to keep that handler's own branching within this repo's complexity
// budget.
//
// Redeeming the join token is NOT done here: it has to happen before the
// credential is created, as one atomic claim — see [claimJoinToken].
//
// The reload failure here is the opposite direction from a revoke's reload
// failure (which IS surfaced to ITS caller): a revoke failing to reload
// leaves the broker too PERMISSIVE (still trusting a credential the store
// says is gone), which its caller needs to know about; an enroll failing to
// reload leaves the broker too STRICT (a valid worker just can't connect
// yet), which is safe but not self-correcting — there is no background
// reconciliation, so recovery depends on some later enroll or revoke
// triggering another reload, and on an idle farm the new worker stays
// unable to connect until the server restarts. It is not recoverable by the
// worker simply retrying, either: a credential the broker rejects is fatal
// in the worker (it exits rather than looping on reconnect), so getting it
// connected after this failure relies on an external process supervisor
// restarting it, or an operator restarting sqi-server — see
// [BrokerCredentialReloader].
func (h *workerEnrollHandler) finishEnrollment(ctx context.Context, workerID string) {
	if err := h.reloader.ReloadBrokerCredentials(ctx); err != nil {
		h.logger.ErrorContext(ctx, "workerenroll: reload broker credentials failed after enrollment",
			slog.String("worker_id", workerID), slog.Any("error", err))
	}
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
// {id} and, via the injected [WorkerRevoker], disconnects it from the broker
// where a live handle is available. The underlying store write only matches
// a row with a nil RevokedAt, so a worker that was never enrolled and a
// worker whose credential is already revoked are indistinguishable here —
// the response says so rather than claiming the worker does not exist.
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
