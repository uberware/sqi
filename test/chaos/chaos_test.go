// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build chaos

package chaos

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/runner"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/target"
	"github.com/uberware/sqi/internal/loadtest/verify"
	"github.com/uberware/sqi/internal/store/storetest"
)

const (
	// outDirEnv names a directory the runs are written into instead of a
	// per-test temporary one, so CI can upload a known path.
	outDirEnv = "SQI_LOAD_OUT"
	// storeEnv selects the store every scenario runs on: sqlite (the
	// default) or postgres, which needs SQI_TEST_POSTGRES_DSN.
	storeEnv = "SQI_CHAOS_STORE"
	// chaosPrefix marks the built-in scenarios the suite runs.
	chaosPrefix = "chaos-"
)

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
		binDir, errBuild = os.MkdirTemp("", "sqi-chaos-bin-") //nolint:usetesting // shared across tests, see above
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

// chaosStore is the store SQI_CHAOS_STORE selects.
func chaosStore(t *testing.T) string {
	t.Helper()
	switch s := os.Getenv(storeEnv); s {
	case "", "sqlite":
		return "sqlite"
	case "postgres":
		return s
	default:
		t.Fatalf("%s=%q: want sqlite or postgres", storeEnv, s)
		return ""
	}
}

// managedFor is the target configuration for one store. PostgreSQL gets an
// empty database the harness uses as it is; without SQI_TEST_POSTGRES_DSN
// the test skips (or fails, with SQI_TEST_REQUIRE_POSTGRES=1).
func managedFor(t *testing.T, store string) *target.ManagedConfig {
	t.Helper()
	cfg := *binaries(t)
	if store == "postgres" {
		cfg.PostgresDSN = storetest.PostgresEmptyDSN(t)
	}
	return &cfg
}

// chaosNames lists the built-in chaos scenarios, sorted.
func chaosNames() []string {
	var names []string
	for _, n := range scenario.BuiltinNames() {
		if strings.HasPrefix(n, chaosPrefix) {
			names = append(names, n)
		}
	}
	return names
}

// chaosScenario returns the named built-in scenario on store, changed by
// mutate when it is set, defaults applied and validated.
func chaosScenario(t *testing.T, name, store string, mutate func(*scenario.Scenario)) *scenario.Scenario {
	t.Helper()
	sc, err := scenario.Builtin(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Set("server.store", store); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(sc)
	}
	sc.ApplyDefaults()
	if err := sc.Validate(scenario.TargetManaged); err != nil {
		t.Fatal(err)
	}
	return sc
}

// stallReason says why sc cannot run on this OS: a stall is SIGSTOP and
// SIGCONT, which Windows does not have. It is "" when sc can run.
func stallReason(sc *scenario.Scenario) string {
	if runtime.GOOS != "windows" {
		return ""
	}
	for i, f := range sc.Faults {
		if f.Stall == nil {
			continue
		}
		what := "the server"
		if !f.Stall.Server {
			what = "real workers of group " + f.Stall.Group
		}
		return fmt.Sprintf("faults[%d] stalls %s, and a stall (SIGSTOP/SIGCONT) is POSIX only", i, what)
	}
	return ""
}

// runChaos runs sc once with seed 1 and returns its record and run directory.
// A run that returns an error fails the test, with the summary logged.
func runChaos(t *testing.T, sc *scenario.Scenario, store string) (rec *report.Record, dir string) {
	t.Helper()
	out := outDir(t)
	runID := runner.NewRunID(time.Now())
	rec, err := runner.Run(t.Context(), runner.Options{
		Scenario: sc,
		RunID:    runID,
		OutDir:   out,
		Seed:     1,
		Managed:  managedFor(t, store),
		Logger:   testLogger(t),
	})
	dir = filepath.Join(out, runID)
	t.Logf("run directory: %s", dir)
	if err != nil {
		logSummary(t, dir)
		t.Fatalf("Run: %v", err)
	}
	if rec == nil {
		t.Fatal("Run returned no record")
	}
	return rec, dir
}

