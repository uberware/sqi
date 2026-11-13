// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/internal/proctest"
	"github.com/uberware/sqi/internal/loadtest/netfault"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/store/backend"
)

// The signal helpers' environment. "parent" starts a "child" and writes its
// pid to the file helperChildPIDEnv names; "counter" writes a rising count to
// the file helperCounterEnv names every 50ms and exits on an interrupt.
const (
	helperChildPIDEnv = "LOADTARGET_TEST_CHILDPID"
	helperCounterEnv  = "LOADTARGET_TEST_COUNTER"
	// helperLifetime bounds a helper, so a test that fails to clean one up
	// leaves nothing running for long.
	helperLifetime = 2 * time.Minute
)

// runSignalHelper runs the helper modes these tests use; ok is false for a
// mode it does not know.
func runSignalHelper(mode string) (code int, ok bool) {
	switch mode {
	case "parent":
		return helperParent(), true
	case "child":
		time.Sleep(helperLifetime)
		return 0, true
	case "counter":
		return helperCounter(), true
	default:
		return 0, false
	}
}

func helperParent() int {
	self, err := os.Executable()
	if err != nil {
		return 3
	}
	cmd := exec.CommandContext(context.Background(), self)
	cmd.Env = append(os.Environ(), helperEnv+"=child")
	if err := cmd.Start(); err != nil {
		return 3
	}
	if err := os.WriteFile(os.Getenv(helperChildPIDEnv), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		return 3
	}
	time.Sleep(helperLifetime)
	return 0
}

func helperCounter() int {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	path := os.Getenv(helperCounterEnv)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	end := time.After(helperLifetime)
	for n := 1; ; n++ {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(strconv.Itoa(n)), 0o600); err != nil {
			return 3
		}
		if err := os.Rename(tmp, path); err != nil {
			return 3
		}
		select {
		case <-sigs:
			return 0
		case <-end:
			return 0
		case <-tick.C:
		}
	}
}

// startHelper starts the test binary as helper mode in its own process group,
// as the target starts sqi-server, and stops it when the test ends.
func startHelper(t *testing.T, mode string, env ...string) *proc {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p, err := startProc(t.Context(), procSpec{
		name:    "helper " + mode,
		bin:     self,
		env:     childEnv(os.Environ(), append([]string{helperEnv + "=" + mode}, env...)...),
		logPath: filepath.Join(dir, "helper.log"),
	}, testLogger(t))
	if err != nil {
		t.Fatalf("start helper %s: %v", mode, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := p.stopCtx(ctx, time.Second); err != nil {
			t.Errorf("stop helper %s: %v", mode, err)
		}
	})
	return p
}

// readPID waits for a helper to write a pid to path.
func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no pid in %s after 30s", path)
	return 0
}

func TestSignals_KillOneLeavesSiblings(t *testing.T) {
	childFile := filepath.Join(t.TempDir(), "child.pid")
	p := startHelper(t, "parent", helperChildPIDEnv+"="+childFile)
	child := readPID(t, childFile)
	t.Cleanup(func() {
		if c, err := os.FindProcess(child); err == nil {
			c.Kill()    //nolint:errcheck // best-effort cleanup of the orphaned child
			c.Release() //nolint:errcheck // best-effort cleanup
		}
	})
	if !proctest.Alive(child) {
		t.Fatalf("child %d is not running before the kill", child)
	}

	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the killed parent was not reaped within 10s")
	}
	time.Sleep(time.Second)
	if !proctest.Alive(child) {
		t.Errorf("child %d died with its parent: killOne reached more than the one process", child)
	}
}

