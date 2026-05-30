// SPDX-License-Identifier: AGPL-3.0-only

// Package health implements liveness and readiness checks for sqi-server.
//
// # Concepts
//
// A [Checker] represents one dependency that must be reachable for the server
// to be considered ready. Components register themselves via [Registry.Register]
// during startup; later tasks wire in real implementations:
//
//   - SQLite store checker — task 29
//   - NATS JetStream checker — task 36
//
// # Endpoints
//
// [Registry.LivenessHandler] → GET /healthz
//
// Always returns HTTP 200 with {"status":"ok"}. It performs no dependency
// checks; its only purpose is to prove the process is alive and the HTTP
// event loop is responding. An orchestrator that receives a non-200 here
// (or a timeout) will restart the process.
//
// [Registry.ReadinessHandler] → GET /readyz
//
// Runs all registered checkers concurrently under a five-second deadline.
// Returns HTTP 200 with {"status":"ok","checks":{…}} when every checker
// passes, or HTTP 503 with {"status":"degraded","checks":{…}} when any
// checker fails. If no checkers are registered the server is considered ready
// immediately — components opt in to readiness gating by calling Register.
package health

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"sync"
	"time"
)

// checkTimeout is the maximum duration granted to all checkers on a single
// /readyz request. Individual checkers should respect context cancellation.
const checkTimeout = 5 * time.Second

// Checker is implemented by any component that wants to gate server readiness.
// Check must return nil when the component is healthy and a descriptive,
// human-readable error when it is not. Implementations must honor context
// cancellation and return promptly when ctx is done.
type Checker interface {
	Check(ctx context.Context) error
}

// CheckerFunc is a function that implements [Checker]. It allows an inline
// closure to be registered without defining a named type.
//
//	reg.Register("sqlite", health.CheckerFunc(func(ctx context.Context) error {
//	    return db.PingContext(ctx)
//	}))
type CheckerFunc func(ctx context.Context) error

// Check implements [Checker].
func (f CheckerFunc) Check(ctx context.Context) error { return f(ctx) }

// Registry holds a set of named [Checker]s consulted by [Registry.ReadinessHandler].
// It is safe for concurrent use; components may call [Registry.Register] from
// any goroutine, including during server startup.
type Registry struct {
	mu       sync.RWMutex
	checkers map[string]Checker
}

// NewRegistry returns an empty [Registry].
func NewRegistry() *Registry {
	return &Registry{checkers: make(map[string]Checker)}
}

// Register adds a named [Checker] to the registry. If a checker with the same
// name already exists it is replaced. Safe to call concurrently.
func (r *Registry) Register(name string, c Checker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checkers[name] = c
}

// LivenessHandler returns an [http.Handler] for GET /healthz.
//
// It always responds HTTP 200 with {"status":"ok"} and performs no dependency
// checks. Use this as a process-alive probe — if the handler stops responding
// the process should be restarted.
func (*Registry) LivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, livenessResponse{Status: "ok"})
	})
}

// ReadinessHandler returns an [http.Handler] for GET /readyz.
//
// It runs all registered checkers concurrently under a [checkTimeout] deadline
// derived from the incoming request context. The response body always includes
// a per-checker breakdown regardless of overall status.
//
// HTTP 200 {"status":"ok","checks":{…}}       — all checkers passed.
// HTTP 503 {"status":"degraded","checks":{…}} — one or more checkers failed.
func (r *Registry) ReadinessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), checkTimeout)
		defer cancel()

		// Copy the checker map under a read lock so we hold the lock as
		// briefly as possible and don't block Register calls during the checks.
		r.mu.RLock()
		snapshot := make(map[string]Checker, len(r.checkers))
		maps.Copy(snapshot, r.checkers)
		r.mu.RUnlock()

		results := make(map[string]checkResult, len(snapshot))

		if len(snapshot) == 0 {
			writeJSON(w, http.StatusOK, readinessResponse{Status: "ok", Checks: results})
			return
		}

		var (
			mu sync.Mutex
			wg sync.WaitGroup
		)

		for name, checker := range snapshot {
			wg.Add(1)

			go func(n string, c Checker) {
				defer wg.Done()

				res := checkResult{Status: "ok"}
				if err := c.Check(ctx); err != nil {
					res = checkResult{Status: "error", Error: err.Error()}
				}

				mu.Lock()
				results[n] = res
				mu.Unlock()
			}(name, checker)
		}

		wg.Wait()

		overallStatus := "ok"
		httpStatus := http.StatusOK

		for _, res := range results {
			if res.Status == "error" {
				overallStatus = "degraded"
				httpStatus = http.StatusServiceUnavailable

				break
			}
		}

		writeJSON(w, httpStatus, readinessResponse{Status: overallStatus, Checks: results})
	})
}

// ── JSON shapes ───────────────────────────────────────────────────────────────

type livenessResponse struct {
	Status string `json:"status"`
}

type readinessResponse struct {
	Status string                 `json:"status"`
	Checks map[string]checkResult `json:"checks"`
}

// checkResult is the per-checker entry in the /readyz response body.
type checkResult struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// ── helpers ───────────────────────────────────────────────────────────────────

// writeJSON marshals v to JSON and writes it with the given HTTP status code.
// Content-Type is set to application/json. If marshaling fails, a plain-text
// 500 is returned instead — this should never happen for the fixed structs used
// in this package.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"status":"error","message":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b) //nolint:errcheck // response write errors cannot be handled after headers are sent
}