// logSummary logs the run's summary.md, or why it could not be read.
func logSummary(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "summary.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Logf("summary %s: %v", path, err)
		return
	}
	t.Logf("summary %s:\n%s", path, data)
}

// TestChaos runs every built-in chaos-* scenario once, seed 1, against real
// sqi binaries on the store SQI_CHAOS_STORE selects (SQLite by default). A
// scenario passes when its record is complete and every result holds, known
// violations applied: a failure the scenario expects counts as held, and an
// expected "must" failure that did not happen is a failed result of its own.
//
// Scenarios that stall a process are skipped on Windows. Each run's directory
// is logged, and on failure its summary.md; it lies under $SQI_LOAD_OUT when
// that is set and under t.TempDir otherwise.
func TestChaos(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real sqi binaries for several minutes per scenario; skipped with -short")
	}
	store := chaosStore(t)
	names := chaosNames()
	if len(names) == 0 {
		t.Fatalf("no built-in scenario is named %s*", chaosPrefix)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			sc := chaosScenario(t, name, store, nil)
			if why := stallReason(sc); why != "" {
				t.Skipf("skipped on %s: %s", runtime.GOOS, why)
			}
			rec, dir := runChaos(t, sc, store)
			if !checkRecord(t, rec) {
				logSummary(t, dir)
			}
		})
	}
}

// checkRecord fails t unless rec is complete with every result OK, and logs
// the failures a known violation excused. It reports whether rec passed.
func checkRecord(t *testing.T, rec *report.Record) bool {
	t.Helper()
	ok := true
	if !rec.Complete {
		t.Errorf("record is incomplete: the run ended before its planned duration")
		ok = false
	}
	if !rec.Valid {
		t.Logf("record is marked invalid (%s); its invariants are still judged", rec.InvalidReason)
	}
	if len(rec.Invariants) == 0 {
		t.Errorf("record holds no invariant results")
		ok = false
	}
	for _, inv := range rec.Invariants {
		switch {
		case !inv.OK:
			t.Errorf("%s failed: %q", inv.Name, inv.Evidence)
			ok = false
		case inv.Known != "":
			t.Logf("%s failed as expected (%s): %q", inv.Name, inv.Known, inv.Evidence)
		}
	}
	return ok
}

// TestChaos_MustViolationThatDoesNotFireFails proves a "must" known violation
// is enforced: chaos-server-term run with an expected outcomes failure that
// does not happen must record a failed known:outcomes result.
func TestChaos_MustViolationThatDoesNotFireFails(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real sqi binaries for several minutes; skipped with -short")
	}
	store := chaosStore(t)
	sc := chaosScenario(t, "chaos-server-term", store, func(sc *scenario.Scenario) {
		sc.Expect.KnownViolations = append(sc.Expect.KnownViolations, scenario.KnownViolation{
			Check:   verify.NameOutcomes,
			Kind:    scenario.KnownMust,
			Reason:  "proves must is enforced",
			Tracker: "H13-test",
		})
	})
	rec, dir := runChaos(t, sc, store)
	if !rec.Complete {
		logSummary(t, dir)
		t.Fatal("record is incomplete, so no expectations were applied")
	}
	want := "known:" + verify.NameOutcomes
	for _, inv := range rec.Invariants {
		if inv.Name != want {
			continue
		}
		if inv.OK {
			t.Fatalf("%s is OK, want failed: %q", want, inv.Evidence)
		}
		t.Logf("%s failed as it must: %q", want, inv.Evidence)
		return
	}
	logSummary(t, dir)
	for _, inv := range rec.Invariants {
		if inv.Name == verify.NameOutcomes {
			t.Fatalf("no %s result; %s itself has OK=%v known=%q evidence %q (an outcomes failure in this run would satisfy the must entry)",
				want, inv.Name, inv.OK, inv.Known, inv.Evidence)
		}
	}
	t.Fatalf("no %s result and no %s result in the record: %+v", want, verify.NameOutcomes, rec.Invariants)
}
