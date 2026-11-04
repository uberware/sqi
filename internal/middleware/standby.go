// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"encoding/json"
	"net/http"
)

// standbyRetryAfter is the Retry-After a standby answers with, in seconds:
// about a failover's own pace, so a client retrying through a balancer lands
// on the new leader soon after it opens.
const standbyRetryAfter = "2"

// Standby returns middleware for a server in a high-availability group. While
// isLeader reports false it answers every request other than probes, the
// paths a standby still serves (the probes a supervisor or load balancer
// reads, and its metrics, matched exactly), with 503, Retry-After: 2 and
// {"error":"standby","leader":"<name>"}, where the name is leaderName's, ""
// when no leader is known. Nothing is proxied to the leader: a balancer
// health-checking /leaderz routes there itself.
//
// Both functions are read on every request, so a promotion opens the API
// without rebuilding the router.
func Standby(isLeader func() bool, leaderName func() string, probes []string) func(http.Handler) http.Handler {
	served := make(map[string]bool, len(probes))
	for _, p := range probes {
		served[p] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isLeader() || served[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}
			b, _ := json.Marshal(struct { //nolint:errcheck // a struct of two strings always marshals
				Error  string `json:"error"`
				Leader string `json:"leader"`
			}{"standby", leaderName()})
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", standbyRetryAfter)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(b) //nolint:errcheck // response write errors cannot be handled after headers are sent
		})
	}
}
