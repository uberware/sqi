// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/store/storetest"
)

// helperEnv makes the test binary stand in for sqi-server: TestMain sees it and
// runs [runHelper] instead of the tests. It is not SQI_-prefixed, so the
// child environment keeps it.
const (
	helperEnv    = "LOADTARGET_TEST_HELPER"
	helperPIDEnv = "LOADTARGET_TEST_PIDFILE"
)

var (
	binOnce                       sync.Once
	binDir                        string
	serverBin, workerBin, noopBin string
	errBuild                      error
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(runHelper(mode))
	}
	code := m.Run()
	if binDir != "" {
		os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// runHelper is a fake server that never becomes ready. "hang" exits on the
// first interrupt; "stubborn" swallows interrupts, so only a kill stops it.
func runHelper(mode string) int {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt)
	if f := os.Getenv(helperPIDEnv); f != "" {
		if err := os.WriteFile(f, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			return 3
		}
	}
	fmt.Println("helper: running as a server that never becomes ready")
	for range sigs {
		if mode == "hang" {
			return 0
		}
	}
	return 0
}

// binaries builds sqi-server, sqi-worker and the no-op command once per test
// process.
func binaries(t *testing.T) (server, worker, noop string) {
	t.Helper()
	binOnce.Do(func() {
		// Not t.TempDir: the binaries outlive the test that builds them and
		// serve every later one; TestMain removes the directory.
		binDir, errBuild = os.MkdirTemp("", "sqi-load-target-bin-") //nolint:usetesting // shared across tests, see above
		if errBuild != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		serverBin, workerBin, noopBin, errBuild = BuildBinaries(ctx, binDir)
	})
	if errBuild != nil {
		t.Fatalf("BuildBinaries: %v", errBuild)
	}
	return serverBin, workerBin, noopBin
}

// testLogger writes the target's own log lines into the test's output.
func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func skipShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs real sqi binaries; skipped with -short")
	}
}

func testScenario(spec scenario.ServerSpec) *scenario.Scenario {
	return &scenario.Scenario{
		Name:   "target-test",
		Server: spec,
		Farm: scenario.FarmSpec{
			Queues:     []scenario.QueueSpec{{Name: "render", MaxConcurrentTasks: 4}, {Name: "comp"}},
			UsagePools: []scenario.PoolSpec{{Name: "arnold", Limit: 3}},
		},
	}
}

// startManaged starts a managed target and closes it when the test ends, so a
// failing assertion never leaks the server.
func startManaged(t *testing.T, cfg ManagedConfig) *managed {
	t.Helper()
	cfg.Logger = testLogger(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	tgt, err := StartManaged(ctx, cfg)
	if err != nil {
		t.Fatalf("StartManaged: %v", err)
	}
	m, ok := tgt.(*managed)
	if !ok {
		t.Fatalf("StartManaged returned %T, want *managed", tgt)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		if err := m.Close(cctx); err != nil {
			t.Errorf("cleanup Close: %v", err)
		}
	})
	return m
}