// TestManaged_CloseEndsWhatAKilledWorkerLeft gives a real worker slot a
// process that starts a child and kills that process: the child survives the
// kill and StopForChecks, for the checks to find, and Close ends it.
func TestManaged_CloseEndsWhatAKilledWorkerLeft(t *testing.T) {
	m := newTestManaged(t, laneScenario(false))
	m.addWorkerSlots()
	childFile := filepath.Join(t.TempDir(), "child.pid")
	p := startHelper(t, "parent", helperChildPIDEnv+"="+childFile)
	child := readPID(t, childFile)
	t.Cleanup(func() {
		if c, err := os.FindProcess(child); err == nil {
			c.Kill()    //nolint:errcheck // best-effort cleanup if the assertion below fails
			c.Release() //nolint:errcheck // best-effort cleanup
		}
	})
	m.mu.Lock()
	m.workers[0].proc, m.workers[0].started, m.workers[0].inc = p, 1, 0
	m.incarnations = []Incarnation{{ID: incarnationID(0, 0), Started: time.Now()}}
	m.mu.Unlock()

	if err := m.KillWorker(t.Context(), 0); err != nil {
		t.Fatalf("KillWorker: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if !proctest.Alive(child) {
		t.Fatalf("child %d died with the killed worker; KillWorker must leave it for the checks", child)
	}
	if err := m.StopForChecks(t.Context()); err != nil {
		t.Fatalf("StopForChecks: %v", err)
	}
	if !proctest.Alive(child) {
		t.Fatalf("child %d died in StopForChecks; only Close may end it", child)
	}
	if err := m.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for proctest.Alive(child) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d of the killed worker still running 5s after Close", child)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// readCounter returns the helper's count, waiting out a missing file.
func readCounter(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				return n
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no count in %s after 10s", path)
	return 0
}

// assertFrozen checks the counter does not move for 500ms.
func assertFrozen(t *testing.T, path string) {
	t.Helper()
	time.Sleep(150 * time.Millisecond) // a write in flight lands
	before := readCounter(t, path)
	time.Sleep(500 * time.Millisecond)
	if after := readCounter(t, path); after != before {
		t.Errorf("counter moved from %d to %d while stopped", before, after)
	}
}

// assertAdvancing checks the counter moves within 5s.
func assertAdvancing(t *testing.T, path string) {
	t.Helper()
	before := readCounter(t, path)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if readCounter(t, path) > before {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("counter stuck at %d after continuing", before)
}

func TestSignals_StallAndResume(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP is POSIX only")
	}
	tests := []struct {
		name       string
		stop, cont func(int) error
	}{
		{"process", stopProcess, contProcess},
		{"group", stopGroup, contGroup},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counter := filepath.Join(t.TempDir(), "count")
			p := startHelper(t, "counter", helperCounterEnv+"="+counter)
			assertAdvancing(t, counter)
			// The helper is its own process group's leader, so its pid is the
			// group's id.
			if err := tt.stop(p.pid()); err != nil {
				t.Fatalf("stop: %v", err)
			}
			assertFrozen(t, counter)
			if err := tt.cont(p.pid()); err != nil {
				t.Fatalf("continue: %v", err)
			}
			assertAdvancing(t, counter)
		})
	}
}

func TestSignals_StallUnsupportedOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows only")
	}
	for name, f := range map[string]func(int) error{
		"stopProcess": stopProcess, "stopGroup": stopGroup, "contProcess": contProcess, "contGroup": contGroup,
	} {
		if err := f(os.Getpid()); !errors.Is(err, errStallUnsupported) {
			t.Errorf("%s = %v, want errStallUnsupported", name, err)
		}
	}
}

// TestProc_StopCtxResumesAStalledProcess checks a stopped process is
// continued before it is interrupted, so stopping it does not wait out the
// grace and end in a kill.
func TestProc_StopCtxResumesAStalledProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP is POSIX only")
	}
	counter := filepath.Join(t.TempDir(), "count")
	p := startHelper(t, "counter", helperCounterEnv+"="+counter)
	assertAdvancing(t, counter)
	if err := p.stall(); err != nil {
		t.Fatalf("stall: %v", err)
	}
	assertFrozen(t, counter)
	start := time.Now()
	if err := p.stopCtx(t.Context(), 30*time.Second); err != nil {
		t.Fatalf("stopCtx: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("stopCtx took %s on a stalled process; it should resume it and stop at once", elapsed)
	}
	if p.waitErr != nil {
		t.Errorf("helper exit = %v, want a clean exit on the interrupt (not a kill)", p.waitErr)
	}
}

