// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/scheduler"
)

// TestServerConfig_CarriesTheExprCostBounds covers the FIRST of the three hops
// that carry an operator's expression cost bounds from the config file to the
// code that enforces them: config.Config -> server.Config, here in the CLI.
//
// It exists because nothing else can fail when this hop breaks. Range
// validation still runs at startup, the docs still describe the keys, and every
// test of the enforcing code builds its own value directly — so a dropped or
// renamed line in this literal leaves a server that starts cleanly, reports the
// operator's configuration on `sqi-server config print`, and enforces NOTHING.
// The deadline is the acute case: it is EXPR sub-project H1's only bound on
// wall-clock time. While the EXPR extension was StatusInProgress no submission
// exercised it at all; since H2 flipped the status every EXPR submission does,
// so its absence would now surface as an outage rather than as a test failure.
func TestServerConfig_CarriesTheExprCostBounds(t *testing.T) {
	// Deliberately distinct, non-default values: these are same-typed fields
	// crossing a struct boundary, where a transposition compiles and starts.
	cfg := config.DefaultConfig()
	cfg.OpenJD.ExprSubmissionDeadline = 37 * time.Second
	cfg.OpenJD.ExprOperationLimit = 11
	cfg.OpenJD.ExprMemoryLimit = 22
	cfg.OpenJD.ExprTemplatePositions = 33
	cfg.OpenJD.ExprTemplateRetainedBytes = 44

	got := serverConfig(cfg, scheduler.Config{})

	if want := 37 * time.Second; got.OpenJDExprSubmissionDeadline != want {
		t.Errorf("server.Config.OpenJDExprSubmissionDeadline = %s, want the configured %s -- "+
			"openjd.expr_submission_deadline is not reaching the server, so the wall-clock "+
			"backstop is silently absent", got.OpenJDExprSubmissionDeadline, want)
	}
	want := openjd.ExprLimits{
		SubmissionOperations:  11,
		SubmissionMemoryBytes: 22,
		TemplatePositions:     33,
		TemplateRetainedBytes: 44,
	}
	if got.OpenJDExprLimits != want {
		t.Errorf("server.Config.OpenJDExprLimits = %+v, want %+v", got.OpenJDExprLimits, want)
	}
}

// TestServerConfig_DefaultsAreTheConfigDefaults pins the other half: a server
// started with no configuration at all must run with internal/config's own
// defaults, not with a zero value that would mean "no backstop".
func TestServerConfig_DefaultsAreTheConfigDefaults(t *testing.T) {
	got := serverConfig(config.DefaultConfig(), scheduler.Config{})

	if want := config.DefaultOpenJDExprSubmissionDeadline; got.OpenJDExprSubmissionDeadline != want {
		t.Errorf("at defaults OpenJDExprSubmissionDeadline = %s, want %s",
			got.OpenJDExprSubmissionDeadline, want)
	}
	if got.OpenJDExprLimits != openjd.DefaultExprLimits() {
		t.Errorf("at defaults OpenJDExprLimits = %+v, want %+v",
			got.OpenJDExprLimits, openjd.DefaultExprLimits())
	}
}

// TestServerConfig_CarriesTheRestOfTheConfig guards the extraction itself: this
// literal moved out of runServe to become testable, and every other field it
// maps must still arrive. A spot check across the sections is enough — the
// failure being guarded against is a botched move, not a per-field typo.
func TestServerConfig_CarriesTheRestOfTheConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.HTTP.Addr = ":9999"
	cfg.NATS.Addr = "nats://127.0.0.1:4333"
	cfg.Store.SQLitePath = "/tmp/does-not-need-to-exist.db"
	cfg.Auth.Enabled = true
	cfg.Auth.Session.CookieName = "cookie-under-test"
	cfg.PresetLibrary.URL = "https://presets.example/index.json"
	cfg.OpenJD.EnforceLimits = false

	got := serverConfig(cfg, scheduler.Config{FarmID: "farm-42"})

	if got.HTTPAddr != ":9999" {
		t.Errorf("HTTPAddr = %q, want %q", got.HTTPAddr, ":9999")
	}
	if got.NATSAddr != "nats://127.0.0.1:4333" {
		t.Errorf("NATSAddr = %q, want the configured address", got.NATSAddr)
	}
	if got.SQLitePath != "/tmp/does-not-need-to-exist.db" {
		t.Errorf("SQLitePath = %q, want the configured path", got.SQLitePath)
	}
	if !got.AuthEnabled {
		t.Error("AuthEnabled = false, want the configured true")
	}
	if got.AuthCookieName != "cookie-under-test" {
		t.Errorf("AuthCookieName = %q, want the configured name", got.AuthCookieName)
	}
	if got.PresetLibraryURL != "https://presets.example/index.json" {
		t.Errorf("PresetLibraryURL = %q, want the configured URL", got.PresetLibraryURL)
	}
	if got.EnforceOpenJDLimits {
		t.Error("EnforceOpenJDLimits = true, want the configured false")
	}
	if got.Scheduler.FarmID != "farm-42" {
		t.Errorf("Scheduler.FarmID = %q, want the caller's scheduler config", got.Scheduler.FarmID)
	}
	if !got.SeedDefaults {
		t.Error("SeedDefaults = false; this binary always seeds")
	}
}
