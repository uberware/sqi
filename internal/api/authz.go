// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Route authorization for the REST API. requirePermission gates a route group
// on a single policy.Permission, reading the auth.Principal that middleware.Auth
// placed in the request context. Denials are audited (append-only) and logged.

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/policy"
	"github.com/uberware/sqi/internal/store"
)

type authz struct {
	store  store.Store
	logger *slog.Logger
}

func newAuthz(st store.Store, logger *slog.Logger) *authz {
	return &authz{store: st, logger: logger}
}

// require returns middleware that allows the request only if the context
// principal holds perm. It must run after middleware.Auth, which guarantees a
// principal is present. On denial it writes a 403 problem-details response,
// records an audit entry, and does not call the wrapped handler.
func (a *authz) require(perm policy.Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := auth.FromContext(r.Context())
			if policy.Can(p, perm) {
				next.ServeHTTP(w, r)
				return
			}
			a.logger.WarnContext(
				r.Context(), "authz: permission denied",
				slog.String("subject", p.Subject),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("permission", string(perm)),
			)
			// Key the audit row on the route PATTERN, not the raw request
			// path: the path is attacker-controlled (any authenticated
			// caller can vary it freely, e.g. /jobs/<random-id>) and would
			// give the audit table unbounded cardinality. The pattern
			// collapses all such requests to one entry per gated route. The
			// raw path is still recorded, just inside Details rather than as
			// the indexed EntityID.
			routePattern := chi.RouteContext(r.Context()).RoutePattern()
			if routePattern == "" {
				routePattern = r.URL.Path
			}
			entry := store.AuditEntry{
				ID:         uuid.NewString(),
				EntityType: "authz",
				EntityID:   routePattern,
				Action:     "denied",
				Actor:      p.Subject,
				Details:    map[string]any{"method": r.Method, "permission": string(perm), "path": r.URL.Path},
				CreatedAt:  time.Now().UTC(),
			}
			if err := a.store.AppendAuditEntry(r.Context(), entry); err != nil {
				a.logger.ErrorContext(r.Context(), "authz: audit append failed", slog.Any("error", err))
			}
			writeProblem(w, r, http.StatusForbidden, "forbidden")
		})
	}
}