func TestProc_KillOneReapsAStalledProcess(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "count")
	p := startHelper(t, "counter", helperCounterEnv+"="+counter)
	assertAdvancing(t, counter)
	if runtime.GOOS != "windows" {
		if err := p.stall(); err != nil {
			t.Fatalf("stall: %v", err)
		}
	}
	if err := p.killOne(t.Context()); err != nil {
		t.Fatalf("killOne: %v", err)
	}
	if !p.exited() || p.isStalled() {
		t.Errorf("after killOne: exited %v, stalled %v; want exited and not stalled", p.exited(), p.isStalled())
	}
}

// laneScenario is a scenario with two worker groups and, when network is
// set, a partition fault, so the target has lanes.
func laneScenario(network bool) *scenario.Scenario {
	sc := testScenario(scenario.ServerSpec{Store: "sqlite"})
	sc.Workers = []scenario.WorkerGroup{
		{Name: "render", Real: 2, Count: 10},
		{Name: "comp", Count: 5},
		{Name: "edit", Real: 1},
	}
	if network {
		sc.Faults = []scenario.Fault{{
			At: scenario.Duration{Duration: time.Second}, Duration: scenario.Duration{Duration: time.Second},
			Partition: &scenario.NetTarget{Groups: []string{"render"}},
		}}
	}
	return sc
}

func newTestManaged(t *testing.T, sc *scenario.Scenario) *managed {
	t.Helper()
	m, err := newManaged(ManagedConfig{RunDir: t.TempDir(), Scenario: sc})
	if err != nil {
		t.Fatalf("newManaged: %v", err)
	}
	m.settings.NATSAddr = "127.0.0.1:1"
	m.natsURL = "nats://" + m.settings.NATSAddr
	return m
}

func TestManaged_LaneIsUntypedNilWithoutANetworkFault(t *testing.T) {
	m := newTestManaged(t, laneScenario(false))
	if err := m.startLanes(t.Context()); err != nil {
		t.Fatalf("startLanes: %v", err)
	}
	var f Faults = m
	if l := f.Lane("render"); l != nil {
		t.Errorf("Lane(render) = %#v without a network fault, want an untyped nil", l)
	}
	if got := m.NATSURLFor("render"); got != m.natsURL {
		t.Errorf("NATSURLFor(render) = %q, want the broker's %q", got, m.natsURL)
	}
}

func TestManaged_LanesPerGroup(t *testing.T) {
	m := newTestManaged(t, laneScenario(true))
	if err := m.startLanes(t.Context()); err != nil {
		t.Fatalf("startLanes: %v", err)
	}
	t.Cleanup(func() { m.closeLanes() }) //nolint:errcheck // best-effort cleanup of loopback listeners
	addrs := map[string]bool{}
	for _, g := range []string{"render", "comp", "edit"} {
		l := m.Lane(g)
		p, ok := l.(*netfault.Proxy)
		if !ok || p == nil {
			t.Fatalf("Lane(%s) = %#v, want a *netfault.Proxy", g, l)
		}
		if got, want := m.NATSURLFor(g), "nats://"+p.Addr(); got != want {
			t.Errorf("NATSURLFor(%s) = %q, want %q", g, got, want)
		}
		addrs[p.Addr()] = true
	}
	if len(addrs) != 3 {
		t.Errorf("lane addresses %v, want three distinct", addrs)
	}
	if l := m.Lane("nope"); l != nil {
		t.Errorf("Lane(nope) = %#v, want nil", l)
	}
	if got := m.NATSURLFor("nope"); got != m.natsURL {
		t.Errorf("NATSURLFor(nope) = %q, want the broker's", got)
	}
}

func TestLaneSeed(t *testing.T) {
	if got := laneSeed(0, 0); got != 1 {
		t.Errorf("laneSeed(0, 0) = %d, want 1", got)
	}
	if got := laneSeed(0, 4); got != 5 {
		t.Errorf("laneSeed(0, 4) = %d, want 5", got)
	}
	if laneSeed(7, 0) == laneSeed(7, 1) || laneSeed(7, 0) == laneSeed(8, 0) || laneSeed(7, 0) == 1 {
		t.Error("laneSeed does not mix the run seed with the lane index")
	}
}

