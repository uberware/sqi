// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// capacity_act_test.go — capacity groups whose provider acts, end to end
// against the real binaries: a wake_on_lan group putting its idle member to
// sleep and waking it with a magic packet when work arrives, and a sized group
// whose script provider starts real workers when work queues and stops them
// once they are idle.
//
// This test binary stands in for the programs the product runs (see
// runStandIn in worker_binary_test.go): the worker's sleep command, the
// provider script, and the tasks, which wait for a file the test creates.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/capacity/script"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/server"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/natsclient"
)

const (
	// actTimeoutSeconds is every group's idle_timeout and boot_timeout: the
	// minimum the API accepts.
	actTimeoutSeconds = 60
	// actIdleWait bounds a wait for a member to be retired: the idle timeout,
	// then up to a sweep interval, the provider call and the store writes.
	actIdleWait = actTimeoutSeconds*time.Second + 90*time.Second
	// actStopWait bounds a worker's graceful shutdown: its five-second drain
	// grace, the deregistration and the broker drain.
	actStopWait = 20 * time.Second
)

// ── Wire types ───────────────────────────────────────────────────────────────

// actActionWire mirrors internal/api's capacityActionResponse.
type actActionWire struct {
	Action  string    `json:"action"`
	Result  string    `json:"result"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// actStatusWire is the subset of internal/api's capacityStatusResponse these
// tests read.
type actStatusWire struct {
	DemandCores       int            `json:"demand_cores"`
	DesiredMachines   int            `json:"desired_machines"`
	OnlineMembers     int            `json:"online_members"`
	OfflineMembers    int            `json:"offline_members"`
	Waking            int            `json:"waking"`
	Retiring          int            `json:"retiring"`
	Asleep            int            `json:"asleep"`
	WakeFailed        int            `json:"wake_failed"`
	RequestedMachines *int           `json:"requested_machines"`
	LastAction        *actActionWire `json:"last_action"`
	EvaluatedAt       time.Time      `json:"evaluated_at"`
}

// actGroupWire is a capacity group with its status.
type actGroupWire struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Status *actStatusWire `json:"status"`
}

// actWorkerWire is the subset of internal/api's workerResponse these tests
// read.
type actWorkerWire struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	PowerState  string `json:"power_state"`
	PowerReason string `json:"power_reason"`
	Interfaces  []struct {
		Name  string   `json:"name"`
		MAC   string   `json:"mac"`
		Addrs []string `json:"addrs"`
	} `json:"interfaces"`
}

// describeStatus renders s for a failure message.
func describeStatus(s *actStatusWire) string {
	if s == nil {
		return "no status yet"
	}
	requested := "null"
	if s.RequestedMachines != nil {
		requested = strconv.Itoa(*s.RequestedMachines)
	}
	last := "none"
	if a := s.LastAction; a != nil {
		last = fmt.Sprintf("%s %s (%q at %s)", a.Action, a.Result, a.Message, a.At.Format(time.RFC3339))
	}
	return fmt.Sprintf("demand %d, desired %d, online %d, offline %d, waking %d, retiring %d, asleep %d, wake_failed %d, "+
		"requested %s, last action %s, evaluated %s",
		s.DemandCores, s.DesiredMachines, s.OnlineMembers, s.OfflineMembers, s.Waking, s.Retiring, s.Asleep, s.WakeFailed,
		requested, last, s.EvaluatedAt.Format(time.RFC3339Nano))
}

// describeWorker renders w for a failure message.
func describeWorker(w actWorkerWire) string {
	return fmt.Sprintf("%s %s, power %s (%q)", w.ID, w.Status, w.PowerState, w.PowerReason)
}

// ── REST helpers ─────────────────────────────────────────────────────────────

// getActJSON GETs path and decodes a 200 response into dst. It returns the
// HTTP status.
func getActJSON(t *testing.T, ts *testServer, path string, dst any) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, apiURL(ts, path), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := clientFor(ts).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatalf("GET %s: decode: %v", path, err)
		}
	}
	return resp.StatusCode
}

// actGroup reads capacity group id with its status.
func actGroup(t *testing.T, ts *testServer, id string) actGroupWire {
	t.Helper()
	var g actGroupWire
	if code := getActJSON(t, ts, "/api/v1/capacity-groups/"+id, &g); code != http.StatusOK {
		t.Fatalf("GET /capacity-groups/%s: HTTP %d", id, code)
	}
	return g
}

// actWorker reads worker id. A worker that is gone reads as zero with 404.
func actWorker(t *testing.T, ts *testServer, id string) (actWorkerWire, int) {
	t.Helper()
	var w actWorkerWire
	code := getActJSON(t, ts, "/api/v1/workers/"+id, &w)
	return w, code
}

// submitActJob submits yamlBody to queueID in farmID through ts's client and
// returns the job's ID. With authentication on the job's owner is the session's
// user; off, it is "capacity-test".
func submitActJob(t *testing.T, ts *testServer, farmID, queueID, yamlBody string, authOn bool) string {
	t.Helper()
	path := "/api/v1/jobs?farm_id=" + farmID + "&queue_id=" + queueID
	if !authOn {
		path += "&owner=capacity-test"
	}
	var resp jobResp
	mustDoJSONClient(t, clientFor(ts), http.MethodPost, apiURL(ts, path), []byte(yamlBody), "application/x-yaml",
		http.StatusCreated, &resp)
	if resp.ID == "" {
		t.Fatal("POST /jobs returned no id")
	}
	return resp.ID
}

// actJobYAML is a job of tasks tasks, each running this test binary as a gate
// stand-in: it exits 0 once gate exists. With special set, each task needs a
// worker tagged special=yes. No task declares cores, so each takes a whole
// machine.
func actJobYAML(name string, tasks int, testBin, gate string, special bool) string {
	requirements := ""
	if special {
		requirements = `    hostRequirements:
      attributes:
        - name: attr.worker.tag.special
          anyOf: ["yes"]
`
	}
	return fmt.Sprintf(`specificationVersion: "jobtemplate-2023-09"
name: %s
steps:
  - name: Work
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "1-%d"
%s    script:
      actions:
        onRun:
          command: %q
          args: [%q, %q]
`, name, tasks, requirements, testBin, standInGateArg, gate)
}

// waitUntil calls check every 100 ms until it reports done, and fails the test
// after timeout naming what it waited for and the last state check described.
func waitUntil(t *testing.T, timeout time.Duration, what string, check func() (done bool, state string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		done, state := check()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s; last observed: %s", what, timeout, state)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// testBinary returns this test binary's absolute path, which the stand-ins
// run.
func testBinary(t *testing.T) string {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	return bin
}

// ── Worker processes ─────────────────────────────────────────────────────────

// actWorkerProc is a real sqi-worker process. One goroutine owns its Wait.
type actWorkerProc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

// loopbackPort returns a TCP port free on loopback. It needs no test, so a
// provider's handler goroutine can call it.
func loopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // a probe released at once
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected address type %T", ln.Addr())
	}
	return addr.Port, nil
}

// launchActWorker starts bin, a sqi-worker, against ts for queueID in farmID
// with dataDir, global args before "start" and env after the standard
// environment. The caller stops it.
func launchActWorker(bin string, ts *testServer, farmID, queueID, dataDir string, args, env []string) (*actWorkerProc, error) {
	port, err := loopbackPort()
	if err != nil {
		return nil, fmt.Errorf("pick a metrics port: %w", err)
	}
	cmd := exec.Command(bin, append(slices.Clone(args), "start")...) //nolint:noctx // stopped by stop or kill, not a context
	cmd.Env = append(realWorkerEnv(ts, farmID, queueID, dataDir, port), env...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	setStoppableAttrs(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start sqi-worker: %w", err)
	}
	w := &actWorkerProc{cmd: cmd, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		_ = cmd.Wait() //nolint:errcheck // the exit status of a worker the test stops is not asserted
	}()
	return w, nil
}

// exited reports whether the process has exited.
func (w *actWorkerProc) exited() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// kill ends the process at once, as a machine that loses power or suspends
// would: it deregisters nothing.
func (w *actWorkerProc) kill() {
	if !w.exited() {
		_ = w.cmd.Process.Kill() //nolint:errcheck // it may have exited meanwhile; done says so either way
	}
	<-w.done
}

// stop asks the worker to shut down gracefully and waits for it, killing it
// when it cannot be asked or has not exited within actStopWait. It reports
// whether it exited on the request: a worker that had already exited was not
// stopped gracefully.
func (w *actWorkerProc) stop() (graceful bool, err error) {
	if w.exited() {
		return false, nil
	}
	if err := interruptProcess(w.cmd.Process); err != nil {
		w.kill()
		return false, fmt.Errorf("could not ask the worker to stop, killed it: %w", err)
	}
	select {
	case <-w.done:
		return true, nil
	case <-time.After(actStopWait):
		w.kill()
		return false, fmt.Errorf("the worker did not exit within %s of the request, killed it", actStopWait)
	}
}

// ── wake_on_lan: sleep and wake ──────────────────────────────────────────────

// hostHasWakeableInterface reports whether this host has an interface the
// worker reports and the server can address a magic packet with: up, not
// loopback, a 6-byte MAC and an IPv4 address.
func hostHasWakeableInterface() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || len(ifc.HardwareAddr) != 6 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				return true
			}
		}
	}
	return false
}

// wolFarm is a server with a wake_on_lan send_to target the test listens on,
// and the files of the one worker it runs.
type wolFarm struct {
	ts              *testServer
	farmID, queueID string
	// packets receives each UDP datagram sent to the send_to target.
	packets <-chan []byte
	testBin string
	// dataDir is the worker's, kept across restarts so it keeps its ID.
	dataDir, configPath, marker string
}

// newWOLFarm boots the server and writes the worker's configuration: remote
// sleep allowed or not, and this test binary as its sleep command.
func newWOLFarm(t *testing.T, allowRemoteSleep bool) *wolFarm {
	t.Helper()
	pc, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for magic packets: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	packets := make(chan []byte, 64)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			select {
			case packets <- slices.Clone(buf[:n]):
			default:
			}
		}
	}()

	ts := startServerWith(t, func(c *server.Config) {
		c.Scheduler.HeartbeatSweepInterval = capacitySweepInterval
		c.Scheduler.BusReconnectGrace = 2 * time.Second
		// The unschedulable sweep is off by default in a hand-built config.
		c.Scheduler.UnschedulableGrace = time.Second
		c.Capacity.WakeOnLAN.SendTo = []string{pc.LocalAddr().String()}
	})
	farmID, queueID := seedFarmAndQueue(t, ts)

	f := &wolFarm{
		ts: ts, farmID: farmID, queueID: queueID, packets: packets, testBin: testBinary(t),
	}
	dir := t.TempDir()
	f.dataDir = filepath.Join(dir, "data")
	f.marker = filepath.Join(dir, "slept")
	f.configPath = filepath.Join(dir, "worker.json")
	// JSON is YAML, and encoding/json quotes a Windows path correctly.
	cfg, err := json.Marshal(map[string]any{"worker": map[string]any{"power": map[string]any{
		"allow_remote_sleep": allowRemoteSleep,
		"sleep_command":      []string{f.testBin},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.configPath, cfg, 0o600); err != nil {
		t.Fatalf("write the worker configuration: %v", err)
	}
	return f
}

// startWorker starts the farm's worker, stopped when the test ends.
func (f *wolFarm) startWorker(t *testing.T) *actWorkerProc {
	t.Helper()
	w, err := launchActWorker(buildWorkerBinary(t), f.ts, f.farmID, f.queueID, f.dataDir,
		[]string{"--config", f.configPath},
		[]string{standInEnv + "=sleep", standInMarkerEnv + "=" + f.marker})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := w.stop(); err != nil {
			t.Logf("stop worker: %v", err)
		}
	})
	return w
}

// joinWOLGroup creates the named wake_on_lan group "wol", which retires by
// sleep, and adds workerID to it.
func (f *wolFarm) joinWOLGroup(t *testing.T, workerID string) actGroupWire {
	t.Helper()
	g := createCapacityGroup(t, f.ts, map[string]any{
		"farm_id":              f.farmID,
		"name":                 "wol",
		"kind":                 "named",
		"provider":             "wake_on_lan",
		"queue_ids":            []string{f.queueID},
		"min_machines":         0,
		"max_machines":         1,
		"idle_timeout_seconds": actTimeoutSeconds,
		"boot_timeout_seconds": actTimeoutSeconds,
		"retire_action":        "sleep",
	})
	mustDoJSONClient(t, clientFor(f.ts), http.MethodPut,
		apiURL(f.ts, "/api/v1/capacity-groups/"+g.ID+"/members/"+workerID), nil, "", http.StatusOK, nil)
	return actGroup(t, f.ts, g.ID)
}

// recordedMACs returns the MACs the server recorded for w on interfaces with
// an IPv4 address: the ones a wake addresses.
func recordedMACs(w actWorkerWire) []string {
	var macs []string
	for _, ifc := range w.Interfaces {
		hasV4 := slices.ContainsFunc(ifc.Addrs, func(a string) bool {
			ip, _, err := net.ParseCIDR(a)
			return err == nil && ip.To4() != nil
		})
		if mac, err := net.ParseMAC(ifc.MAC); err == nil && len(mac) == 6 && hasV4 {
			macs = append(macs, mac.String())
		}
	}
	return macs
}

// magicPacketMAC returns the MAC pkt wakes, or an error when pkt is not a
// 102-byte magic packet: six 0xFF bytes, then one MAC sixteen times.
func magicPacketMAC(pkt []byte) (string, error) {
	if len(pkt) != 102 {
		return "", fmt.Errorf("%d bytes, want 102", len(pkt))
	}
	if !bytes.Equal(pkt[:6], bytes.Repeat([]byte{0xFF}, 6)) {
		return "", fmt.Errorf("does not start with six 0xFF bytes: % x", pkt[:6])
	}
	mac := pkt[6:12]
	if !bytes.Equal(pkt[6:], bytes.Repeat(mac, 16)) {
		return "", errors.New("the MAC is not repeated sixteen times")
	}
	return net.HardwareAddr(mac).String(), nil
}

// TestCapacityGroup_WakeOnLANSleepAndWake runs a named wake_on_lan group with
// one member, authentication off. With the member's remote sleep allowed, its
// idle timeout sends it to sleep (the sleep command runs and the member is
// asleep and offline), and a job queued for it then sends a magic packet
// carrying its MAC to the group's send_to target; the worker, restarted in the
// machine's place, registers awake and runs the job. With remote sleep not
// allowed, the same idle timeout is refused and the member stays awake and
// online.
func TestCapacityGroup_WakeOnLANSleepAndWake(t *testing.T) {
	t.Parallel()
	if !hostHasWakeableInterface() {
		t.Skip("this host has no up, non-loopback interface with a 6-byte MAC and an IPv4 address to wake")
	}
	buildWorkerBinary(t)
	t.Run("sleeps-and-wakes", func(t *testing.T) {
		t.Parallel()
		testWOLSleepAndWake(t)
	})
	t.Run("refused-without-opt-in", func(t *testing.T) {
		t.Parallel()
		testWOLRefused(t)
	})
}

func testWOLSleepAndWake(t *testing.T) {
	f := newWOLFarm(t, true)
	worker := f.startWorker(t)
	workerID := pollForOnlineWorker(t, f.ts, f.farmID, 60*time.Second)
	group := f.joinWOLGroup(t, workerID)
	awake, _ := actWorker(t, f.ts, workerID)
	macs := recordedMACs(awake)
	if len(macs) == 0 {
		t.Fatalf("worker %s recorded no MAC on an interface with an IPv4 address: %+v", workerID, awake.Interfaces)
	}
	joined := time.Now()

	// ── Idle past idle_timeout: retiring, then asleep and offline ────────────
	sawRetiring := false
	waitUntil(t, actIdleWait, "the idle member retired to asleep and offline, with the sleep command run", func() (bool, string) {
		w, _ := actWorker(t, f.ts, workerID)
		g := actGroup(t, f.ts, group.ID)
		if w.PowerState == "retiring" || (g.Status != nil && g.Status.Retiring > 0) {
			sawRetiring = true
		}
		_, markerErr := os.Stat(f.marker)
		done := w.PowerState == "asleep" && w.Status == "offline" && markerErr == nil &&
			g.Status != nil && g.Status.Asleep == 1 &&
			g.Status.LastAction != nil && g.Status.LastAction.Action == "retire" && g.Status.LastAction.Result == "ok"
		return done, fmt.Sprintf("worker %s; sleep marker present %v; group: %s",
			describeWorker(w), markerErr == nil, describeStatus(g.Status))
	})
	t.Logf("member asleep %s after joining the group (retiring observed by polling: %v)",
		time.Since(joined).Round(time.Second), sawRetiring)

	// The machine suspends: the process ends without a word to the server.
	worker.kill()

	// ── Work arrives: a magic packet for the member ──────────────────────────
	gate := filepath.Join(t.TempDir(), "release")
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Only a packet the job causes counts.
	for drained := false; !drained; {
		select {
		case <-f.packets:
		default:
			drained = true
		}
	}
	submitted := time.Now()
	jobID := submitActJob(t, f.ts, f.farmID, f.queueID, actJobYAML("Wake On LAN Test", 1, f.testBin, gate, false), false)
	deadline := time.After(30 * time.Second)
	var woken string
	for woken == "" {
		select {
		case pkt := <-f.packets:
			mac, err := magicPacketMAC(pkt)
			if err != nil {
				t.Fatalf("the send_to target received a datagram that is not a magic packet: %v", err)
			}
			if !slices.Contains(macs, mac) {
				t.Fatalf("magic packet for %s; the member recorded %v", mac, macs)
			}
			woken = mac
		case <-deadline:
			g := actGroup(t, f.ts, group.ID)
			t.Fatalf("no magic packet reached the send_to target within 30s of the job; group: %s", describeStatus(g.Status))
		}
	}
	t.Logf("magic packet for %s received %s after the job was submitted", woken, time.Since(submitted).Round(time.Millisecond))

	// The task names the group it waits for.
	tasks := capacityJobTasks(t, f.ts, jobID)
	if len(tasks) != 1 {
		t.Fatalf("job %s has %d tasks, want 1", jobID, len(tasks))
	}
	taskID := tasks[0].ID
	var reason string
	waitUntil(t, 30*time.Second, "the waiting task's unschedulable reason naming the group", func() (bool, string) {
		task := getTaskDetailAct(t, f.ts, taskID)
		reason = task.UnschedulableReason
		w, _ := actWorker(t, f.ts, workerID)
		return strings.Contains(reason, "capacity group wol"), fmt.Sprintf("task %s %s, reason %q; worker %s",
			task.ID, task.Status, reason, describeWorker(w))
	})
	t.Logf("while it waited, the task's unschedulable reason was %q", reason)
	if w, _ := actWorker(t, f.ts, workerID); w.PowerState != "waking" {
		t.Errorf("member after the wake: %s; want power waking", describeWorker(w))
	}
	waitUntil(t, 15*time.Second, "the wake recorded as the group's last action", func() (bool, string) {
		s := actGroup(t, f.ts, group.ID).Status
		return s != nil && s.LastAction != nil && s.LastAction.Action == "wake" && s.LastAction.Result == "ok" &&
			strings.HasPrefix(s.LastAction.Message, workerID+": "), describeStatus(s)
	})

	// ── The machine wakes: the worker comes back and runs the task ───────────
	f.startWorker(t)
	waitUntil(t, 60*time.Second, "the woken member awake and online", func() (bool, string) {
		w, _ := actWorker(t, f.ts, workerID)
		return w.Status == "online" && w.PowerState == "awake", describeWorker(w)
	})
	waitUntil(t, 60*time.Second, "the task succeeded", func() (bool, string) {
		task := getTaskDetailAct(t, f.ts, taskID)
		return task.Status == "succeeded", fmt.Sprintf("task %s %s, reason %q", task.ID, task.Status, task.UnschedulableReason)
	})
	attempts := taskAttempts(t, f.ts, taskID)
	if len(attempts) != 1 || attempts[0].Status != "succeeded" {
		t.Errorf("task %s attempts: %+v; want one, succeeded", taskID, attempts)
	}
	if task := getTaskDetailAct(t, f.ts, taskID); task.AssignedWorkerID != workerID {
		t.Errorf("task %s ran on %q, want the woken member %s", taskID, task.AssignedWorkerID, workerID)
	}
}

func testWOLRefused(t *testing.T) {
	f := newWOLFarm(t, false)
	worker := f.startWorker(t)
	workerID := pollForOnlineWorker(t, f.ts, f.farmID, 60*time.Second)
	group := f.joinWOLGroup(t, workerID)
	joined := time.Now()

	waitUntil(t, actIdleWait, "a retire of the idle member refused", func() (bool, string) {
		g := actGroup(t, f.ts, group.ID)
		a := g.Status
		return a != nil && a.LastAction != nil && a.LastAction.Action == "retire" && a.LastAction.Result == "refused",
			describeStatus(g.Status)
	})
	g := actGroup(t, f.ts, group.ID)
	t.Logf("retire refused %s after joining the group: %q", time.Since(joined).Round(time.Second), g.Status.LastAction.Message)
	if !strings.Contains(g.Status.LastAction.Message, "allow_remote_sleep") {
		t.Errorf("refusal message %q does not name worker.power.allow_remote_sleep", g.Status.LastAction.Message)
	}

	// The refusal puts the member back to awake; it never left.
	waitUntil(t, 10*time.Second, "the member awake and online after the refusal", func() (bool, string) {
		w, _ := actWorker(t, f.ts, workerID)
		return w.Status == "online" && w.PowerState == "awake", describeWorker(w)
	})
	for range 3 * int(time.Second/capacitySweepInterval) {
		if w, _ := actWorker(t, f.ts, workerID); w.Status != "online" || w.PowerState != "awake" {
			t.Fatalf("after the refusal the member changed: %s", describeWorker(w))
		}
		time.Sleep(capacitySweepInterval)
	}
	if _, err := os.Stat(f.marker); err == nil {
		t.Error("the sleep command ran although remote sleep is not allowed")
	}
	if worker.exited() {
		t.Error("the worker process exited although its retirement was refused")
	}
}

// getTaskDetailAct reads task id.
func getTaskDetailAct(t *testing.T, ts *testServer, id string) capacityTask {
	t.Helper()
	var task capacityTask
	if code := getActJSON(t, ts, "/api/v1/tasks/"+id, &task); code != http.StatusOK {
		t.Fatalf("GET /tasks/%s: HTTP %d", id, code)
	}
	return task
}

// ── script: grow and retire ──────────────────────────────────────────────────

// scriptSweepInterval is the script test's sweep interval. A member's idle time
// starts at the first sweep that sees it idle, so two members that finish
// their work within one interval of each other are retired by one request;
// the test releases the work just after a sweep, leaving the whole interval
// for it to finish. The interval is several times what the work takes, so a
// slow or loaded machine still finishes within it.
const scriptSweepInterval = 10 * time.Second

// scriptProvider is the HTTP end of the provider stand-in. It records each
// request and carries it out with real sqi-worker processes: a grow starts
// workers enrolled with the group's token, a retire stops the named ones
// gracefully.
type scriptProvider struct {
	srv *httptest.Server

	// Set before the first request, read-only after.
	bin, workerBase string
	ts              *testServer
	farmID, queueID string
	groupToken      string

	mu       sync.Mutex
	requests []script.Request
	errs     []error
	// workers are the processes started, and dataDirs their data
	// directories, by worker ID.
	workers  map[string]*actWorkerProc
	dataDirs map[string]string
	// started are the worker IDs grows started, in order.
	started []string
	// stoppedGracefully is, by worker ID, whether a retire's stop was
	// graceful.
	stoppedGracefully map[string]bool
}

func newScriptProvider(t *testing.T) *scriptProvider {
	t.Helper()
	p := &scriptProvider{
		workers: map[string]*actWorkerProc{}, dataDirs: map[string]string{}, stoppedGracefully: map[string]bool{},
	}
	p.srv = httptest.NewServer(p)
	t.Cleanup(p.srv.Close)
	return p
}

// instanceIDPath is the GCP metadata path a worker reads its instance name
// from, under the endpoint the provider gives each worker it starts.
const instanceIDPath = "/computeMetadata/v1/instance/name"

func (p *scriptProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A worker this provider started reads its cloud instance ID here: a
	// sized script group retires only members that report one.
	if id, ok := strings.CutPrefix(r.URL.Path, "/metadata/"); ok && r.Method == http.MethodGet {
		workerID, found := strings.CutSuffix(id, instanceIDPath)
		if !found || workerID == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "inst-"+workerID) //nolint:errcheck // a failed reply fails the worker's read, which the test sees
		return
	}
	var req script.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		p.fail(fmt.Errorf("decode a provider request: %w", err))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()

	resp := script.Response{Result: "ok"}
	switch req.Action {
	case "grow":
		if err := p.grow(req.Desired - req.Current); err != nil {
			p.fail(err)
			resp = script.Response{Result: "error", Message: err.Error()}
		}
	case "retire":
		if err := p.retire(req.Members); err != nil {
			p.fail(err)
			resp = script.Response{Result: "error", Message: err.Error()}
		}
	default:
		resp = script.Response{Result: "unsupported"}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp) //nolint:errcheck // a failed reply fails the run, which the test sees
}

func (p *scriptProvider) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errs = append(p.errs, err)
}

// grow starts n workers enrolled with the group token.
func (p *scriptProvider) grow(n int) error {
	for range n {
		dataDir, err := os.MkdirTemp(p.workerBase, "worker-*")
		if err != nil {
			return err
		}
		id, err := workerconfig.LoadOrCreateWorkerID(dataDir)
		if err != nil {
			return err
		}
		w, err := launchActWorker(p.bin, p.ts, p.farmID, p.queueID, dataDir, nil, []string{
			"SQI_WORKER_NATS_SERVER_URL=http://" + p.ts.HTTPAddr,
			"SQI_WORKER_NATS_JOIN_TOKEN=" + p.groupToken,
			"SQI_WORKER_CAPABILITY_TAGS=special=yes",
			"SQI_WORKER_CLOUD_INSTANCE_ID_SOURCE=gcp",
			"SQI_WORKER_CLOUD_INSTANCE_ID_ENDPOINT=" + p.srv.URL + "/metadata/" + id,
		})
		if err != nil {
			return err
		}
		p.mu.Lock()
		p.workers[id], p.dataDirs[id] = w, dataDir
		p.started = append(p.started, id)
		p.mu.Unlock()
	}
	return nil
}

// retire stops each named worker gracefully, concurrently.
func (p *scriptProvider) retire(members []script.Member) error {
	var wg sync.WaitGroup
	errs := make([]error, len(members))
	for i, m := range members {
		p.mu.Lock()
		w := p.workers[m.WorkerID]
		p.mu.Unlock()
		if w == nil {
			errs[i] = fmt.Errorf("retire names %q, which this provider did not start", m.WorkerID)
			continue
		}
		wg.Go(func() {
			graceful, err := w.stop()
			p.mu.Lock()
			p.stoppedGracefully[m.WorkerID] = graceful
			p.mu.Unlock()
			if err != nil {
				errs[i] = fmt.Errorf("stop %s: %w", m.WorkerID, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// stopAll stops every worker still running.
func (p *scriptProvider) stopAll(t *testing.T) {
	p.mu.Lock()
	workers := make([]*actWorkerProc, 0, len(p.workers))
	for _, w := range p.workers {
		workers = append(workers, w)
	}
	p.mu.Unlock()
	for _, w := range workers {
		if _, err := w.stop(); err != nil {
			t.Logf("stop worker: %v", err)
		}
	}
}

// snapshot returns copies of the requests and errors so far.
func (p *scriptProvider) snapshot() (reqs []script.Request, errs []error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests), slices.Clone(p.errs)
}

// requestsFor returns the requests of action.
func requestsFor(reqs []script.Request, action string) []script.Request {
	var out []script.Request
	for _, r := range reqs {
		if r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

// connectWithCredential connects to ts's broker as workerID with the seed in
// dataDir, and closes the connection at once.
func connectWithCredential(ts *testServer, workerID, dataDir string) error {
	seed, err := brokerauth.LoadSeed(workerconfig.DefaultCredentialFile(dataDir))
	if err != nil {
		return fmt.Errorf("load the credential: %w", err)
	}
	pub, err := brokerauth.PublicKeyFromSeed(seed)
	if err != nil {
		return fmt.Errorf("read the credential: %w", err)
	}
	nc, err := nats.Connect(
		"nats://"+ts.NATSAddr,
		nats.Nkey(pub, func(nonce []byte) ([]byte, error) {
			kp, err := nkeys.FromSeed(seed)
			if err != nil {
				return nil, err
			}
			return kp.Sign(nonce)
		}),
		nats.CustomInboxPrefix(brokerauth.InboxPrefix(workerID)),
		nats.NoReconnect(),
		nats.Timeout(5*time.Second),
	)
	if err != nil {
		return err
	}
	nc.Close()
	return nil
}

// TestCapacityGroup_ScriptGrowAndRetire runs a sized group whose provider is a
// script, with REST and broker authentication on. Four tasks queued for the
// group make the server ask the script for two machines; the script starts two
// real workers enrolled with the group's token, which join as members and run
// the tasks. Once both are idle past the idle timeout, the server asks the
// script to retire both in one request; the script stops them, and their rows
// and credentials are gone.
func TestCapacityGroup_ScriptGrowAndRetire(t *testing.T) {
	t.Parallel()
	const (
		cookieName    = "sqi_session"
		adminUser     = "admin"
		adminPassword = "capacity-script-admin-password"
	)
	bin := buildWorkerBinary(t)
	testBin := testBinary(t)
	prov := newScriptProvider(t)

	httpAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	ts := startAuthServer(t, httpAddr, "sqi-capacity-script.db", func(c *server.Config) {
		c.AuthCookieName = cookieName
		c.AuthBootstrapUsername = adminUser
		c.AuthBootstrapPassword = adminPassword
		c.NATS.Auth = config.NATSAuthConfig{
			Enabled:                   true,
			UserTTL:                   time.Hour,
			JoinTokenTTL:              time.Hour,
			JoinTokenSingleUse:        true,
			EnrollmentEndpointEnabled: true,
		}
		c.Scheduler.HeartbeatSweepInterval = scriptSweepInterval
		c.Scheduler.BusReconnectGrace = 2 * time.Second
		c.Capacity.Providers = map[string]config.CapacityProviderConfig{
			"test": {Command: []string{testBin}, Env: map[string]string{capScriptURLEnv: prov.srv.URL}},
		}
	})
	ts.Client = sessionClient(t, ts, cookieName, adminUser, adminPassword)
	farmID, queueID := seedFarmAndQueue(t, ts)

	group := createCapacityGroup(t, ts, map[string]any{
		"farm_id":              farmID,
		"name":                 "scripted",
		"kind":                 "sized",
		"provider":             "script:test",
		"queue_ids":            []string{queueID},
		"min_machines":         0,
		"max_machines":         2,
		"max_step_up":          2,
		"idle_timeout_seconds": actTimeoutSeconds,
		"boot_timeout_seconds": actTimeoutSeconds,
		"retire_action":        "shutdown",
		"machine_profile": map[string]any{
			"cpu_count": 1,
			"tags":      map[string]string{"special": "yes"},
		},
	})
	var minted struct {
		Token string `json:"token"`
	}
	mustDoJSONClient(t, clientFor(ts), http.MethodPost, apiURL(ts, "/api/v1/capacity-groups/"+group.ID+"/tokens"),
		[]byte(`{"name":"capacity-script"}`), "application/json", http.StatusCreated, &minted)

	prov.bin, prov.workerBase, prov.ts = bin, t.TempDir(), ts
	prov.farmID, prov.queueID, prov.groupToken = farmID, queueID, minted.Token
	// Registered after the server's, so it runs first: the workers stop
	// while the broker is still there to hear them.
	t.Cleanup(func() { prov.stopAll(t) })

	// ── Work queues: one grow for two machines, which join and run it ────────
	gate := filepath.Join(t.TempDir(), "release")
	jobID := submitActJob(t, ts, farmID, queueID, actJobYAML("Script Provider Test", 4, testBin, gate, true), true)

	var members []string
	waitUntil(t, 90*time.Second, "two members registered and online", func() (bool, string) {
		members = capacityMemberIDs(t, ts, group.ID)
		online := 0
		for _, id := range members {
			if w, _ := actWorker(t, ts, id); w.Status == "online" {
				online++
			}
		}
		reqs, errs := prov.snapshot()
		return len(members) == 2 && online == 2, fmt.Sprintf("members %v (%d online); provider requests %+v, errors %v; group: %s",
			members, online, reqs, errs, describeStatus(actGroup(t, ts, group.ID).Status))
	})
	prov.mu.Lock()
	started := slices.Clone(prov.started)
	prov.mu.Unlock()
	if !sameIDs(members, started) {
		t.Fatalf("members %v, but the provider started %v", members, started)
	}
	// The credential check after the retirement means something only if the
	// same check is accepted now.
	for _, id := range members {
		prov.mu.Lock()
		dataDir := prov.dataDirs[id]
		prov.mu.Unlock()
		if err := connectWithCredential(ts, id, dataDir); err != nil {
			t.Fatalf("connecting as member %s with its credential before the retirement: %v", id, err)
		}
	}

	// Both members hold a task before any is released.
	waitUntil(t, 60*time.Second, "a task running on each member", func() (bool, string) {
		tasks := capacityJobTasks(t, ts, jobID)
		var on []string
		for _, task := range tasks {
			if task.Status == "running" && !slices.Contains(on, task.AssignedWorkerID) {
				on = append(on, task.AssignedWorkerID)
			}
		}
		return sameIDs(on, members), fmt.Sprintf("tasks %+v", tasks)
	})

	// Release the work just after a sweep, so both members finish inside one
	// sweep interval (see scriptSweepInterval).
	var before time.Time
	if s := actGroup(t, ts, group.ID).Status; s != nil {
		before = s.EvaluatedAt
	}
	waitUntil(t, 2*scriptSweepInterval+5*time.Second, "a sweep to release the work after", func() (bool, string) {
		s := actGroup(t, ts, group.ID).Status
		return s != nil && s.EvaluatedAt.After(before), describeStatus(s)
	})
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatalf("release the tasks: %v", err)
	}
	released := time.Now()
	if final := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, 60*time.Second); final != "completed" {
		t.Fatalf("job %s ended %q, want completed", jobID, final)
	}
	t.Logf("job completed %s after the release", time.Since(released).Round(time.Millisecond))
	for _, task := range capacityJobTasks(t, ts, jobID) {
		attempts := taskAttempts(t, ts, task.ID)
		if task.Status != "succeeded" || task.FailedAttempts != 0 || len(attempts) != 1 || attempts[0].Status != "succeeded" {
			t.Errorf("task %s: %s, failed_attempts %d, attempts %+v; want succeeded at its one attempt",
				task.ID, task.Status, task.FailedAttempts, attempts)
		}
	}

	// ── Idle past idle_timeout: one retire naming both ───────────────────────
	waitUntil(t, actIdleWait, "a retire request from the server", func() (bool, string) {
		reqs, errs := prov.snapshot()
		return len(requestsFor(reqs, "retire")) > 0, fmt.Sprintf("provider requests %+v, errors %v; group: %s",
			reqs, errs, describeStatus(actGroup(t, ts, group.ID).Status))
	})
	t.Logf("retire requested %s after the release", time.Since(released).Round(time.Second))

	waitUntil(t, 30*time.Second, "both members' rows removed", func() (bool, string) {
		var codes []int
		for _, id := range members {
			_, code := actWorker(t, ts, id)
			codes = append(codes, code)
		}
		return codes[0] == http.StatusNotFound && codes[1] == http.StatusNotFound, fmt.Sprintf("GET /workers: %v", codes)
	})
	for _, id := range members {
		prov.mu.Lock()
		dataDir, graceful := prov.dataDirs[id], prov.stoppedGracefully[id]
		prov.mu.Unlock()
		t.Logf("member %s stopped gracefully: %v", id, graceful)
		err := connectWithCredential(ts, id, dataDir)
		if !natsclient.IsAuthRefusal(err) {
			t.Errorf("connecting as retired member %s with its credential: %v; want the broker to refuse it", id, err)
		}
	}
	waitUntil(t, 2*scriptSweepInterval+5*time.Second, "requested_machines 0", func() (bool, string) {
		s := actGroup(t, ts, group.ID).Status
		return s != nil && s.RequestedMachines != nil && *s.RequestedMachines == 0, describeStatus(s)
	})

	// ── What the script was asked ────────────────────────────────────────────
	reqs, errs := prov.snapshot()
	if len(errs) > 0 {
		t.Errorf("the provider stand-in failed: %v", errs)
	}
	grows := requestsFor(reqs, "grow")
	if len(grows) != 1 || grows[0].Desired != 2 || grows[0].Current != 0 {
		t.Errorf("grow requests %+v; want one, desired 2, current 0", grows)
	}
	retires := requestsFor(reqs, "retire")
	var named []string
	if len(retires) == 1 {
		for _, m := range retires[0].Members {
			named = append(named, m.WorkerID)
			if m.InstanceID != "inst-"+m.WorkerID {
				t.Errorf("retire names %s with instance ID %q, want the one it reported, %q", m.WorkerID, m.InstanceID, "inst-"+m.WorkerID)
			}
		}
	}
	if len(retires) != 1 || !sameIDs(named, members) {
		t.Errorf("retire requests %+v; want one naming both members %v", retires, members)
	}
	for _, r := range reqs {
		if r.Group.ID != group.ID || r.Version != script.Version {
			t.Errorf("request %+v: want version %d for group %s", r, script.Version, group.ID)
		}
	}
}

// sameIDs reports whether a and b hold the same IDs, in any order.
func sameIDs(a, b []string) bool {
	return len(a) == len(b) && !slices.ContainsFunc(a, func(id string) bool { return !slices.Contains(b, id) })
}