func mustClient(t *testing.T, tgt Target) *client.Client {
	t.Helper()
	c, err := NewClient(tgt)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// assertFarm checks through the REST API that the target's farm, queues and
// pools exist.
func assertFarm(t *testing.T, c *client.Client, tgt Target, sc *scenario.Scenario) {
	t.Helper()
	ctx := t.Context()
	if _, err := c.GetFarm(ctx, tgt.FarmID()); err != nil {
		t.Fatalf("GetFarm(%s): %v", tgt.FarmID(), err)
	}
	queues, err := c.ListQueues(ctx, tgt.FarmID())
	if err != nil {
		t.Fatalf("ListQueues: %v", err)
	}
	ids := tgt.QueueIDs()
	if len(queues) != len(sc.Farm.Queues) || len(ids) != len(sc.Farm.Queues) {
		t.Fatalf("farm has %d queues, target maps %d, scenario declares %d", len(queues), len(ids), len(sc.Farm.Queues))
	}
	for _, q := range queues {
		if ids[q.Name] != q.ID {
			t.Errorf("queue %s has id %s, target maps %s", q.Name, q.ID, ids[q.Name])
		}
	}
	pools, err := c.ListUsagePools(ctx)
	if err != nil {
		t.Fatalf("ListUsagePools: %v", err)
	}
	for _, want := range sc.Farm.UsagePools {
		found := false
		for _, p := range pools {
			if p.Name == PoolName(tgt, want.Name) && p.MaxConcurrent == want.Limit {
				found = true
			}
		}
		if !found {
			t.Errorf("usage pool %s (limit %d) not found in %+v", want.Name, want.Limit, pools)
		}
	}
}

func TestStartManaged_SQLite(t *testing.T) {
	skipShort(t)
	server, _, _ := binaries(t)
	tests := []struct {
		name string
		spec scenario.ServerSpec
	}{
		{"plain", scenario.ServerSpec{Store: "sqlite"}},
		{"broker and http auth", scenario.ServerSpec{Store: "sqlite", BrokerAuth: true, HTTPAuth: true}},
		{"tls", scenario.ServerSpec{Store: "sqlite", TLS: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runDir := t.TempDir()
			sc := testScenario(tt.spec)
			m := startManaged(t, ManagedConfig{RunDir: runDir, ServerBin: server, Scenario: sc})

			c := mustClient(t, m)
			if err := c.Ready(t.Context()); err != nil {
				t.Fatalf("/readyz: %v", err)
			}
			assertFarm(t, c, m, sc)
			if m.DataDir() != filepath.Join(runDir, "data") {
				t.Errorf("DataDir = %q", m.DataDir())
			}

			switch {
			case tt.spec.HTTPAuth:
				if m.APIKey() == "" {
					t.Error("http_auth target has no API key")
				}
				// Without the key the gate refuses the same call.
				_, err := client.New(m.HTTPURL()).ListQueues(t.Context(), m.FarmID())
				var apiErr *client.APIError
				if !errors.As(err, &apiErr) || apiErr.Status != 401 {
					t.Errorf("unauthenticated ListQueues err = %v, want a 401", err)
				}
			default:
				if m.APIKey() != "" {
					t.Errorf("APIKey = %q without http_auth", m.APIKey())
				}
			}
			if tt.spec.BrokerAuth {
				// The reusable join token enrolls any number of workers.
				for i := range 2 {
					seed, pub, err := m.Credentials().For(t.Context(), fmt.Sprintf("enroll-check-%d", i))
					if err != nil || len(seed) == 0 || pub == "" {
						t.Fatalf("enroll %d: seed %d bytes, key %q, err %v", i, len(seed), pub, err)
					}
				}
			} else if _, ok := m.Credentials().(simworker.NoCredentials); !ok {
				t.Errorf("Credentials = %T without broker_auth, want NoCredentials", m.Credentials())
			}
			if tt.spec.TLS {
				if !strings.HasPrefix(m.HTTPURL(), "https://") || m.TLS().CAFile == "" || m.TLS().Enabled != "true" {
					t.Errorf("tls target: HTTPURL %q, TLS %+v", m.HTTPURL(), m.TLS())
				}
			}

			pid := m.serverPID()
			if got := PIDs(m); !slices.Equal(got, []int{pid}) {
				t.Errorf("PIDs = %v, want [%d]", got, pid)
			}
			if !ProcessAlive(pid) {
				t.Errorf("ProcessAlive(%d) = false for the running server", pid)
			}
			if dsn := PostgresDSN(m); dsn != "" {
				t.Errorf("PostgresDSN = %q on SQLite", dsn)
			}
			if err := m.Close(t.Context()); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if ProcessAlive(pid) {
				t.Errorf("server pid %d still running after Close", pid)
			}
			info, err := os.Stat(filepath.Join(runDir, "server.log"))
			if err != nil || info.Size() == 0 {
				t.Errorf("server.log: %v (size %d)", err, sizeOf(info))
			}
			assertCleanStop(t, filepath.Join(runDir, "server.log"))
			if err := m.Close(t.Context()); err != nil {
				t.Errorf("second Close: %v", err)
			}
		})
	}
}

// assertCleanStop checks that Close stopped the server with an interrupt it
// handled, not a kill. A Windows harness with no console cannot send
// CTRL_BREAK_EVENT and falls back to a kill, so there it is only logged.
func assertCleanStop(t *testing.T, logPath string) {
	t.Helper()
	tail := tailLines(logPath, 10)
	if strings.Contains(tail, "sqi-server stopped cleanly") {
		return
	}
	if runtime.GOOS == "windows" {
		t.Logf("sqi-server was not stopped cleanly (no console to send CTRL_BREAK_EVENT?); log tail:\n%s", tail)
		return
	}
	t.Errorf("sqi-server was not stopped cleanly; log tail:\n%s", tail)
}

func sizeOf(info os.FileInfo) int64 {
	if info == nil {
		return -1
	}
	return info.Size()
}

const noopJob = `specificationVersion: jobtemplate-2023-09
name: noop
steps:
  - name: step-1
    script:
      actions:
        onRun:
          command: %q
          args: [%s]
`

func TestStartManaged_RealWorker(t *testing.T) {
	skipShort(t)
	server, worker, noop := binaries(t)
	tests := []struct {
		name string
		spec scenario.ServerSpec
	}{
		{"plain", scenario.ServerSpec{Store: "sqlite"}},
		{"secured", scenario.ServerSpec{Store: "sqlite", BrokerAuth: true, HTTPAuth: true, TLS: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runDir := t.TempDir()
			sc := testScenario(tt.spec)
			m := startManaged(t, ManagedConfig{
				RunDir: runDir, ServerBin: server, WorkerBin: worker, NoopBin: noop, Scenario: sc,
				RealWorkers: []scenario.WorkerGroup{{Name: "real", Real: 1, Queues: []string{"render"}, Tags: map[string]string{"load": "real"}}},
			})
			c := mustClient(t, m)
			ctx := t.Context()

			workers, err := c.ListWorkers(ctx, m.FarmID(), 100, 0)
			if err != nil || len(workers.Items) != 1 || workers.Items[0].Status != "online" {
				t.Fatalf("workers after StartManaged = %+v, %v; want one online", workers.Items, err)
			}

			cmd := m.NoopCommand()
			if cmd.Path != noop {
				t.Errorf("NoopCommand path = %q, want %q", cmd.Path, noop)
			}
			args := append(append([]string(nil), cmd.Args...), scenario.ClassArg("noop"))
			quoted := make([]string, len(args))
			for i, a := range args {
				quoted[i] = strconv.Quote(a)
			}
			tmpl := fmt.Sprintf(noopJob, cmd.Path, strings.Join(quoted, ", "))
			jobID, _, err := c.SubmitJob(ctx, client.SubmitRequest{FarmID: m.FarmID(), QueueID: m.QueueIDs()["render"], Template: []byte(tmpl)})
			if err != nil {
				t.Fatalf("SubmitJob: %v", err)
			}
			deadline := time.Now().Add(60 * time.Second)
			var job client.Job
			for time.Now().Before(deadline) {
				job, err = c.GetJob(ctx, jobID)
				if err != nil {
					t.Fatalf("GetJob: %v", err)
				}
				if job.Status == "completed" || job.Status == "failed" || job.Status == "canceled" {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			if job.Status != "completed" {
				tasks, _ := c.ListTasks(ctx, jobID, 10, 0) //nolint:errcheck // diagnostics only
				t.Fatalf("job status = %q after 60s, tasks %+v; worker log:\n%s", job.Status, tasks.Items,
					tailLines(filepath.Join(runDir, "workers", "1.log"), 40))
			}

			pids := append([]int{m.serverPID()}, m.workerPIDs()...)
			if err := m.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
			for _, pid := range pids {
				if processAlive(pid) {
					t.Errorf("pid %d still running after Close", pid)
				}
			}
			if info, err := os.Stat(filepath.Join(runDir, "workers", "1.log")); err != nil || info.Size() == 0 {
				t.Errorf("worker log: %v (size %d)", err, sizeOf(info))
			}
		})
	}
}

// TestStartManaged_Postgres runs the server on a database the harness creates
// through SQI_TEST_POSTGRES_DSN (an administrative DSN, as make test-postgres
// provides) and drops on Close.
func TestStartManaged_Postgres(t *testing.T) {
	skipShort(t)
	if !storetest.PostgresEnabled() {
		t.Skip("PostgreSQL not configured: set SQI_TEST_POSTGRES_DSN, or run make test-postgres")
	}
	admin := os.Getenv("SQI_TEST_POSTGRES_DSN")
	if admin == "" {
		t.Fatal("SQI_TEST_POSTGRES_DSN is unset")
	}
	server, _, _ := binaries(t)
	runID := "pg-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	sc := testScenario(scenario.ServerSpec{Store: "postgres", BrokerAuth: true})
	m := startManaged(t, ManagedConfig{RunDir: t.TempDir(), RunID: runID, ServerBin: server, Scenario: sc, PostgresDSN: admin})
	assertFarm(t, mustClient(t, m), m, sc)

	db := databaseName(runID)
	exists := func() bool {
		var n int
		conn, err := sql.Open("pgx", admin)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := conn.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_database WHERE datname = $1`, db).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if !exists() {
		t.Fatalf("database %s does not exist while the target runs", db)
	}
	if dsn := PostgresDSN(m); !strings.Contains(dsn, db) {
		t.Errorf("PostgresDSN = %q, want the run's database %s", dsn, db)
	}
	if err := m.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if exists() {
		t.Errorf("database %s still exists after Close", db)
	}
}

// TestStartManaged_FailedStartLeavesNoProcess covers the startup error paths:
// a server that exits before it is ready, and one that never becomes ready,
// including one that ignores the interrupt and has to be killed.
func TestStartManaged_FailedStartLeavesNoProcess(t *testing.T) {
	skipShort(t)
	server, _, _ := binaries(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		bin     string
		helper  string
		config  map[string]any
		wantErr string
	}{
		{"server exits at startup", server, "", map[string]any{"log": map[string]any{"level": "bogus"}}, "log.level"},
		{"server never ready", self, "hang", nil, "not ready"},
		{"server ignores interrupt", self, "stubborn", nil, "not ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runDir := t.TempDir()
			pidFile := filepath.Join(runDir, "helper.pid")
			if tt.helper != "" {
				t.Setenv(helperEnv, tt.helper)
				t.Setenv(helperPIDEnv, pidFile)
			}
			sc := testScenario(scenario.ServerSpec{Store: "sqlite", Config: tt.config})
			start := time.Now()
			tgt, err := StartManaged(t.Context(), ManagedConfig{
				RunDir: runDir, ServerBin: tt.bin, Scenario: sc,
				readyTimeout: 3 * time.Second, stopTimeout: 2 * time.Second,
			})
			if err == nil {
				tgt.Close(context.Background())
				t.Fatal("StartManaged succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want one containing %q", err, tt.wantErr)
			}
			if elapsed := time.Since(start); elapsed > 30*time.Second {
				t.Errorf("StartManaged took %s to fail", elapsed)
			}
			if tt.helper != "" {
				raw, err := os.ReadFile(pidFile)
				if err != nil {
					t.Fatalf("helper pid file: %v", err)
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
				if err != nil {
					t.Fatal(err)
				}
				if processAlive(pid) {
					t.Errorf("helper pid %d still running after the failed start", pid)
				}
			}
		})
	}
}

func TestAttach_CreatesAndDeletesFarm(t *testing.T) {
	skipShort(t)
	server, _, _ := binaries(t)
	remote := startManaged(t, ManagedConfig{
		RunDir: t.TempDir(), ServerBin: server,
		Scenario: testScenario(scenario.ServerSpec{Store: "sqlite", BrokerAuth: true, HTTPAuth: true}),
	})
	admin := mustClient(t, remote)

	tests := []struct {
		name   string
		keep   bool
		shared bool // SharedPools: the farm goes, the pools stay
	}{
		{"deleted on close", false, false},
		{"kept with keep", true, false},
		{"shared pools stay", false, true},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runID := fmt.Sprintf("attach-%d", i)
			sc := testScenario(scenario.ServerSpec{})
			sc.Farm.UsagePools = []scenario.PoolSpec{{Name: "attach-pool-" + strconv.Itoa(i), Limit: 2}}
			tgt, err := Attach(t.Context(), AttachConfig{
				HTTPURL: remote.HTTPURL(), NATSURL: remote.NATSURL(), APIKey: remote.APIKey(),
				JoinToken: remote.joinToken, Keep: tt.keep, SharedPools: tt.shared, Scenario: sc, RunID: runID,
			})
			if err != nil {
				t.Fatalf("Attach: %v", err)
			}
			farm, err := admin.GetFarm(t.Context(), tgt.FarmID())
			if err != nil || farm.Name != "load-"+runID {
				t.Fatalf("GetFarm = %+v, %v; want load-%s", farm, err, runID)
			}
			assertFarm(t, admin, tgt, sc)
			if tgt.DataDir() != "" || tgt.APIKey() != remote.APIKey() {
				t.Errorf("DataDir %q, APIKey match %v", tgt.DataDir(), tgt.APIKey() == remote.APIKey())
			}
			if _, _, err := tgt.Credentials().For(t.Context(), "attach-enroll-"+strconv.Itoa(i)); err != nil {
				t.Errorf("enroll through the attach target: %v", err)
			}
			// A job in the farm must not stop the farm being deleted.
			tmpl := fmt.Sprintf(noopJob, "sqi-load-noop", strconv.Quote(scenario.ClassArg("x")))
			if _, _, err := admin.SubmitJob(t.Context(), client.SubmitRequest{
				FarmID: tgt.FarmID(), QueueID: tgt.QueueIDs()["render"], Template: []byte(tmpl),
			}); err != nil {
				t.Fatalf("SubmitJob: %v", err)
			}

			if err := tgt.Close(t.Context()); err != nil {
				t.Fatalf("Close: %v", err)
			}
			_, err = admin.GetFarm(t.Context(), tgt.FarmID())
			var apiErr *client.APIError
			gone := errors.As(err, &apiErr) && apiErr.Status == 404
			if gone == tt.keep {
				t.Errorf("after Close (keep=%v): GetFarm err = %v", tt.keep, err)
			}
			pools, err := admin.ListUsagePools(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			poolLeft := false
			for _, p := range pools {
				poolLeft = poolLeft || p.Name == PoolName(tgt, sc.Farm.UsagePools[0].Name)
			}
			if poolLeft != (tt.keep || tt.shared) {
				t.Errorf("after Close (keep=%v, shared=%v): pool present = %v", tt.keep, tt.shared, poolLeft)
			}
		})
	}
}

// poolByName returns the server's usage pool of that name.
func poolByName(t *testing.T, c *client.Client, name string) (client.UsagePool, bool) {
	t.Helper()
	pools, err := c.ListUsagePools(t.Context())
	if err != nil {
		t.Fatalf("ListUsagePools: %v", err)
	}
	for _, p := range pools {
		if p.Name == name {
			return p, true
		}
	}
	return client.UsagePool{}, false
}

// TestAttach_NamespacesUsagePools checks that an attached run never claims a
// usage pool somebody else uses: the server already has a production pool
// named like the scenario's, and the run must create, claim and delete a pool
// of its own beside it, and leave the production one exactly as it was. A
// pool the run's own name already exists under fails the setup, unless the
// shards of one run share it.
func TestAttach_NamespacesUsagePools(t *testing.T) {
	skipShort(t)
	server, _, _ := binaries(t)
	remote := startManaged(t, ManagedConfig{
		RunDir: t.TempDir(), ServerBin: server,
		Scenario: testScenario(scenario.ServerSpec{Store: "sqlite", BrokerAuth: true, HTTPAuth: true}),
	})
	admin := mustClient(t, remote)
	ctx := t.Context()
	// The managed remote created the scenario's "arnold" pool, limit 3, under
	// its own name: the production pool here.
	prod, ok := poolByName(t, admin, "arnold")
	if !ok {
		t.Fatal("the remote has no production pool arnold")
	}
	attach := func(runID, poolRunID string, shared bool) (Target, error) {
		sc := testScenario(scenario.ServerSpec{})
		sc.Farm.UsagePools = []scenario.PoolSpec{{Name: "arnold", Limit: 2}}
		return Attach(ctx, AttachConfig{
			HTTPURL: remote.HTTPURL(), NATSURL: remote.NATSURL(), APIKey: remote.APIKey(),
			JoinToken: remote.joinToken, SharedPools: shared, Scenario: sc, RunID: runID, PoolRunID: poolRunID,
			Logger: testLogger(t),
		})
	}

	t.Run("own pool beside the production one", func(t *testing.T) {
		tgt, err := attach("ns-1", "", false)
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
		own := PoolName(tgt, "arnold")
		if own != "load_ns_1_arnold" {
			t.Fatalf("PoolName(arnold) = %q, want load_ns_1_arnold", own)
		}
		if p, ok := poolByName(t, admin, own); !ok || p.MaxConcurrent != 2 {
			t.Fatalf("the run's pool %s = %+v (found %v), want limit 2", own, p, ok)
		}
		// A job claiming the run's pool by its server name is accepted.
		tmpl := fmt.Sprintf(poolJob, own)
		if _, _, err := admin.SubmitJob(ctx, client.SubmitRequest{FarmID: tgt.FarmID(), QueueID: tgt.QueueIDs()["render"], Template: []byte(tmpl)}); err != nil {
			t.Fatalf("SubmitJob claiming %s: %v", own, err)
		}
		if err := tgt.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if _, ok := poolByName(t, admin, own); ok {
			t.Errorf("the run's pool %s is still there after Close", own)
		}
		if p, ok := poolByName(t, admin, "arnold"); !ok || p != prod {
			t.Errorf("production pool after the run = %+v (found %v), want it untouched: %+v", p, ok, prod)
		}
	})

	t.Run("an existing pool of the run's name fails the setup", func(t *testing.T) {
		taken := RunPoolName("ns-2", "arnold")
		id, err := admin.CreateUsagePool(ctx, taken, 9)
		if err != nil {
			t.Fatalf("CreateUsagePool(%s): %v", taken, err)
		}
		t.Cleanup(func() { admin.DeleteUsagePool(context.Background(), id) }) //nolint:errcheck // best-effort cleanup
		tgt, err := attach("ns-2", "", false)
		if err == nil {
			tgt.Close(context.Background()) //nolint:errcheck // the assertion below fails anyway
			t.Fatal("Attach succeeded on a server that already has the run's pool")
		}
		if !strings.Contains(err.Error(), taken) || !strings.Contains(err.Error(), "already exists") {
			t.Errorf("err = %v, want one naming %s as already existing", err, taken)
		}
		farms, err := admin.ListFarms(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range farms {
			if f.Name == "load-ns-2" {
				t.Errorf("the failed setup left farm %s behind", f.Name)
			}
		}

		// A shard of run ns-2 shares the pool another shard created.
		shard, err := attach("ns-2-s1", "ns-2", true)
		if err != nil {
			t.Fatalf("Attach as a shard: %v", err)
		}
		if got := PoolName(shard, "arnold"); got != taken {
			t.Errorf("shard PoolName = %q, want the run's %q", got, taken)
		}
		if err := shard.Close(ctx); err != nil {
			t.Fatalf("shard Close: %v", err)
		}
		if p, ok := poolByName(t, admin, taken); !ok || p.MaxConcurrent != 9 {
			t.Errorf("the shared pool after a shard's Close = %+v (found %v), want it left in place", p, ok)
		}
	})
}

const poolJob = `specificationVersion: jobtemplate-2023-09
name: pool
steps:
  - name: step-1
    hostRequirements:
      amounts:
        - name: amount.worker.usagepool.%s
          min: 1
    script:
      actions:
        onRun:
          command: sqi-load-noop
`

// TestCleanup_RemovesWhatARunLeft keeps a run's farm (and a shard's) on a
// real server, beside a production pool and another live load run whose name
// extends this one's, and checks that Cleanup removes exactly the run's farms
// and pools.
func TestCleanup_RemovesWhatARunLeft(t *testing.T) {
	skipShort(t)
	server, _, _ := binaries(t)
	remote := startManaged(t, ManagedConfig{
		RunDir: t.TempDir(), ServerBin: server,
		Scenario: testScenario(scenario.ServerSpec{Store: "sqlite", BrokerAuth: true, HTTPAuth: true}),
	})
	admin := mustClient(t, remote)
	ctx := t.Context()
	keep := func(runID, poolRunID string, shared bool) Target {
		t.Helper()
		sc := testScenario(scenario.ServerSpec{})
		tgt, err := Attach(ctx, AttachConfig{
			HTTPURL: remote.HTTPURL(), NATSURL: remote.NATSURL(), APIKey: remote.APIKey(),
			JoinToken: remote.joinToken, Keep: true, SharedPools: shared, Scenario: sc, RunID: runID, PoolRunID: poolRunID,
		})
		if err != nil {
			t.Fatalf("Attach %s: %v", runID, err)
		}
		tmpl := fmt.Sprintf(noopJob, "sqi-load-noop", strconv.Quote(scenario.ClassArg("x")))
		if _, _, err := admin.SubmitJob(ctx, client.SubmitRequest{FarmID: tgt.FarmID(), QueueID: tgt.QueueIDs()["render"], Template: []byte(tmpl)}); err != nil {
			t.Fatalf("SubmitJob: %v", err)
		}
		if _, _, err := tgt.Credentials().For(ctx, runID+"-worker"); err != nil {
			t.Fatalf("enroll: %v", err)
		}
		if err := tgt.Close(ctx); err != nil {
			t.Fatalf("Close (kept): %v", err)
		}
		return tgt
	}
	keep("cl-1", "", false)
	keep("cl-1-s1", "cl-1", true)
	keep("cl-1-2", "", false) // another run, still on the server

	done, err := Cleanup(ctx, CleanupConfig{HTTPURL: remote.HTTPURL(), APIKey: remote.APIKey(), RunID: "cl-1", Logger: testLogger(t)})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	slices.Sort(done.Farms)
	if !slices.Equal(done.Farms, []string{"load-cl-1", "load-cl-1-s1"}) {
		t.Errorf("removed farms %v, want load-cl-1 and load-cl-1-s1", done.Farms)
	}
	if !slices.Equal(done.Pools, []string{"load_cl_1_arnold"}) {
		t.Errorf("removed pools %v, want load_cl_1_arnold", done.Pools)
	}
	farms, err := admin.ListFarms(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range farms {
		names = append(names, f.Name)
	}
	if slices.Contains(names, "load-cl-1") || slices.Contains(names, "load-cl-1-s1") || !slices.Contains(names, "load-cl-1-2") {
		t.Errorf("farms after cleanup: %v, want only the other run's load-cl-1-2 of the three", names)
	}
	for _, p := range []string{"arnold", "load_cl_1_2_arnold"} {
		if _, ok := poolByName(t, admin, p); !ok {
			t.Errorf("pool %s was removed; it is not the run's", p)
		}
	}

	if _, err := Cleanup(ctx, CleanupConfig{HTTPURL: remote.HTTPURL(), APIKey: remote.APIKey(), RunID: "cl-1-2"}); err != nil {
		t.Errorf("Cleanup of the other run: %v", err)
	}
	if _, ok := poolByName(t, admin, "load_cl_1_2_arnold"); ok {
		t.Error("the other run's pool survived its own cleanup")
	}
}

// TestStartManaged_RefusesANonEmptyPostgres gives a managed run a DSN naming
// a database that already holds sqi's schema, as a server's own database
// does: the run must refuse it before starting a server on it.
func TestStartManaged_RefusesANonEmptyPostgres(t *testing.T) {
	if !storetest.PostgresEnabled() {
		t.Skip("PostgreSQL not configured: set SQI_TEST_POSTGRES_DSN, or run make test-postgres")
	}
	dsn := storetest.PostgresDSN(t) // migrated: it has tables
	sc := testScenario(scenario.ServerSpec{Store: "postgres"})
	tgt, err := StartManaged(t.Context(), ManagedConfig{RunDir: t.TempDir(), ServerBin: "never-started", Scenario: sc, PostgresDSN: dsn})
	if err == nil {
		tgt.Close(context.Background()) //nolint:errcheck // the test fails anyway
		t.Fatal("StartManaged accepted a database that already holds tables")
	}
	if !strings.Contains(err.Error(), "not empty") {
		t.Errorf("err = %v, want one saying the database is not empty", err)
	}
}
