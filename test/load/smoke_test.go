// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build load

package load

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/runner"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/loadtest/target"
	"github.com/uberware/sqi/internal/loadtest/verify"
	"github.com/uberware/sqi/internal/store/storetest"
)

// outDirEnv names a directory the runs are written into instead of a
// per-test temporary one, so CI can upload a known path.
const outDirEnv = "SQI_LOAD_OUT"

// invariantCount is how many invariants the verifier checks.
const invariantCount = 7

var (
	binOnce                     sync.Once
	binDir                      string
	serverBin, workerBin, noBin string
	errBuild                    error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// binaries builds sqi-server, sqi-worker and the no-op command once per test
// process.
func binaries(t *testing.T) *target.ManagedConfig {
	t.Helper()
	binOnce.Do(func() {
		// Shared by every test of the process; TestMain removes it.
		binDir, errBuild = os.MkdirTemp("", "sqi-load-bin-") //nolint:usetesting // shared across tests, see above
		if errBuild != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		serverBin, workerBin, noBin, errBuild = target.BuildBinaries(ctx, binDir)
	})
	if errBuild != nil {
		t.Fatalf("BuildBinaries: %v", errBuild)
	}
	return &target.ManagedConfig{ServerBin: serverBin, WorkerBin: workerBin, NoopBin: noBin}
}

// outDir is where a test's runs are written: SQI_LOAD_OUT when set, otherwise
// a temporary directory.
func outDir(t *testing.T) string {
	t.Helper()
	if d := os.Getenv(outDirEnv); d != "" {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("%s: %v", outDirEnv, err)
		}
		return d
	}
	return t.TempDir()
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// smoke returns the smoke scenario on the named store.
func smoke(t *testing.T, store string) *scenario.Scenario {
	t.Helper()
	sc, err := scenario.Builtin("smoke")
	if err != nil {
		t.Fatal(err)
	}
	if store != "" {
		if err := sc.Set("server.store", store); err != nil {
			t.Fatal(err)
		}
	}
	sc.ApplyDefaults()
	if err := sc.Validate(scenario.TargetManaged); err != nil {
		t.Fatal(err)
	}
	return sc
}

// stores are the backends the smoke scenario runs on: SQLite always,
// PostgreSQL when the PostgreSQL test database is available.
func stores() []string {
	if storetest.PostgresEnabled() {
		return []string{"sqlite", "postgres"}
	}
	return []string{"sqlite"}
}

// managedFor is the target configuration for one store. PostgreSQL gets an
// empty database the harness uses as it is.
func managedFor(t *testing.T, store string) *target.ManagedConfig {
	t.Helper()
	cfg := *binaries(t)
	if store == "postgres" {
		cfg.PostgresDSN = storetest.PostgresEmptyDSN(t)
	}
	return &cfg
}

// TestLoadSmoke runs the smoke scenario (20 simulated workers, 50 jobs of 2
// steps x 10 tasks with logs, 5 WebSocket clients and one expected crash)
// against a real managed sqi-server on SQLite, and on PostgreSQL too when
// storetest.PostgresEnabled. Every task must end terminal on the server and
// every invariant must hold.
//
// Each run's directory is logged with its summary.md. It lies under
// $SQI_LOAD_OUT when that is set and under t.TempDir otherwise.
func TestLoadSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real sqi binaries for about a minute per store; skipped with -short")
	}
	out := outDir(t)
	for _, store := range stores() {
		t.Run(store, func(t *testing.T) {
			runID := runner.NewRunID(time.Now())
			rec, err := runner.Run(t.Context(), runner.Options{
				Scenario: smoke(t, store),
				RunID:    runID,
				OutDir:   out,
				Seed:     1,
				Managed:  managedFor(t, store),
				Logger:   testLogger(t),
			})
			dir := filepath.Join(out, runID)
			t.Logf("run directory: %s", dir)
			t.Logf("summary: %s", filepath.Join(dir, "summary.md"))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !rec.Complete || !rec.Valid {
				t.Errorf("record complete=%v valid=%v (%s), want both", rec.Complete, rec.Valid, rec.InvalidReason)
			}
			if len(rec.Invariants) != invariantCount {
				t.Fatalf("%d invariants, want %d: %+v", len(rec.Invariants), invariantCount, rec.Invariants)
			}
			for _, inv := range rec.Invariants {
				if !inv.OK {
					t.Errorf("invariant %s failed: %q", inv.Name, inv.Evidence)
				}
			}
			if got := rec.Counters[report.CounterTasksTerminal]; got != 1000 {
				t.Errorf("tasks.terminal = %d, want 1000", got)
			}
			if _, err := os.Stat(filepath.Join(dir, "summary.md")); err != nil {
				t.Errorf("summary.md: %v", err)
			}
		})
	}
}

// TestLoadSmoke_VerifierCanFail proves the verifier can fail a run that went
// wrong: just before the invariants are checked, one task's ledger outcome is
// made to disagree with what the server holds (the ledger says the task
// failed; the server saw it succeed), standing in for a worker that tells the
// ledger one thing and the server another. The outcomes invariant must fail
// and name that task.
//
// Smoke has no fail_rate, so every task succeeds and a succeeded one is the
// one flipped.
func TestLoadSmoke_VerifierCanFail(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real sqi binaries for about a minute; skipped with -short")
	}
	var flipped string
	rec, err := runner.Run(t.Context(), runner.Options{
		Scenario: smoke(t, "sqlite"),
		RunID:    runner.NewRunID(time.Now()),
		OutDir:   outDir(t),
		Seed:     1,
		Managed:  managedFor(t, "sqlite"),
		Logger:   testLogger(t),
		BeforeVerify: func(in *verify.Input) {
			for _, e := range in.Ledger.Events() {
				if e.Kind == simworker.KindTerminal && e.Outcome == simworker.OutcomeSucceeded {
					if in.Ledger.SetOutcome(e.TaskID, simworker.OutcomeFailed) {
						flipped = e.TaskID
						return
					}
				}
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if flipped == "" {
		t.Fatal("the ledger held no succeeded task to corrupt")
	}
	var found bool
	for _, inv := range rec.Invariants {
		if inv.Name != verify.NameOutcomes {
			continue
		}
		found = true
		if inv.OK {
			t.Fatal("outcomes held although one ledger outcome was made to disagree with the server")
		}
		if !strings.Contains(strings.Join(inv.Evidence, "\n"), flipped) {
			t.Errorf("outcomes evidence does not name task %s: %q", flipped, inv.Evidence)
		}
	}
	if !found {
		t.Fatalf("no %s invariant in the record: %+v", verify.NameOutcomes, rec.Invariants)
	}
}
