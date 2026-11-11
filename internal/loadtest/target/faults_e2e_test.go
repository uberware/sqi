// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/internal/proctest"
	"github.com/uberware/sqi/internal/loadtest/marker"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// assertServerBack checks the restarted server answers on the same addresses
// and still holds the farm created at start.
func assertServerBack(t *testing.T, m *managed, httpURL, natsURL string) {
	t.Helper()
	if m.HTTPURL() != httpURL || m.NATSURL() != natsURL {
		t.Errorf("after restart HTTPURL %q NATSURL %q, want %q and %q", m.HTTPURL(), m.NATSURL(), httpURL, natsURL)
	}
	c := mustClient(t, m)
	if err := c.Ready(t.Context()); err != nil {
		t.Fatalf("/readyz after restart: %v", err)
	}
	if _, err := c.GetFarm(t.Context(), m.FarmID()); err != nil {
		t.Errorf("GetFarm after restart: %v", err)
	}
}

func TestManaged_RestartServerKeepsAddresses(t *testing.T) {
	skipShort(t)
	server, _, _ := binaries(t)
	m := startManaged(t, ManagedConfig{RunDir: t.TempDir(), ServerBin: server, Scenario: testScenario(scenario.ServerSpec{Store: "sqlite"})})
	httpURL, natsURL := m.HTTPURL(), m.NATSURL()
	pid := m.serverPID()

	if err := m.KillServer(t.Context()); err != nil {
		t.Fatalf("KillServer: %v", err)
	}
	if proctest.Alive(pid) {
		t.Fatalf("server pid %d still running after KillServer", pid)
	}
	if err := m.RestartServer(t.Context()); err != nil {
		t.Fatalf("RestartServer: %v", err)
	}
	if m.serverPID() == pid {
		t.Errorf("RestartServer kept pid %d; want a new process", pid)
	}
	assertServerBack(t, m, httpURL, natsURL)
}

func TestManaged_RestartServerBindFailureIsAnError(t *testing.T) {
	skipShort(t)
	server, _, _ := binaries(t)
	const ready = 20 * time.Second
	m := startManaged(t, ManagedConfig{
		RunDir: t.TempDir(), ServerBin: server, Scenario: testScenario(scenario.ServerSpec{Store: "sqlite"}),
		readyTimeout: ready,
	})
	addr := strings.TrimPrefix(m.HTTPURL(), "http://")
	if err := m.KillServer(t.Context()); err != nil {
		t.Fatalf("KillServer: %v", err)
	}
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("occupy %s: %v", addr, err)
	}
	defer l.Close()

	start := time.Now()
	err = m.RestartServer(t.Context())
	if err == nil {
		t.Fatalf("RestartServer succeeded with %s taken", addr)
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("RestartServer err = %v, want one naming %s", err, addr)
	}
	if elapsed := time.Since(start); elapsed > ready+10*time.Second {
		t.Errorf("RestartServer took %s to fail, ready timeout %s", elapsed, ready)
	}
	if m.HTTPURL() != "http://"+addr {
		t.Errorf("HTTPURL moved to %q; a restart must never pick new ports", m.HTTPURL())
	}
}

func TestManaged_TermServerThenRestart(t *testing.T) {
	skipShort(t)
	server, _, _ := binaries(t)
	runDir := t.TempDir()
	m := startManaged(t, ManagedConfig{RunDir: runDir, ServerBin: server, Scenario: testScenario(scenario.ServerSpec{Store: "sqlite"})})
	httpURL, natsURL := m.HTTPURL(), m.NATSURL()
	pid := m.serverPID()

	if err := m.TermServer(t.Context()); err != nil {
		t.Fatalf("TermServer: %v", err)
	}
	if proctest.Alive(pid) {
		t.Fatalf("server pid %d still running after TermServer", pid)
	}
	assertCleanStop(t, m.serverLog())
	if err := m.RestartServer(t.Context()); err != nil {
		t.Fatalf("RestartServer: %v", err)
	}
	assertServerBack(t, m, httpURL, natsURL)
}

