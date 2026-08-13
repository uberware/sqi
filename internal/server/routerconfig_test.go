// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/config"
)

// TestRouterConfig_CarriesTheSubmissionDeadline covers the SECOND of the three
// hops that carry EXPR sub-project H1's wall-clock backstop from the config
// file to the handlers that enforce it: server.Config -> api.Config.
//
// Every other test of the deadline builds a handler with a literal duration, so
// without this one the whole production path could be severed and the suite
// would stay green: internal/config would still validate the range at startup,
// internal/api would still map the error to a 503, and the server would still
// boot -- with no deadline in force. That failure is invisible today (EXPR is
// StatusInProgress, so no submission reaches an expression evaluation) and
// becomes an unbounded request the moment sub-project H2 flips the status,
// which is exactly the wrong time to discover a dropped assignment.
func TestRouterConfig_CarriesTheSubmissionDeadline(t *testing.T) {
	const want = 37 * time.Second // non-default, so a stale default cannot pass
	cfg := DefaultConfig()
	cfg.OpenJDExprSubmissionDeadline = want

	if got := routerConfig(cfg, time.Minute).ExprSubmissionDeadline; got != want {
		t.Fatalf("api.Config.ExprSubmissionDeadline = %s, want the configured %s -- "+
			"the submission handlers would run with no wall-clock backstop at all", got, want)
	}
}

// TestRouterConfig_DefaultCarriesTheConfigDefault pins that a server left alone
// still hands the handlers a real duration. The zero value is legal at this
// boundary and means "no backstop", so "the field is populated" is the whole
// assertion: a hop that silently zeroes it would look identical to one that
// never existed.
func TestRouterConfig_DefaultCarriesTheConfigDefault(t *testing.T) {
	got := routerConfig(DefaultConfig(), time.Minute).ExprSubmissionDeadline
	if want := config.DefaultOpenJDExprSubmissionDeadline; got != want {
		t.Fatalf("at defaults api.Config.ExprSubmissionDeadline = %s, want %s", got, want)
	}
	if got <= 0 {
		t.Fatal("a default server must not disable the backstop")
	}
}

// TestRouterConfig_CarriesTheOtherHTTPSettings guards the extraction itself:
// this literal moved out of start to become reachable from a test, and the
// fields that were already there must still arrive. WorkerOfflineThreshold is
// the one that does NOT come from Config -- start passes the running
// scheduler's normalized timeout -- so it is checked as a parameter.
func TestRouterConfig_CarriesTheOtherHTTPSettings(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CORSOrigins = []string{"https://ui.example"}
	cfg.EnablePprof = true
	cfg.DisableRateLimit = true
	cfg.AuthEnabled = true
	cfg.AuthValidateJobOwner = false

	got := routerConfig(cfg, 90*time.Second)

	if len(got.CORSOrigins) != 1 || got.CORSOrigins[0] != "https://ui.example" {
		t.Errorf("CORSOrigins = %v, want the configured origin", got.CORSOrigins)
	}
	if !got.EnablePprof {
		t.Error("EnablePprof = false, want the configured true")
	}
	if !got.DisableRateLimit {
		t.Error("DisableRateLimit = false, want the configured true")
	}
	if !got.AuthEnabled {
		t.Error("AuthEnabled = false, want the configured true")
	}
	if got.ValidateJobOwner {
		t.Error("ValidateJobOwner = true, want the configured false")
	}
	if got.WorkerOfflineThreshold != 90*time.Second {
		t.Errorf("WorkerOfflineThreshold = %s, want the scheduler's 90s", got.WorkerOfflineThreshold)
	}
}