func TestManaged_WorkerEnv(t *testing.T) {
	sc := laneScenario(true)
	sc.Jobs = []scenario.JobClass{{Name: "slow", RealTask: &scenario.RealTask{Duration: scenario.Duration{Duration: time.Second}}}}
	m := newTestManaged(t, sc)
	if err := m.startLanes(t.Context()); err != nil {
		t.Fatalf("startLanes: %v", err)
	}
	t.Cleanup(func() { m.closeLanes() }) //nolint:errcheck // best-effort cleanup of loopback listeners
	if m.MarkerDir() == "" {
		t.Fatal("MarkerDir is empty though a class has real_task")
	}
	env, err := m.workerEnv(sc.Workers[0], 1, "0.1")
	if err != nil {
		t.Fatalf("workerEnv: %v", err)
	}
	for _, want := range []string{
		"SQI_WORKER_NATS_URL=" + m.NATSURLFor("render"),
		"SQILOAD_INCARNATION=0.1",
		"SQILOAD_MARKER_DIR=" + m.MarkerDir(),
	} {
		if !slices.Contains(env, want) {
			t.Errorf("worker env lacks %q", want)
		}
	}

	plain := newTestManaged(t, laneScenario(false))
	if plain.MarkerDir() != "" {
		t.Errorf("MarkerDir = %q without real_task, want empty", plain.MarkerDir())
	}
	env, err = plain.workerEnv(sc.Workers[0], 1, "0.0")
	if err != nil {
		t.Fatalf("workerEnv: %v", err)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "SQILOAD_MARKER_DIR=") {
			t.Errorf("worker env has %q without a marker directory", kv)
		}
	}
	if !slices.Contains(env, "SQI_WORKER_NATS_URL="+plain.natsURL) {
		t.Errorf("worker env without lanes does not dial the broker %s", plain.natsURL)
	}
}

func TestManaged_RealWorkers(t *testing.T) {
	m := newTestManaged(t, laneScenario(false))
	m.addWorkerSlots()
	tests := []struct {
		group string
		want  []int
	}{
		{"render", []int{0, 1}},
		{"edit", []int{2}},
		{"comp", nil},
		{"nope", nil},
	}
	for _, tt := range tests {
		if got := m.RealWorkers(tt.group); !slices.Equal(got, tt.want) {
			t.Errorf("RealWorkers(%s) = %v, want %v", tt.group, got, tt.want)
		}
	}
	if n := m.RealLive(); n != 0 {
		t.Errorf("RealLive = %d before any worker started", n)
	}
	for _, i := range []int{-1, 3} {
		if err := m.KillWorker(t.Context(), i); err == nil {
			t.Errorf("KillWorker(%d) succeeded on a target with 3 real workers", i)
		}
		if err := m.RestartWorker(t.Context(), i); err == nil {
			t.Errorf("RestartWorker(%d) succeeded on a target with 3 real workers", i)
		}
	}
}

func TestManaged_StoreOptions(t *testing.T) {
	m := newTestManaged(t, laneScenario(false))
	got := m.StoreOptions()
	if got.Driver != backend.DriverSQLite || got.SQLitePath != filepath.Join(m.settings.dataDir(), "sqi.db") || got.AutoMigrate {
		t.Errorf("SQLite StoreOptions = %+v", got)
	}
	m.settings.Spec.Store = "postgres"
	m.pgDSN = "postgres://u@h/sqi_load_x"
	got = m.StoreOptions()
	if got.Driver != backend.DriverPostgres || got.PostgresDSN != m.pgDSN || got.AutoMigrate {
		t.Errorf("PostgreSQL StoreOptions = %+v", got)
	}
}

func TestIncarnationID(t *testing.T) {
	if got := incarnationID(0, 0); got != "0.0" {
		t.Errorf("incarnationID(0, 0) = %q", got)
	}
	if got := incarnationID(3, 12); got != "3.12" {
		t.Errorf("incarnationID(3, 12) = %q", got)
	}
}