func TestManaged_CloseResumesAStalledProcess(t *testing.T) {
	skipShort(t)
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP is POSIX only")
	}
	server, _, _ := binaries(t)
	m := startManaged(t, ManagedConfig{RunDir: t.TempDir(), ServerBin: server, Scenario: testScenario(scenario.ServerSpec{Store: "sqlite"})})
	pid := m.serverPID()
	if err := m.StallServer(); err != nil {
		t.Fatalf("StallServer: %v", err)
	}
	rctx, rcancel := context.WithTimeout(t.Context(), time.Second)
	if err := mustClient(t, m).Ready(rctx); err == nil {
		t.Error("/readyz answered while the server was stalled")
	}
	rcancel()

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	start := time.Now()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Close took %s after a stall; it should resume the server first", elapsed)
	}
	if proctest.Alive(pid) {
		t.Errorf("server pid %d still running after Close", pid)
	}
	assertCleanStop(t, m.serverLog())
}

// waitWorker polls the farm until it lists exactly one worker whose status is
// want, and returns it.
func waitWorker(t *testing.T, m *managed, c *client.Client, want string, timeout time.Duration) client.Worker {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []client.Worker
	for time.Now().Before(deadline) {
		page, err := c.ListWorkers(t.Context(), m.FarmID(), 100, 0)
		if err == nil {
			last = page.Items
			if len(page.Items) == 1 && page.Items[0].Status == want {
				return page.Items[0]
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("no single %s worker after %s; workers %+v\nworker log:\n%s\nserver log:\n%s",
		want, timeout, last, tailLines(m.workerLog(0), 30), tailLines(m.serverLog(), 30))
	return client.Worker{}
}

// The lane test's timings. heartbeatTimeout is the server's, short so a cut
// worker goes offline quickly; the server's heartbeat sweep runs every
// sweepInterval (scheduler.DefaultConfig, not configurable).
const (
	heartbeatTimeout  = 10 * time.Second
	heartbeatInterval = 2 * time.Second
	sweepInterval     = 15 * time.Second
)

// livenessGrace is how long after its broker connection comes up the server
// marks no worker offline: internal/server's busReconnectGrace, which is not
// configurable.
func livenessGrace(workerTimeout time.Duration) time.Duration {
	redial := time.Duration(float64(protocol.MaxReconnectBackoff) * (1 + protocol.ReconnectJitterFraction))
	return workerTimeout + redial + protocol.DefaultHeartbeatInterval
}

// lastHeartbeat is the server's last stored heartbeat of the farm's one
// worker.
func lastHeartbeat(t *testing.T, c *client.Client, farmID string) time.Time {
	t.Helper()
	page, err := c.ListWorkers(t.Context(), farmID, 100, 0)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("ListWorkers = %+v, %v; want one worker", page.Items, err)
	}
	return page.Items[0].LastHeartbeatAt
}

// waitHeartbeatAfter waits for the server to store a heartbeat later than
// after.
func waitHeartbeatAfter(t *testing.T, m *managed, c *client.Client, after time.Time, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if lastHeartbeat(t, c, m.FarmID()).After(after) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("no heartbeat after %s within %s\nworker log:\n%s", after, timeout, tailLines(m.workerLog(0), 30))
}

// registrations counts the worker log's registrations as worker id.
func registrations(log, id string) int {
	n := 0
	for line := range strings.Lines(log) {
		if strings.Contains(line, "registration: worker registered") && strings.Contains(line, "worker_id="+id) {
			n++
		}
	}
	return n
}

// slowTestsEnv opts in to the tests that wait out the server's liveness grace.
const slowTestsEnv = "SQI_LOAD_SLOW_TESTS"

// startLaneTarget starts a managed target with one real worker in group
// render behind a lane, and returns it once the worker is online and
// heartbeating, with that worker and when the server's liveness grace ends.
func startLaneTarget(t *testing.T) (m *managed, c *client.Client, lane Lane, w client.Worker, graceEnds time.Time) {
	t.Helper()
	server, worker, noop := binaries(t)
	sc := testScenario(scenario.ServerSpec{
		Store:  "sqlite",
		Config: map[string]any{"scheduler": map[string]any{"heartbeat_timeout": heartbeatTimeout.String()}},
	})
	sc.Workers = []scenario.WorkerGroup{{
		Name: "render", Real: 1, Queues: []string{"render"}, Heartbeat: scenario.Duration{Duration: heartbeatInterval},
	}}
	sc.Faults = []scenario.Fault{{
		At: scenario.Duration{Duration: time.Second}, Duration: scenario.Duration{Duration: time.Second},
		Partition: &scenario.NetTarget{Groups: []string{"render"}},
	}}
	m = startManaged(t, ManagedConfig{RunDir: t.TempDir(), ServerBin: server, WorkerBin: worker, NoopBin: noop, Scenario: sc})
	graceEnds = time.Now().Add(livenessGrace(heartbeatTimeout))
	c = mustClient(t, m)

	lane = m.Lane("render")
	if lane == nil {
		t.Fatal("Lane(render) is nil on a scenario with a partition fault")
	}
	if m.NATSURLFor("render") == m.NATSURL() {
		t.Fatalf("NATSURLFor(render) = the broker's own %s; the worker does not go through its lane", m.NATSURL())
	}
	w = waitWorker(t, m, c, "online", 30*time.Second)
	waitHeartbeatAfter(t, m, c, lastHeartbeat(t, c, m.FarmID()), 5*heartbeatInterval)
	return m, c, lane, w, graceEnds
}

// TestManaged_LanesFrontTheBroker cuts and heals the real worker's lane: its
// heartbeats stop reaching the server and resume.
func TestManaged_LanesFrontTheBroker(t *testing.T) {
	skipShort(t)
	m, c, lane, _, _ := startLaneTarget(t)

	// Heartbeats stop reaching the server while the lane is cut.
	lane.Cut()
	time.Sleep(heartbeatInterval) // one in flight lands
	frozen := lastHeartbeat(t, c, m.FarmID())
	time.Sleep(3 * heartbeatInterval)
	if got := lastHeartbeat(t, c, m.FarmID()); !got.Equal(frozen) {
		t.Fatalf("the server stored a heartbeat at %s while the lane was cut (last before: %s)", got, frozen)
	}
	lane.Heal()
	waitHeartbeatAfter(t, m, c, frozen, 30*time.Second)
}

// TestManaged_LaneCutTakesTheWorkerOffline waits out the server's post-start
// liveness grace (almost three minutes), then cuts the lane: the server takes
// the worker offline, and the heal brings it back. It runs only with
// SQI_LOAD_SLOW_TESTS=1.
func TestManaged_LaneCutTakesTheWorkerOffline(t *testing.T) {
	skipShort(t)
	if os.Getenv(slowTestsEnv) != "1" {
		t.Skipf("waits about three minutes for the server's liveness grace; set %s=1 to run it", slowTestsEnv)
	}
	m, c, lane, w, graceEnds := startLaneTarget(t)
	if wait := time.Until(graceEnds); wait > 0 {
		t.Logf("waiting %s for the server's liveness grace to pass", wait.Round(time.Second))
		time.Sleep(wait)
	}
	lane.Cut()
	offline := waitWorker(t, m, c, "offline", heartbeatTimeout+sweepInterval+10*time.Second)
	if offline.ID != w.ID {
		t.Errorf("offline worker %s, want %s", offline.ID, w.ID)
	}
	lane.Heal()
	if back := waitWorker(t, m, c, "online", 60*time.Second); back.ID != w.ID {
		t.Errorf("worker back online is %s, want %s", back.ID, w.ID)
	}
}

func TestManaged_RestartWorkerKeepsWorkerID(t *testing.T) {
	skipShort(t)
	server, worker, noop := binaries(t)
	sc := testScenario(scenario.ServerSpec{Store: "sqlite"})
	sc.Workers = []scenario.WorkerGroup{{
		Name: "real", Real: 1, Queues: []string{"render"}, Heartbeat: scenario.Duration{Duration: heartbeatInterval},
	}}
	m := startManaged(t, ManagedConfig{RunDir: t.TempDir(), ServerBin: server, WorkerBin: worker, NoopBin: noop, Scenario: sc})
	c := mustClient(t, m)
	w := waitWorker(t, m, c, "online", 30*time.Second)
	if got := m.RealWorkers("real"); len(got) != 1 || got[0] != 0 {
		t.Fatalf("RealWorkers(real) = %v, want [0]", got)
	}
	pid := m.workerPIDs()[0]

	if err := m.KillWorker(t.Context(), 0); err != nil {
		t.Fatalf("KillWorker: %v", err)
	}
	if proctest.Alive(pid) {
		t.Fatalf("worker pid %d still running after KillWorker", pid)
	}
	if n := m.RealLive(); n != 0 {
		t.Errorf("RealLive = %d after the kill, want 0", n)
	}
	// The killed worker's row stays online through the server's liveness
	// grace, so only a heartbeat stored after this proves the new process.
	killed := time.Now()
	if err := m.RestartWorker(t.Context(), 0); err != nil {
		t.Fatalf("RestartWorker: %v", err)
	}
	if n := m.RealLive(); n != 1 {
		t.Errorf("RealLive = %d after the restart, want 1", n)
	}
	if err := m.RestartWorker(t.Context(), 0); err == nil {
		t.Error("RestartWorker of a running worker succeeded; two processes would share its data directory")
	}
	// The new process heartbeats as the same worker, and no second worker
	// appears.
	assertOnlyWorker := func() {
		t.Helper()
		page, err := c.ListWorkers(t.Context(), m.FarmID(), 100, 0)
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != w.ID || page.Items[0].Status != "online" {
			t.Fatalf("after the restart workers = %+v, %v; want only %s, online", page.Items, err, w.ID)
		}
	}
	waitHeartbeatAfter(t, m, c, killed, 30*time.Second)
	assertOnlyWorker()
	time.Sleep(2 * heartbeatInterval)
	assertOnlyWorker()
	log, err := os.ReadFile(m.workerLog(0))
	if err != nil {
		t.Fatal(err)
	}
	if n := registrations(string(log), w.ID); n != 2 {
		t.Errorf("worker log has %d registrations as %s, want 2 (one per incarnation)\n%s", n, w.ID, tailLines(m.workerLog(0), 30))
	}

	incs := m.Incarnations()
	if len(incs) != 2 {
		t.Fatalf("Incarnations = %+v, want two", incs)
	}
	if incs[0].ID != "0.0" || incs[0].Worker != 0 || incs[0].Ended.IsZero() || incs[0].Started.IsZero() {
		t.Errorf("first incarnation = %+v, want 0.0 of worker 0, ended", incs[0])
	}
	if incs[1].ID != "0.1" || incs[1].Worker != 0 || !incs[1].Ended.IsZero() || incs[1].Started.Before(incs[0].Ended) {
		t.Errorf("second incarnation = %+v, want 0.1 of worker 0, running, started after the first ended", incs[1])
	}

	if err := m.TermWorker(t.Context(), 0); err != nil {
		t.Fatalf("TermWorker: %v", err)
	}
	if n := m.RealLive(); n != 0 {
		t.Errorf("RealLive = %d after TermWorker, want 0", n)
	}
	if incs := m.Incarnations(); incs[1].Ended.IsZero() {
		t.Errorf("incarnation 0.1 has no end after TermWorker: %+v", incs[1])
	}
}

// realTaskJob is a one-task job whose onRun is the no-op with the arguments
// the driver gives a task: the target's own, the task key, then the class.
const realTaskJob = `specificationVersion: jobtemplate-2023-09
name: %s
steps:
  - name: step-1
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "1"
    script:
      actions:
        onRun:
          command: %q
          args: [%s]
`

// startRealTaskTarget starts a managed target with one real worker on queue
// render and one job class, "slow", running rt.
func startRealTaskTarget(t *testing.T, rt scenario.RealTask) (*managed, *client.Client) {
	t.Helper()
	server, worker, noop := binaries(t)
	sc := testScenario(scenario.ServerSpec{Store: "sqlite"})
	sc.Workers = []scenario.WorkerGroup{{Name: "real", Real: 1, Queues: []string{"render"}}}
	sc.Jobs = []scenario.JobClass{{Name: "slow", RealTask: &rt}}
	m := startManaged(t, ManagedConfig{RunDir: t.TempDir(), ServerBin: server, WorkerBin: worker, NoopBin: noop, Scenario: sc})
	return m, mustClient(t, m)
}

// submitRealTask submits a realTaskJob named name of class slow.
func submitRealTask(t *testing.T, m *managed, c *client.Client, name string) string {
	t.Helper()
	cmd := m.NoopCommand()
	args := append(append([]string(nil), cmd.Args...),
		scenario.KeyArg(scenario.TaskKey(name, "step-1", "{{Task.Param.Frame}}", "")), scenario.ClassArg("slow"))
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = strconv.Quote(a)
	}
	tmpl := fmt.Sprintf(realTaskJob, name, cmd.Path, strings.Join(quoted, ", "))
	id, _, err := c.SubmitJob(t.Context(), client.SubmitRequest{FarmID: m.FarmID(), QueueID: m.QueueIDs()["render"], Template: []byte(tmpl)})
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	return id
}

// waitExecRole waits for the markers to show a live exec of role and returns
// it.
func waitExecRole(t *testing.T, m *managed, role string, timeout time.Duration) marker.Exec {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e, ok := execOfRole(t, m, role); ok && time.Since(e.LastSeen) < time.Second {
			return e
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no live %s exec after %s\nworker log:\n%s", role, timeout, tailLines(m.workerLog(0), 30))
	return marker.Exec{}
}

func execOfRole(t *testing.T, m *managed, role string) (marker.Exec, bool) {
	t.Helper()
	execs, _, err := marker.ReadAll(m.MarkerDir())
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	for _, e := range execs {
		if e.Role == role {
			return e, true
		}
	}
	return marker.Exec{}, false
}

func TestManaged_TaskInheritsMarkerEnv(t *testing.T) {
	skipShort(t)
	m, c := startRealTaskTarget(t, scenario.RealTask{Duration: scenario.Duration{Duration: time.Second}})
	const name = "marker-job"
	jobID := submitRealTask(t, m, c, name)
	deadline := time.Now().Add(60 * time.Second)
	var job client.Job
	for time.Now().Before(deadline) {
		var err error
		if job, err = c.GetJob(t.Context(), jobID); err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if job.Status == "completed" || job.Status == "failed" || job.Status == "canceled" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if job.Status != "completed" {
		t.Fatalf("job status = %q, want completed\nworker log:\n%s", job.Status, tailLines(m.workerLog(0), 40))
	}
	execs, skipped, err := marker.ReadAll(m.MarkerDir())
	if err != nil || skipped != 0 || len(execs) != 1 {
		t.Fatalf("ReadAll = %+v, skipped %d, %v; want one exec", execs, skipped, err)
	}
	e := execs[0]
	if e.Role != marker.RoleMain || e.Incarnation != "0.0" || e.Key != scenario.TaskKey(name, "step-1", "1", "") {
		t.Errorf("exec = %+v, want the main process of incarnation 0.0 keyed %s", e, scenario.TaskKey(name, "step-1", "1", ""))
	}
	if e.End == nil || e.ExitCode != 0 || e.End.Sub(e.Start) < time.Second {
		t.Errorf("exec = %+v, want it ended with 0 after its 1s duration", e)
	}
}

// TestManaged_StallWorkerStopsItsTasks stalls a real worker running a task
// with a silent child: the child stops too, and resumes with the worker. A
// second stall is left for Close, which must continue the task's group and
// end it.
func TestManaged_StallWorkerStopsItsTasks(t *testing.T) {
	skipShort(t)
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP is POSIX only")
	}
	m, c := startRealTaskTarget(t, scenario.RealTask{Duration: scenario.Duration{Duration: time.Minute}, SilentChild: true})
	submitRealTask(t, m, c, "stall-job")
	silent := waitExecRole(t, m, marker.RoleSilent, 60*time.Second)

	if err := m.StallWorker(0); err != nil {
		t.Fatalf("StallWorker: %v", err)
	}
	time.Sleep(marker.RefreshInterval) // a refresh in flight lands
	frozen, _ := execOfRole(t, m, marker.RoleSilent)
	time.Sleep(time.Second)
	if now, _ := execOfRole(t, m, marker.RoleSilent); !now.LastSeen.Equal(frozen.LastSeen) {
		t.Fatalf("the silent child's LastSeen advanced from %s to %s while its worker was stalled", frozen.LastSeen, now.LastSeen)
	}
	if err := m.ResumeWorker(0); err != nil {
		t.Fatalf("ResumeWorker: %v", err)
	}
	time.Sleep(time.Second)
	if now, _ := execOfRole(t, m, marker.RoleSilent); !now.LastSeen.After(frozen.LastSeen) {
		t.Fatalf("the silent child's LastSeen stayed at %s after ResumeWorker", frozen.LastSeen)
	}

	if err := m.StallWorker(0); err != nil {
		t.Fatalf("second StallWorker: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	start := time.Now()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The worker's own shutdown grace is 10s; a task group left stopped would
	// hold Close to the harness's 30s stop grace.
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Errorf("Close took %s after a stall", elapsed)
	}
	main, _ := execOfRole(t, m, marker.RoleMain)
	for _, pid := range []int{silent.PID, main.PID} {
		if proctest.Alive(pid) {
			t.Errorf("task process %d still running after Close", pid)
		}
	}
}

// TestManaged_CloseEndsAStalledThenKilledWorkersTasks stalls a real worker
// running a task with a silent child, kills the worker and closes the target
// with no StopForChecks first, as a run's error path does: the task's group,
// continued by Close with a marker gone stale during the stall, must still be
// ended.
func TestManaged_CloseEndsAStalledThenKilledWorkersTasks(t *testing.T) {
	skipShort(t)
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP is POSIX only")
	}
	m, c := startRealTaskTarget(t, scenario.RealTask{Duration: scenario.Duration{Duration: time.Minute}, SilentChild: true})
	submitRealTask(t, m, c, "stall-kill-job")
	silent := waitExecRole(t, m, marker.RoleSilent, 60*time.Second)
	main, _ := execOfRole(t, m, marker.RoleMain)
	t.Cleanup(func() {
		killGroup(silent.PGID) //nolint:errcheck // a leak guard; ESRCH when Close already ended it
	})

	if err := m.StallWorker(0); err != nil {
		t.Fatalf("StallWorker: %v", err)
	}
	if err := m.KillWorker(t.Context(), 0); err != nil {
		t.Fatalf("KillWorker: %v", err)
	}
	// Long enough for the stopped task's marker to go stale.
	time.Sleep(taskStaleAfter + time.Second)
	if !proctest.Alive(silent.PID) {
		t.Fatalf("the silent child %d died with its killed worker; it should survive until Close", silent.PID)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (proctest.Alive(silent.PID) || proctest.Alive(main.PID)) {
		time.Sleep(100 * time.Millisecond)
	}
	for _, pid := range []int{silent.PID, main.PID} {
		if proctest.Alive(pid) {
			t.Errorf("task process %d still running after Close", pid)
		}
	}
}
