// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration && unix

package integration

// ha_test.go: scheduler high availability end to end, run by `make test-ha`
// (scripts/test-ha.sh).
//
// Every server is the real sqi-server binary in ha.enabled mode, on a
// PostgreSQL database of its test's own (empty, so the first server to start
// migrates it) and on the account of the real three-node NATS cluster
// natscluster_test.go describes. Workers are the real sqi-worker. The tests
// signal the servers directly (SIGTERM, SIGKILL, SIGSTOP), cut one server off
// from the broker through TCP proxies of its own, and stop the cluster and
// the PostgreSQL container.
//
// A test-side balancer stands in for the one an operator runs: it sends each
// request to whichever server answers /leaderz 200, as the reference HAProxy
// configuration (deploy/ha/haproxy.cfg) does, and retries a connection that
// could not be made. It also retries a standby's own refusal (503 with
// {"error":"standby"}), which a real client does on Retry-After, so that the
// REST helpers of the rest of the suite, which go through it, need no retry
// loop of their own.
//
// Two watchers run for the life of every group: one probes /leaderz on every
// server at once every 50ms and fails the test if two answer 200 together,
// and one polls the database every 200ms and fails it if any task ever has
// two open attempts. TestHA_NoDoubleAssignment reads what the second recorded
// across the tests before it, and runs a scenario of its own.
//
// Every test skips without SQI_TEST_HA, which only the script sets, and fails
// rather than skips when the script set it but something is missing. CI
// asserts each test's PASS line by name.

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/store/storetest"
)

// haLeaseTTL is ha.lease_ttl's default, which every group here runs with, so
// the bounds below are the documented timings.
const haLeaseTTL = 15 * time.Second

// haFailoverBound is how soon a standby must lead after the leader crashes:
// the lease (15s), a campaign poll (1s) and promotion, with headroom for CI.
const haFailoverBound = 25 * time.Second

// haStopWait is how long a graceful stop may take: the server's own shutdown
// budget (30s) and the hand-over beyond it (2s + 5s), plus slack.
const haStopWait = 45 * time.Second

// haRig is what scripts/test-ha.sh started: the NATS cluster and a
// PostgreSQL container.
type haRig struct {
	*natsCluster

	pgContainer string
}

// requireHARig returns the rig, skipping without SQI_TEST_HA and failing when
// it is set but the rest of the script's environment is not.
func requireHARig(t *testing.T) *haRig {
	t.Helper()
	if os.Getenv("SQI_TEST_HA") == "" {
		t.Skip("SQI_TEST_HA not set; run `make test-ha` for the high-availability tests")
	}
	for _, name := range []string{"SQI_TEST_NATS_URLS", "SQI_TEST_POSTGRES_DSN", "SQI_TEST_HA_POSTGRES_CONTAINER"} {
		if os.Getenv(name) == "" {
			t.Fatalf("SQI_TEST_HA is set but %s is not; run these tests through scripts/test-ha.sh", name)
		}
	}
	return &haRig{natsCluster: requireNATSCluster(t), pgContainer: os.Getenv("SQI_TEST_HA_POSTGRES_CONTAINER")}
}

// docker runs `docker <args>`, failing the test on error.
func (*haRig) docker(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// ── A server process ─────────────────────────────────────────────────────────

// haProc is one run of an sqi-server binary.
type haProc struct {
	cmd     *exec.Cmd
	out     *lockedBuffer
	exited  chan struct{}
	waitErr error

	mu sync.Mutex
	// expected is the exit codes the test has caused this run to end with
	// (-1: killed by a signal). A run that exits while it is empty, or with
	// another code, died on its own, and the group's close fails the test.
	expected []int
}

// expect records that the test is about to end the run, and the exit codes
// that ending it may produce.
func (p *haProc) expect(codes ...int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expected = codes
}

// unexpectedExit describes the run's exit if the test did not cause it, or
// "" when the run is still going or ended as expected.
func (p *haProc) unexpectedExit() string {
	if p.running() {
		return ""
	}
	code := p.exitCode()
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case len(p.expected) == 0:
		return fmt.Sprintf("exited %d without the test stopping it", code)
	case !slices.Contains(p.expected, code):
		return fmt.Sprintf("exited %d, want one of %v", code, p.expected)
	}
	return ""
}

// running reports whether the process has not exited.
func (p *haProc) running() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// waitExit waits up to timeout for the process to exit and returns its exit
// code ([haProc.exitCode]). ok is false when it was still running.
func (p *haProc) waitExit(timeout time.Duration) (code int, ok bool) {
	select {
	case <-p.exited:
		return p.exitCode(), true
	case <-time.After(timeout):
		return 0, false
	}
}

// exitCode is the code the process exited with, -1 when a signal ended it.
// It waits for the process to exit.
func (p *haProc) exitCode() int {
	<-p.exited
	if p.waitErr == nil {
		return 0
	}
	if ee, isExit := errors.AsType[*exec.ExitError](p.waitErr); isExit {
		return ee.ExitCode()
	}
	return -1
}

// signal sends sig to the process, failing the test if it cannot.
func (p *haProc) signal(t *testing.T, sig os.Signal) {
	t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal %v to sqi-server pid %d: %v", sig, p.cmd.Process.Pid, err)
	}
}

// ── A server of the group ────────────────────────────────────────────────────

// haNode is one server of a group across its restarts: a name, an HTTP
// address, a config file, and its own proxies to the broker's nodes.
type haNode struct {
	name       string
	httpAddr   string
	dir        string
	configPath string
	proxies    []*tcpProxy

	mu    sync.Mutex
	procs []*haProc // every run, the current one last
}

// proc is the node's current (latest) process, nil before its first start.
func (n *haNode) proc() *haProc {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.procs) == 0 {
		return nil
	}
	return n.procs[len(n.procs)-1]
}

// running reports whether the node's current process is running.
func (n *haNode) running() bool {
	p := n.proc()
	return p != nil && p.running()
}

// cutBroker closes every connection of the node to the broker and refuses new
// ones (cut true), or lets it connect again (cut false).
func (n *haNode) cutBroker(cut bool) {
	for _, p := range n.proxies {
		p.setCut(cut)
	}
}

// logTail is the end of each of the node's runs' output.
func (n *haNode) logTail() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var b strings.Builder
	for i, p := range n.procs {
		out := withoutRequestLogs(p.out.String())
		const keep = 12 << 10
		if len(out) > keep {
			out = "…" + out[len(out)-keep:]
		}
		fmt.Fprintf(&b, "── %s run %d (pid %d) ──\n%s\n", n.name, i+1, p.cmd.Process.Pid, out)
	}
	return b.String()
}

// withoutRequestLogs drops the access log's lines from a server's output:
// the probes write one every few milliseconds, which would leave nothing else
// in a tail.
func withoutRequestLogs(out string) string {
	lines := strings.SplitAfter(out, "\n")
	return strings.Join(slices.DeleteFunc(lines, func(l string) bool {
		return strings.Contains(l, `msg="http request"`)
	}), "")
}

// ── A group ──────────────────────────────────────────────────────────────────

// haGroup is servers sharing one fresh database, a balancer in front of them,
// and the watchers.
type haGroup struct {
	t     *testing.T
	rig   *haRig
	dsn   string
	db    *sql.DB
	nodes []*haNode
	lb    *haBalancer
	// api is what the suite's REST helpers take: the balancer's address and
	// the group's database.
	api *testServer

	leaders  *leaderWatch
	attempts *attemptWatch

	// logLevel is the servers' --log-level; empty is info.
	logLevel string
}

// newHAGroup makes a group of n servers, named ha-1 … ha-n, on a new empty
// database. None is started.
func newHAGroup(t *testing.T, rig *haRig, n int) *haGroup {
	t.Helper()
	return newHAGroupOn(t, rig, n, storetest.PostgresEmptyDSN(t), "ha")
}

// newHAGroupOn is [newHAGroup] on the database dsn, with servers named
// prefix-1 … prefix-n.
func newHAGroupOn(t *testing.T, rig *haRig, n int, dsn, prefix string) *haGroup {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open the group's database: %v", err)
	}
	g := &haGroup{t: t, rig: rig, dsn: dsn, db: db}
	addrs := make([]string, 0, n)
	for i := range n {
		node := g.newNode(fmt.Sprintf("%s-%d", prefix, i+1))
		g.nodes = append(g.nodes, node)
		addrs = append(addrs, node.httpAddr)
	}
	g.lb = newHABalancer(t, addrs)
	g.api = &testServer{
		HTTPAddr: g.lb.addr,
		Store:    backend.Options{Driver: backend.DriverPostgres, PostgresDSN: dsn},
	}
	g.leaders = startLeaderWatch(g.nodes)
	g.attempts = startAttemptWatch(t.Name(), db)
	t.Cleanup(g.close)
	return g
}

// newNode makes a server's directory, proxies and config file.
func (g *haGroup) newNode(name string) *haNode {
	t := g.t
	n := &haNode{name: name, dir: t.TempDir(), httpAddr: fmt.Sprintf("127.0.0.1:%d", freePort(t))}
	urls := make([]string, 0, len(g.rig.urls))
	for _, u := range g.rig.urls {
		p := newTCPProxy(t, strings.TrimPrefix(u, "tls://"))
		n.proxies = append(n.proxies, p)
		urls = append(urls, strconv.Quote("tls://"+p.addr()))
	}
	c := g.rig
	cfg := fmt.Sprintf(`http:
  addr: %q
store:
  driver: postgres
  postgres:
    dsn: %q
    max_conns: 8
discovery:
  enabled: false
scheduler:
  tick_interval: 100ms
ha:
  enabled: true
  instance_name: %q
nats:
  mode: external
  auth:
    enabled: true
    enrollment_endpoint_enabled: true
  external:
    urls: [%s]
    account: %q
    nkey_seed_file: %q
    stream_replicas: %d
    connect_timeout: 30s
    tls:
      ca_file: %q
    callout:
      nkey_seed_file: %q
      issuer_seed_file: %q
      system_creds_file: %q
`, n.httpAddr, g.dsn, name, strings.Join(urls, ", "), clusterAccount, c.dataSeed, clusterReplicas,
		c.caFile, c.calloutSeed, c.issuerSeed, c.systemSeed)
	n.configPath = filepath.Join(n.dir, "sqi-server.yaml")
	if err := os.WriteFile(n.configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return n
}

// start starts a new run of node n, without waiting for it.
func (g *haGroup) start(n *haNode) *haProc {
	t := g.t
	t.Helper()
	level := cmp.Or(g.logLevel, "info")
	cmd := exec.CommandContext(context.Background(), g.rig.serverBin,
		"--config", n.configPath, "--log-format", "text", "--log-level", level, "serve")
	cmd.Dir = n.dir
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "SQI_") })
	p := &haProc{cmd: cmd, out: &lockedBuffer{}, exited: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p.out, p.out
	var logFile *os.File
	if dir := os.Getenv("SQI_TEST_HA_LOG_DIR"); dir != "" {
		logFile = g.logFile(dir, n)
		cmd.Stdout = io.MultiWriter(p.out, logFile)
		cmd.Stderr = cmd.Stdout
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sqi-server %s: %v", n.name, err)
	}
	go func() {
		p.waitErr = cmd.Wait()
		if logFile != nil {
			_ = logFile.Close()
		}
		close(p.exited)
	}()
	n.mu.Lock()
	n.procs = append(n.procs, p)
	n.mu.Unlock()
	return p
}

// logFile creates a file under dir for the next run of node n's output; the
// caller closes it when the run exits. SQI_TEST_HA_LOG_DIR names dir, for
// reading a run's whole output after the fact.
func (g *haGroup) logFile(dir string, n *haNode) *os.File {
	t := g.t
	n.mu.Lock()
	run := len(n.procs) + 1
	n.mu.Unlock()
	path := filepath.Join(dir, strings.ReplaceAll(t.Name(), "/", "_"), fmt.Sprintf("%s-run%d.log", n.name, run))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// startAll starts every server of the group at once.
func (g *haGroup) startAll() {
	for _, n := range g.nodes {
		g.start(n)
	}
}

// stopNode stops node n gracefully and fails the test unless it exits 0.
func (g *haGroup) stopNode(n *haNode) {
	t := g.t
	t.Helper()
	p := n.proc()
	p.expect(0)
	p.signal(t, syscall.SIGTERM)
	code, ok := p.waitExit(haStopWait)
	if !ok {
		t.Fatalf("%s did not exit within %s of SIGTERM\n%s", n.name, haStopWait, n.logTail())
	}
	if code != 0 {
		t.Fatalf("%s exited %d after SIGTERM, want 0\n%s", n.name, code, n.logTail())
	}
}

// kill ends node n's current run with SIGKILL, as a crash would.
func (g *haGroup) kill(n *haNode) {
	g.t.Helper()
	p := n.proc()
	p.expect(-1)
	p.signal(g.t, syscall.SIGKILL)
}

// close stops the watchers and every server still running, and reports what
// the watchers saw, and any run that ended without the test ending it. On a failure it logs the end of every server's output.
func (g *haGroup) close() {
	t := g.t
	if v := g.leaders.stop(); len(v) > 0 {
		t.Errorf("two servers answered /leaderz 200 at once (%d times):\n%s", len(v), strings.Join(v, "\n"))
	}
	if v := g.attempts.stop(); len(v) > 0 {
		t.Errorf("a task had two open attempts (%d times):\n%s", len(v), strings.Join(v, "\n"))
	}
	for _, n := range g.nodes {
		p := n.proc()
		if p == nil || !p.running() {
			continue
		}
		// 75: a server that was losing its leadership as the test ended.
		p.expect(0, 75)
		if err := p.cmd.Process.Signal(syscall.SIGCONT); err != nil {
			t.Logf("SIGCONT %s: %v", n.name, err)
		}
		if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Logf("SIGTERM %s: %v", n.name, err)
		}
		if _, ok := p.waitExit(haStopWait); !ok {
			t.Errorf("%s did not stop within %s of SIGTERM", n.name, haStopWait)
			p.expect(-1)
			_ = p.cmd.Process.Kill() //nolint:errcheck // already failing
			<-p.exited
		}
	}
	g.assertNoUnexpectedExits()
	g.lb.close()
	if err := g.db.Close(); err != nil {
		t.Logf("close the group's database: %v", err)
	}
	if t.Failed() {
		for _, n := range g.nodes {
			t.Logf("%s", n.logTail())
		}
	}
}

// assertNoUnexpectedExits fails the test for every run of every server that
// ended without the test ending it, or with a code the test did not expect.
func (g *haGroup) assertNoUnexpectedExits() {
	for _, n := range g.nodes {
		n.mu.Lock()
		procs := slices.Clone(n.procs)
		n.mu.Unlock()
		for i, p := range procs {
			if why := p.unexpectedExit(); why != "" {
				g.t.Errorf("%s run %d (pid %d) %s", n.name, i+1, p.cmd.Process.Pid, why)
			}
		}
	}
}

// allLogs is the end of every server's output, for a failure message.
func (g *haGroup) allLogs() string {
	var b strings.Builder
	for _, n := range g.nodes {
		b.WriteString(n.logTail())
	}
	return b.String()
}

// ── Probes ───────────────────────────────────────────────────────────────────

// haProbeClient answers within a second or not at all, so a frozen server
// reads as not answering.
var haProbeClient = &http.Client{Timeout: time.Second}

// leaderProbe is one server's /leaderz answer.
type leaderProbe struct {
	answered bool
	leader   bool
	epoch    int64
	role     string
}

func probeLeaderz(addr string) leaderProbe {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/leaderz", nil)
	if err != nil {
		return leaderProbe{}
	}
	resp, err := haProbeClient.Do(req)
	if err != nil {
		return leaderProbe{}
	}
	defer resp.Body.Close()
	var body struct {
		Role  string `json:"role"`
		Epoch int64  `json:"epoch"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return leaderProbe{}
	}
	return leaderProbe{answered: true, leader: resp.StatusCode == http.StatusOK, epoch: body.Epoch, role: body.Role}
}

// probeAll probes every node's /leaderz at once.
func probeAll(nodes []*haNode) []leaderProbe {
	out := make([]leaderProbe, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		if !n.running() {
			continue
		}
		wg.Go(func() { out[i] = probeLeaderz(n.httpAddr) })
	}
	wg.Wait()
	return out
}

// waitLeader waits until exactly one server answers /leaderz 200 and it is
// not one of except, and returns it and its epoch.
func (g *haGroup) waitLeader(timeout time.Duration, except ...*haNode) (*haNode, int64) {
	t := g.t
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var leaders []int
		probes := probeAll(g.nodes)
		for i, p := range probes {
			if p.leader {
				leaders = append(leaders, i)
			}
		}
		if len(leaders) == 1 && !slices.Contains(except, g.nodes[leaders[0]]) {
			return g.nodes[leaders[0]], probes[leaders[0]].epoch
		}
		if time.Now().After(deadline) {
			t.Fatalf("no single new leader within %s (leaders: %v)\n%s", timeout, leaders, g.allLogs())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitStandby waits until node n is ready as a standby: /readyz 200 and
// /leaderz 503.
func (g *haGroup) waitStandby(n *haNode, timeout time.Duration) {
	t := g.t
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		p := n.proc()
		if !p.running() {
			code := p.exitCode()
			t.Fatalf("%s exited %d while starting as a standby\n%s", n.name, code, n.logTail())
		}
		if readyzOK(haProbeClient, n.httpAddr) {
			if lp := probeLeaderz(n.httpAddr); lp.answered && !lp.leader {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not ready as a standby within %s\n%s", n.name, timeout, n.logTail())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// startGroup starts every server and waits for a leader and every other
// server ready as a standby, and returns the leader.
func (g *haGroup) startGroup() *haNode {
	g.t.Helper()
	g.startAll()
	leader, _ := g.waitLeader(2 * time.Minute)
	for _, n := range g.nodes {
		if n != leader {
			g.waitStandby(n, 2*time.Minute)
		}
	}
	return leader
}

// other is the one node of a two-server group that is not n.
func (g *haGroup) other(n *haNode) *haNode {
	for _, m := range g.nodes {
		if m != n {
			return m
		}
	}
	g.t.Fatal("the group has one server")
	return nil
}

// ── Watchers ─────────────────────────────────────────────────────────────────

// leaderWatch probes every server's /leaderz at once, every 50ms, and records
// each round in which more than one answered 200.
type leaderWatch struct {
	stopCh     chan struct{}
	done       chan struct{}
	violations []string
}

func startLeaderWatch(nodes []*haNode) *leaderWatch {
	w := &leaderWatch{stopCh: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for {
			select {
			case <-w.stopCh:
				return
			case <-time.After(50 * time.Millisecond):
			}
			var leaders []string
			for i, p := range probeAll(nodes) {
				if p.leader {
					leaders = append(leaders, fmt.Sprintf("%s (epoch %d)", nodes[i].name, p.epoch))
				}
			}
			if len(leaders) > 1 {
				w.violations = append(w.violations, time.Now().Format(time.StampMilli)+": "+strings.Join(leaders, ", "))
			}
		}
	}()
	return w
}

// stop ends the watch and returns what it recorded.
func (w *leaderWatch) stop() []string {
	close(w.stopCh)
	<-w.done
	return w.violations
}

// openAttemptsQuery finds every task with more than one open attempt.
const openAttemptsQuery = `SELECT task_id, count(*) FROM task_attempts WHERE ended_at IS NULL GROUP BY task_id HAVING count(*) > 1`

// attemptWatch polls a group's database every 200ms for a task with two open
// attempts. A poll that fails (the database is down) is counted, not judged.
type attemptWatch struct {
	scenario   string
	db         *sql.DB
	stopCh     chan struct{}
	done       chan struct{}
	failed     int
	violations []string

	mu    sync.Mutex
	polls int
}

func startAttemptWatch(scenario string, db *sql.DB) *attemptWatch {
	w := &attemptWatch{scenario: scenario, db: db, stopCh: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for {
			select {
			case <-w.stopCh:
				return
			case <-time.After(200 * time.Millisecond):
			}
			w.poll()
		}
	}()
	return w
}

func (w *attemptWatch) poll() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := w.db.QueryContext(ctx, openAttemptsQuery)
	if err != nil {
		w.failed++
		return
	}
	defer rows.Close()
	for rows.Next() {
		var task string
		var open int
		if rows.Scan(&task, &open) == nil {
			w.violations = append(w.violations, fmt.Sprintf("%s: task %s has %d open attempts", time.Now().Format(time.StampMilli), task, open))
		}
	}
	if rows.Err() != nil {
		w.failed++
		return
	}
	w.mu.Lock()
	w.polls++
	w.mu.Unlock()
}

// pollCount is how many polls have succeeded so far.
func (w *attemptWatch) pollCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.polls
}

// stop ends the watch, adds it to the run's ledger and returns its
// violations.
func (w *attemptWatch) stop() []string {
	close(w.stopCh)
	<-w.done
	haAttemptLedger.add(w)
	return w.violations
}

// haAttemptLedger is every attempt watch of the run, so
// TestHA_NoDoubleAssignment can report across the scenarios before it.
var haAttemptLedger attemptLedger

type attemptLedger struct {
	mu         sync.Mutex
	scenarios  []string
	polls      int
	violations []string
}

func (l *attemptLedger) add(w *attemptWatch) {
	l.mu.Lock()
	defer l.mu.Unlock()
	polls := w.pollCount()
	l.scenarios = append(l.scenarios, fmt.Sprintf("%s (%d polls, %d failed)", w.scenario, polls, w.failed))
	l.polls += polls
	for _, v := range w.violations {
		l.violations = append(l.violations, w.scenario+": "+v)
	}
}

// ── The balancer ─────────────────────────────────────────────────────────────

// haBalancer sends each request to the server answering /leaderz 200,
// checked every 100ms. A request that cannot be connected, or that a standby
// refuses with 503, is retried on the leader for up to haBalancerHold; so is
// one that arrives while no server leads. It holds less than the suite's
// 10s client timeout, so a helper sees the balancer's 503, not its own
// timeout.
type haBalancer struct {
	addr     string
	backends []string
	srv      *http.Server
	client   *http.Client
	stopCh   chan struct{}
	done     chan struct{}

	mu     sync.Mutex
	leader string
}

const haBalancerHold = 8 * time.Second

func newHABalancer(t *testing.T, backends []string) *haBalancer {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("balancer listen: %v", err)
	}
	b := &haBalancer{
		addr:     ln.Addr().String(),
		backends: backends,
		client:   &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
	}
	b.srv = &http.Server{Handler: b, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = b.srv.Serve(ln) }() //nolint:errcheck // ends with ErrServerClosed at close
	go b.check()
	return b
}

// check keeps leader current.
func (b *haBalancer) check() {
	defer close(b.done)
	for {
		leader := ""
		var best int64 = -1
		for _, addr := range b.backends {
			if p := probeLeaderz(addr); p.leader && p.epoch > best {
				leader, best = addr, p.epoch
			}
		}
		b.mu.Lock()
		b.leader = leader
		b.mu.Unlock()
		select {
		case <-b.stopCh:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (b *haBalancer) current() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.leader
}

func (b *haBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	deadline := time.Now().Add(haBalancerHold)
	for {
		resp, retry := b.forward(r, body)
		if resp != nil && !retry {
			defer resp.Body.Close()
			maps.Copy(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body) //nolint:errcheck // the client went away
			return
		}
		if time.Now().After(deadline) {
			http.Error(w, "no leader", http.StatusServiceUnavailable)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// forward sends r to the leader. retry is true when it may be sent again: no
// leader is known, the connection could not be made, or a standby refused
// it. A connection that broke after the request was sent is not retried.
func (b *haBalancer) forward(r *http.Request, body []byte) (resp *http.Response, retry bool) {
	leader := b.current()
	if leader == "" {
		return nil, true
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, "http://"+leader+r.URL.RequestURI(), strings.NewReader(string(body)))
	if err != nil {
		return nil, true
	}
	req.Header = r.Header.Clone()
	resp, err = b.client.Do(req)
	if err != nil {
		if opErr, ok := errors.AsType[*net.OpError](err); ok && opErr.Op == "dial" {
			return nil, true
		}
		return &http.Response{
			StatusCode: http.StatusBadGateway, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(err.Error())),
		}, false
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		return resp, false
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	var refusal struct {
		Error string `json:"error"`
	}
	if err == nil && json.Unmarshal(data, &refusal) == nil && refusal.Error == "standby" {
		return nil, true
	}
	resp.Body = io.NopCloser(strings.NewReader(string(data)))
	return resp, false
}

func (b *haBalancer) close() {
	close(b.stopCh)
	<-b.done
	_ = b.srv.Close()
}

// ── A TCP proxy to one broker node ───────────────────────────────────────────

// tcpProxy forwards connections to one broker node. Cutting it closes every
// connection it carries and refuses new ones while the listener stays up, so
// the port is still this proxy's when it is restored.
type tcpProxy struct {
	ln     net.Listener
	target string

	mu    sync.Mutex
	cut   bool
	conns map[net.Conn]struct{}
}

func newTCPProxy(t *testing.T, target string) *tcpProxy {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &tcpProxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	go p.accept()
	t.Cleanup(func() {
		_ = ln.Close()
		p.setCut(true)
	})
	return p
}

func (p *tcpProxy) addr() string { return p.ln.Addr().String() }

func (p *tcpProxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if !p.track(c) {
			_ = c.Close()
			continue
		}
		go p.serve(c)
	}
}

// track adds c to the connections a cut closes, unless the proxy is cut.
func (p *tcpProxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cut {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *tcpProxy) untrack(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, c)
}

func (p *tcpProxy) serve(c net.Conn) {
	defer p.untrack(c)
	defer c.Close()
	d, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(context.Background(), "tcp", p.target)
	if err != nil {
		return
	}
	if !p.track(d) {
		_ = d.Close()
		return
	}
	defer p.untrack(d)
	defer d.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(d, c); done <- struct{}{} }() //nolint:errcheck // ends when either side closes
	go func() { _, _ = io.Copy(c, d); done <- struct{}{} }() //nolint:errcheck // ends when either side closes
	<-done
}

func (p *tcpProxy) setCut(cut bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = cut
	if !cut {
		return
	}
	for c := range p.conns {
		_ = c.Close()
		delete(p.conns, c)
	}
}

// ── The farm ─────────────────────────────────────────────────────────────────

// farm makes a farm and a queue through the balancer, and enrolls one real
// worker on them, connected straight to the broker's nodes.
func (g *haGroup) farm() (farmID, queueID string) {
	t := g.t
	t.Helper()
	farmID, queueID = seedFarmAndQueue(t, g.api)
	token := seedJoinToken(t, g.api.Store, "ha-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	startRealWorkerNoWait(t, g.api, farmID, queueID, nil, []string{
		"SQI_WORKER_NATS_URL=" + strings.Join(g.rig.urls, ","),
		"SQI_WORKER_NATS_TLS_CA_FILE=" + g.rig.caFile,
		"SQI_WORKER_NATS_SERVER_URL=http://" + g.lb.addr,
		"SQI_WORKER_NATS_JOIN_TOKEN=" + token,
	}, false)
	if id := findOnlineWorker(t, g.api, farmID, 90*time.Second); id == "" {
		t.Fatalf("the worker never came online\n%s", g.allLogs())
	}
	return farmID, queueID
}

// haJob is a job of the given number of tasks, each of which appends its
// number to runs (so the test can count how many times each ran), prints
// it, and sleeps sleep seconds.
func haJob(runs string, tasks, sleep int) string {
	script := fmt.Sprintf(`echo {{Task.Param.N}} >> '%s'; echo "run {{Task.Param.N}}"; sleep %d`, runs, sleep)
	return fmt.Sprintf(`specificationVersion: jobtemplate-2023-09
name: ha-job
steps:
  - name: work
    parameterSpace:
      taskParameterDefinitions:
        - name: N
          type: INT
          range: "1-%d"
    script:
      actions:
        onRun:
          command: /bin/sh
          args: ["-c", %q]
`, tasks, script)
}

// submit submits a job of the given number of tasks through the balancer and returns its
// ID and the file its tasks append to.
func (g *haGroup) submit(farmID, queueID string, tasks, sleep int) (jobID, runs string) {
	g.t.Helper()
	runs = filepath.Join(g.t.TempDir(), "runs")
	return submitJobCustomYAML(g.t, g.api, farmID, queueID, haJob(runs, tasks, sleep)), runs
}

// query runs a one-row query on the group's database.
func (g *haGroup) query(q string, args []any, dst ...any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return g.db.QueryRowContext(ctx, q, args...).Scan(dst...)
}

// unfinished counts jobID's tasks that are not yet terminal, and running
// those that are running. A failed query reads as -1, -1.
func (g *haGroup) unfinished(jobID string) (unfinished, running int) {
	err := g.query(`SELECT count(*) FILTER (WHERE status NOT IN ('succeeded', 'failed', 'canceled')),
		count(*) FILTER (WHERE status = 'running') FROM tasks WHERE job_id = $1`, []any{jobID}, &unfinished, &running)
	if err != nil {
		return -1, -1
	}
	return unfinished, running
}

// waitRunning waits until one of jobID's tasks runs while others are still
// to finish, so whatever the test does next happens mid-job.
func (g *haGroup) waitRunning(jobID string) {
	t := g.t
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, running := g.unfinished(jobID); running > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no task of job %s ran within 90s\n%s", jobID, g.allLogs())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitJob waits until jobID is terminal in the database and fails the test
// unless it completed.
func (g *haGroup) waitJob(jobID string, timeout time.Duration) {
	t := g.t
	t.Helper()
	deadline := time.Now().Add(timeout)
	status := ""
	for {
		if err := g.query(`SELECT status FROM jobs WHERE id = $1`, []any{jobID}, &status); err == nil &&
			slices.Contains([]string{"completed", "failed", "canceled"}, status) {
			break
		}
		if time.Now().After(deadline) {
			unfinished, _ := g.unfinished(jobID)
			t.Fatalf("job %s is %q after %s with %d tasks unfinished\n%s", jobID, status, timeout, unfinished, g.allLogs())
		}
		time.Sleep(250 * time.Millisecond)
	}
	if status != "completed" {
		t.Fatalf("job %s ended %q, want completed\n%s", jobID, status, g.allLogs())
	}
}

// assertRanOnce fails the test unless each of jobID's tasks (1 to tasks)
// appended its number to runs exactly once: no task ran twice, and none was
// lost. On a failure it logs every attempt of the job.
func (g *haGroup) assertRanOnce(jobID, runs string, tasks int) {
	t := g.t
	t.Helper()
	data, err := os.ReadFile(runs)
	if err != nil {
		t.Fatalf("read the runs file: %v", err)
	}
	counts := map[int]int{}
	for f := range strings.FieldsSeq(string(data)) {
		n, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("runs file holds %q", f)
		}
		counts[n]++
	}
	ok := len(counts) == tasks
	for n := 1; n <= tasks; n++ {
		if counts[n] != 1 {
			t.Errorf("task %d ran %d times, want once", n, counts[n])
			ok = false
		}
	}
	if !ok {
		t.Errorf("the runs file names %d tasks, want %d: %v\nattempts:\n%s", len(counts), tasks, counts, g.attemptHistory(jobID))
	}
}

// attemptHistory describes every attempt of jobID's tasks, for a failure
// message.
func (g *haGroup) attemptHistory(jobID string) string {
	const q = `SELECT k.parameters, a.attempt_number, a.status, a.worker_id, a.started_at::text,
		coalesce(a.ended_at::text, '-'), a.message
		FROM task_attempts a JOIN tasks k ON k.id = a.task_id WHERE k.job_id = $1
		ORDER BY k.parameters, a.attempt_number`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := g.db.QueryContext(ctx, q, jobID)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var params, status, worker, started, ended, message string
		var number int
		if err := rows.Scan(&params, &number, &status, &worker, &started, &ended, &message); err != nil {
			return err.Error()
		}
		fmt.Fprintf(&b, "%s #%d %s worker=%s started=%s ended=%s %q\n", params, number, status, worker, started, ended, message)
	}
	if err := rows.Err(); err != nil {
		return err.Error()
	}
	return b.String()
}

// jobAttempt is one attempt of a task, and how many log chunks it produced.
type jobAttempt struct {
	status string
	logs   int
}

// jobAttempts lists jobID's attempts by task.
func (g *haGroup) jobAttempts(jobID string) map[string][]jobAttempt {
	t := g.t
	t.Helper()
	const q = `SELECT a.task_id, a.status, (SELECT count(*) FROM task_logs l WHERE l.attempt_id = a.id)
		FROM task_attempts a JOIN tasks k ON k.id = a.task_id WHERE k.job_id = $1`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := g.db.QueryContext(ctx, q, jobID)
	if err != nil {
		t.Fatalf("list the job's attempts: %v", err)
	}
	defer rows.Close()
	byTask := map[string][]jobAttempt{}
	for rows.Next() {
		var task string
		var a jobAttempt
		if err := rows.Scan(&task, &a.status, &a.logs); err != nil {
			t.Fatal(err)
		}
		byTask[task] = append(byTask[task], a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list the job's attempts: %v", err)
	}
	return byTask
}

// judgeAttempts lists what is wrong with a job's attempts by task, and
// counts the attempts closed without running.
func judgeAttempts(byTask map[string][]jobAttempt) (problems []string, reclaimed int) {
	for task, as := range byTask {
		succeeded := 0
		for _, a := range as {
			switch {
			case a.status == "succeeded" && a.logs > 0:
				succeeded++
			case a.status == "succeeded":
				problems = append(problems, task+": its succeeded attempt has no log output yet")
			case a.logs > 0:
				problems = append(problems, fmt.Sprintf("%s: a %s attempt has %d log chunks, so it ran", task, a.status, a.logs))
			default:
				reclaimed++
			}
		}
		if succeeded != 1 {
			problems = append(problems, fmt.Sprintf("%s: %d succeeded attempts with output, want 1", task, succeeded))
		}
	}
	return problems, reclaimed
}

// assertReclaimedNeverRan fails the test unless every task of jobID has
// exactly one succeeded attempt with log output, and every other attempt has
// none: an attempt the leader committed and then lost before a worker ran it
// may be reclaimed, but an attempt that ran may not.
func (g *haGroup) assertReclaimedNeverRan(jobID string) {
	t := g.t
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		problems, reclaimed := judgeAttempts(g.jobAttempts(jobID))
		if len(problems) == 0 {
			t.Logf("every task ran once; %d attempts were closed without running", reclaimed)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s's attempts:\n%s", jobID, strings.Join(problems, "\n"))
		}
		time.Sleep(time.Second)
	}
}

// ── The tests ────────────────────────────────────────────────────────────────

// TestHA_ExactlyOneLeaderUnderContention starts three servers at once on an
// empty database, so they also race to migrate it, and checks that one
// leads and two stand by, as /leaderz and GET /api/v1/servers report. Then
// it forces 50 handovers, stopping the leader and starting it again each
// time: every handover elects one of the other two at a higher epoch, and
// the watch never sees two leaders.
func TestHA_ExactlyOneLeaderUnderContention(t *testing.T) {
	rig := requireHARig(t)
	g := newHAGroup(t, rig, 3)
	leader := g.startGroup()
	g.assertServers(leader)

	_, epoch := g.waitLeader(time.Second)
	const handovers = 50
	began := time.Now()
	for round := 1; round <= handovers; round++ {
		old, oldEpoch := leader, epoch
		g.stopNode(old)
		leader, epoch = g.waitLeader(30*time.Second, old)
		if epoch <= oldEpoch {
			t.Fatalf("handover %d: %s leads at epoch %d, not above %s's %d", round, leader.name, epoch, old.name, oldEpoch)
		}
		g.start(old)
		g.waitStandby(old, 90*time.Second)
	}
	t.Logf("%d handovers in %s; %s leads at epoch %d", handovers, time.Since(began).Round(time.Second), leader.name, epoch)

	var resp serversWire
	mustDoJSON(t, http.MethodGet, apiURL(g.api, "/api/v1/servers"), nil, "", http.StatusOK, &resp)
	if got := resp.leaders(); len(got) != 1 || got[0] != leader.name {
		t.Fatalf("after the handovers GET /api/v1/servers names leaders %v, want [%s]", got, leader.name)
	}
}

// serversWire is GET /api/v1/servers' response.
type serversWire struct {
	HA      bool  `json:"ha"`
	Epoch   int64 `json:"epoch"`
	Servers []struct {
		Name string `json:"name"`
		Role string `json:"role"`
	} `json:"servers"`
}

// leaders names the servers the response reports as leader.
func (s serversWire) leaders() []string {
	var out []string
	for _, m := range s.Servers {
		if m.Role == "leader" {
			out = append(out, m.Name)
		}
	}
	return out
}

// assertServers checks GET /api/v1/servers on each server directly: the
// leader lists itself as leader and the others as standbys; a standby
// refuses with 503 naming the leader.
func (g *haGroup) assertServers(leader *haNode) {
	t := g.t
	t.Helper()
	for _, n := range g.nodes {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+n.httpAddr+"/api/v1/servers", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("GET /api/v1/servers on %s: %v", n.name, err)
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if n != leader {
			if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" ||
				!strings.Contains(string(data), `"leader":"`+leader.name+`"`) {
				t.Errorf("standby %s answered GET /api/v1/servers %d (Retry-After %q): %s; want 503 naming %s",
					n.name, resp.StatusCode, resp.Header.Get("Retry-After"), data, leader.name)
			}
			continue
		}
		var s serversWire
		if resp.StatusCode != http.StatusOK || json.Unmarshal(data, &s) != nil {
			t.Fatalf("leader %s answered GET /api/v1/servers %d: %s", n.name, resp.StatusCode, data)
		}
		roles := map[string]string{}
		for _, m := range s.Servers {
			roles[m.Name] = m.Role
		}
		for _, m := range g.nodes {
			want := "standby"
			if m == leader {
				want = "leader"
			}
			if roles[m.name] != want {
				t.Errorf("GET /api/v1/servers reports %s as %q, want %q (%s)", m.name, roles[m.name], want, data)
			}
		}
		if !s.HA || len(s.Servers) != len(g.nodes) || s.Epoch == 0 {
			t.Errorf("GET /api/v1/servers: %s", data)
		}
	}
}

// TestHA_CrashFailover kills the leader with SIGKILL while a 40-task job
// runs. A standby leads within the failover bound, the job completes, and
// no task ran twice: each task's command ran exactly once (it records every
// run in a file), and an attempt the new leader closed without it
// succeeding had produced no output, so no worker ran it.
func TestHA_CrashFailover(t *testing.T) {
	rig := requireHARig(t)
	g := newHAGroup(t, rig, 2)
	leader := g.startGroup()
	farmID, queueID := g.farm()

	const tasks = 40
	jobID, runs := g.submit(farmID, queueID, tasks, 1)
	g.waitRunning(jobID)
	if unfinished, _ := g.unfinished(jobID); unfinished < 2 {
		t.Fatalf("only %d tasks of job %s were unfinished before the crash; lengthen the job", unfinished, jobID)
	}

	killed := time.Now()
	g.kill(leader)
	next, epoch := g.waitLeader(haFailoverBound, leader)
	t.Logf("%s killed; %s leads at epoch %d after %s", leader.name, next.name, epoch, time.Since(killed).Round(100*time.Millisecond))

	g.waitJob(jobID, 4*time.Minute)
	g.assertRanOnce(jobID, runs, tasks)
	g.assertReclaimedNeverRan(jobID)
}

// haSnapshot is what the rows a leader writes look like: per table, the row
// count and the latest change. Only the leader writes these.
func (g *haGroup) haSnapshot() string {
	t := g.t
	t.Helper()
	queries := []string{
		`SELECT count(*), coalesce(max(updated_at)::text, '') FROM jobs`,
		`SELECT count(*), coalesce(max(updated_at)::text, '') FROM steps`,
		`SELECT count(*), coalesce(max(updated_at)::text, '') FROM tasks`,
		`SELECT count(*), coalesce(max(greatest(started_at, ended_at, created_at))::text, '') FROM task_attempts`,
		`SELECT count(*), coalesce(string_agg(status, ',' ORDER BY id), '') FROM tasks`,
		`SELECT count(*), coalesce(string_agg(status || ':' || coalesce(ended_at::text, '-'), ',' ORDER BY id), '') FROM task_attempts`,
	}
	parts := make([]string, 0, len(queries))
	for _, q := range queries {
		var n int
		var latest string
		if err := g.query(q, nil, &n, &latest); err != nil {
			t.Fatalf("snapshot %q: %v", q, err)
		}
		parts = append(parts, fmt.Sprintf("%d %s", n, latest))
	}
	return strings.Join(parts, "\n")
}

// TestHA_PauseFailover freezes the leader with SIGSTOP for 40s, past its
// lease, while its tasks run. A standby takes over and completes the job.
// On SIGCONT the old leader must exit with the demotion code within a lease,
// and it must change nothing: the rows only a leader writes are the same
// after it exits as just before it resumed.
func TestHA_PauseFailover(t *testing.T) {
	rig := requireHARig(t)
	g := newHAGroup(t, rig, 2)
	leader := g.startGroup()
	standby := g.other(leader)
	farmID, queueID := g.farm()

	// A task that declares no core count takes the whole worker, so the two
	// run one after the other: the first under the old leader, the second
	// under the new one once the worker's lease request parked on the frozen
	// leader has timed out (35s). The job completes while the old leader is
	// still frozen, so nothing but the old leader can change a row once it
	// resumes.
	const tasks = 2
	jobID, runs := g.submit(farmID, queueID, tasks, 3)
	g.waitRunning(jobID)

	frozen := leader.proc()
	stopped := time.Now()
	frozen.expect(75) // fenced on resuming
	frozen.signal(t, syscall.SIGSTOP)
	t.Cleanup(func() { _ = frozen.cmd.Process.Signal(syscall.SIGCONT) }) //nolint:errcheck // may have exited
	next, epoch := g.waitLeader(haFailoverBound, leader)
	if next != standby {
		t.Fatalf("%s leads, want %s", next.name, standby.name)
	}
	t.Logf("%s frozen; %s leads at epoch %d after %s", leader.name, next.name, epoch, time.Since(stopped).Round(100*time.Millisecond))
	g.waitJob(jobID, 3*time.Minute)

	if rest := 40*time.Second - time.Since(stopped); rest > 0 {
		time.Sleep(rest)
	}
	before := g.haSnapshot()
	resumed := time.Now()
	frozen.signal(t, syscall.SIGCONT)
	code, ok := frozen.waitExit(haLeaseTTL)
	if !ok {
		t.Fatalf("the resumed leader %s did not exit within %s\n%s", leader.name, haLeaseTTL, leader.logTail())
	}
	if code != 75 {
		t.Fatalf("the resumed leader %s exited %d, want 75\n%s", leader.name, code, leader.logTail())
	}
	t.Logf("%s resumed and exited 75 after %s", leader.name, time.Since(resumed).Round(100*time.Millisecond))
	if after := g.haSnapshot(); after != before {
		t.Errorf("rows changed while the deposed leader resumed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if now, nowEpoch := g.waitLeader(time.Second); now != standby || nowEpoch != epoch {
		t.Errorf("after the old leader resumed %s leads at epoch %d, want %s at %d", now.name, nowEpoch, standby.name, epoch)
	}
	g.assertRanOnce(jobID, runs, tasks)
}

// TestHA_NoDoubleAssignment checks that no task ever had two open attempts:
// across every scenario the run has gone through so far (the attempt watch
// of each group polled every 200ms), and through one of its own in which
// the leader of a 40-task job is killed, its successor frozen past its lease
// and resumed, and the third server finishes the job.
func TestHA_NoDoubleAssignment(t *testing.T) {
	rig := requireHARig(t)

	haAttemptLedger.mu.Lock()
	scenarios, polls, violations := slices.Clone(haAttemptLedger.scenarios), haAttemptLedger.polls, slices.Clone(haAttemptLedger.violations)
	haAttemptLedger.mu.Unlock()
	t.Logf("earlier scenarios: %d polls across %v", polls, scenarios)
	if len(violations) > 0 {
		t.Errorf("earlier scenarios saw a task with two open attempts:\n%s", strings.Join(violations, "\n"))
	}

	g := newHAGroup(t, rig, 3)
	first := g.startGroup()
	farmID, queueID := g.farm()
	const tasks = 40
	jobID, runs := g.submit(farmID, queueID, tasks, 1)
	g.waitRunning(jobID)

	g.kill(first)
	second, _ := g.waitLeader(haFailoverBound, first)
	g.waitRunning(jobID)

	frozen := second.proc()
	frozen.expect(75) // fenced on resuming
	frozen.signal(t, syscall.SIGSTOP)
	t.Cleanup(func() { _ = frozen.cmd.Process.Signal(syscall.SIGCONT) }) //nolint:errcheck // may have exited
	third, _ := g.waitLeader(haFailoverBound, first, second)
	frozen.signal(t, syscall.SIGCONT)
	if code, ok := frozen.waitExit(haLeaseTTL); !ok || code != 75 {
		t.Fatalf("the resumed leader %s: exit %d (exited %v), want 75 within %s\n%s", second.name, code, ok, haLeaseTTL, second.logTail())
	}
	t.Logf("%s killed, %s frozen and resumed, %s leads", first.name, second.name, third.name)

	g.waitJob(jobID, 5*time.Minute)
	g.assertRanOnce(jobID, runs, tasks)
	if polls := g.attempts.pollCount(); polls < 100 {
		t.Errorf("the attempt watch polled %d times during the scenario, want at least 100", polls)
	}
}

// TestHA_GracefulHandover stops the leader with SIGTERM while the worker is
// idle, its lease request parked on the leader. The leader answers it empty
// as it stops, and releases its lease, so the standby promotes itself at its
// next poll and a job submitted to it has its first task leased within 5s of
// the standby opening its API as the leader.
func TestHA_GracefulHandover(t *testing.T) {
	rig := requireHARig(t)
	g := newHAGroup(t, rig, 2)
	g.logLevel = "debug" // the leader logs a parked lease request at Debug
	leader := g.startGroup()
	standby := g.other(leader)
	farmID, queueID := g.farm()
	waitLogLine(t, leader, "scheduler: no work for the lease request; parking it", 30*time.Second)

	const ready = "sqi-server ready as the leader"
	stopped := time.Now()
	leader.proc().expect(0)
	leader.proc().signal(t, syscall.SIGTERM)
	readyAt := waitLogLine(t, standby, ready, 30*time.Second)
	t.Logf("%s was ready as the leader %s after the leader's SIGTERM", standby.name, readyAt.Sub(stopped).Round(10*time.Millisecond))
	if code, ok := leader.proc().waitExit(haStopWait); !ok || code != 0 {
		t.Fatalf("the stopped leader %s: exit %d (exited %v), want 0\n%s", leader.name, code, ok, leader.logTail())
	}
	g.waitLeader(10*time.Second, leader)

	jobID, _ := g.submit(farmID, queueID, 1, 0)
	deadline := time.Now().Add(30 * time.Second)
	for {
		var leased int
		if err := g.query(`SELECT count(*) FROM tasks WHERE job_id = $1 AND status NOT IN ('pending', 'ready')`,
			[]any{jobID}, &leased); err == nil && leased > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s's task was not leased within 30s\n%s", jobID, g.allLogs())
		}
		time.Sleep(25 * time.Millisecond)
	}
	if took := time.Since(readyAt); took > 5*time.Second {
		t.Fatalf("the first task was leased %s after %s was ready as the leader, want within 5s",
			took.Round(10*time.Millisecond), standby.name)
	} else {
		t.Logf("the first task was leased %s after the new leader was ready", took.Round(10*time.Millisecond))
	}
	g.waitJob(jobID, time.Minute)
}

// waitLogLine waits until node n's current run has logged line, and returns
// when it was first seen.
func waitLogLine(t *testing.T, n *haNode, line string, timeout time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if strings.Contains(n.proc().out.String(), line) {
			return time.Now()
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not log %q within %s\n%s", n.name, line, timeout, n.logTail())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// logLineTime is the time, as its own log record states it, at which node n's
// current run first logged line.
func logLineTime(t *testing.T, n *haNode, line string) time.Time {
	t.Helper()
	for l := range strings.Lines(n.proc().out.String()) {
		if !strings.Contains(l, line) {
			continue
		}
		stamp, _, _ := strings.Cut(strings.TrimPrefix(l, "time="), " ")
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			t.Fatalf("%s logged %q with no readable time: %q (%v)", n.name, line, l, err)
		}
		return at
	}
	t.Fatalf("%s never logged %q\n%s", n.name, line, n.logTail())
	return time.Time{}
}

// TestHA_LeaderLosesBroker cuts the leader off from every broker node while
// the standby still reaches them. The leader steps down once it has been
// disconnected for longer than its lease and the standby reports a working
// broker, exiting with the demotion code; the standby then leads, and the
// farm runs a job.
func TestHA_LeaderLosesBroker(t *testing.T) {
	rig := requireHARig(t)
	g := newHAGroup(t, rig, 2)
	leader := g.startGroup()
	standby := g.other(leader)
	farmID, queueID := g.farm()

	cut := time.Now()
	leader.proc().expect(75)
	leader.cutBroker(true)
	code, ok := leader.proc().waitExit(45 * time.Second)
	if !ok {
		t.Fatalf("the leader cut off from the broker did not exit within 45s\n%s", leader.logTail())
	}
	took := time.Since(cut)
	if code != 75 {
		t.Fatalf("the leader cut off from the broker exited %d, want 75\n%s", code, leader.logTail())
	}
	if took < haLeaseTTL {
		t.Fatalf("the leader stepped down %s after losing the broker, before its lease (%s) had passed", took, haLeaseTTL)
	}
	next, epoch := g.waitLeader(15*time.Second, leader)
	if next != standby {
		t.Fatalf("%s leads, want %s", next.name, standby.name)
	}
	t.Logf("%s exited 75 %s after losing the broker; %s leads at epoch %d", leader.name, took.Round(100*time.Millisecond), next.name, epoch)
	// The leader releases the lease just before it logs the loss, so the
	// standby leads at its next poll plus the promotion, not when the lease
	// lapses, and not after the old leader's own (slower) exit.
	waitLogLine(t, standby, "sqi-server ready as the leader", 10*time.Second)
	steppedDown := logLineTime(t, leader, "ha: lost leadership")
	if gap := logLineTime(t, standby, "sqi-server ready as the leader").Sub(steppedDown); gap > 3*time.Second {
		t.Fatalf("%s was ready as the leader %s after %s stepped down, want within 3s",
			standby.name, gap.Round(10*time.Millisecond), leader.name)
	} else {
		t.Logf("%s was ready as the leader %s after %s stepped down", standby.name, gap.Round(10*time.Millisecond), leader.name)
	}

	leader.cutBroker(false)
	g.start(leader)
	g.waitStandby(leader, 90*time.Second)
	jobID, runs := g.submit(farmID, queueID, 2, 0)
	g.waitJob(jobID, 2*time.Minute)
	g.assertRanOnce(jobID, runs, 2)
}

// TestHA_BrokerDownForEveryone stops all three broker nodes for 30s. With no
// server able to reach the broker, none steps down: no process exits and the
// same leader leads at the same epoch throughout. Once the nodes are back
// the farm runs a job.
func TestHA_BrokerDownForEveryone(t *testing.T) {
	rig := requireHARig(t)
	g := newHAGroup(t, rig, 2)
	leader := g.startGroup()
	_, epoch := g.waitLeader(time.Second)
	farmID, queueID := g.farm()

	rig.compose(t, append([]string{"stop"}, rig.nodes...)...)
	restarted := false
	t.Cleanup(func() {
		if !restarted {
			rig.compose(t, append([]string{"start"}, rig.nodes...)...)
		}
		if err := rig.awaitHealthy(2 * time.Minute); err != nil {
			t.Errorf("the cluster did not recover: %v", err)
		}
	})
	until := time.Now().Add(30 * time.Second)
	for time.Now().Before(until) {
		g.assertAllRunning()
		if p := probeLeaderz(leader.httpAddr); !p.leader || p.epoch != epoch {
			t.Fatalf("with the broker down %s answers /leaderz %+v, want leader at epoch %d\n%s", leader.name, p, epoch, g.allLogs())
		}
		time.Sleep(500 * time.Millisecond)
	}
	rig.compose(t, append([]string{"start"}, rig.nodes...)...)
	restarted = true
	if err := rig.awaitHealthy(2 * time.Minute); err != nil {
		t.Fatalf("the cluster did not recover: %v", err)
	}
	g.assertAllRunning()
	if now, nowEpoch := g.waitLeader(30 * time.Second); now != leader || nowEpoch != epoch {
		t.Fatalf("after the broker returned %s leads at epoch %d, want %s at %d", now.name, nowEpoch, leader.name, epoch)
	}

	jobID, runs := g.submit(farmID, queueID, 2, 0)
	g.waitJob(jobID, 2*time.Minute)
	g.assertRanOnce(jobID, runs, 2)
	g.assertAllRunning()
}

// assertAllRunning fails the test if any server has exited.
func (g *haGroup) assertAllRunning() {
	t := g.t
	t.Helper()
	for _, n := range g.nodes {
		if p := n.proc(); !p.running() {
			code := p.exitCode()
			t.Fatalf("%s exited %d\n%s", n.name, code, n.logTail())
		}
	}
}

// TestHA_PostgresRestart stops the PostgreSQL container under a running job
// and starts it again. The leader, unable to renew its lease, fences itself
// and exits with the demotion code; it is started again, as its supervisor
// would; once the database is back a server leads and the job completes
// with every task run once.
func TestHA_PostgresRestart(t *testing.T) {
	rig := requireHARig(t)
	g := newHAGroup(t, rig, 2)
	leader := g.startGroup()
	farmID, queueID := g.farm()
	const tasks = 6
	jobID, runs := g.submit(farmID, queueID, tasks, 5)
	g.waitRunning(jobID)

	standby := g.other(leader)
	survivor := standby.proc()
	stopped := time.Now()
	leader.proc().expect(75)
	rig.docker(t, "stop", rig.pgContainer)
	started := false
	t.Cleanup(func() {
		if !started {
			rig.docker(t, "start", rig.pgContainer)
		}
	})
	code, ok := leader.proc().waitExit(45 * time.Second)
	if !ok || code != 75 {
		t.Fatalf("with PostgreSQL stopped the leader %s: exit %d (exited %v), want 75 within 45s\n%s", leader.name, code, ok, leader.logTail())
	}
	t.Logf("%s exited 75 %s after PostgreSQL stopped", leader.name, time.Since(stopped).Round(100*time.Millisecond))

	rig.docker(t, "start", rig.pgContainer)
	started = true
	g.waitDatabase(time.Minute)

	// The standby rode the outage out: the same process, warm, takes over as
	// soon as PostgreSQL is back. The old leader is started again only then,
	// as its supervisor would, so it cannot be the one that wins.
	next, epoch := g.waitLeader(90*time.Second, leader)
	if next != standby || standby.proc() != survivor || !survivor.running() {
		t.Fatalf("after the outage %s leads; want %s's original process, which must have survived it\n%s",
			next.name, standby.name, g.allLogs())
	}
	t.Logf("PostgreSQL back after %s; %s, which stood by through the outage, leads at epoch %d",
		time.Since(stopped).Round(100*time.Millisecond), next.name, epoch)
	g.start(leader)
	g.waitStandby(leader, 90*time.Second)

	g.waitJob(jobID, 4*time.Minute)
	g.assertRanOnce(jobID, runs, tasks)
}

// waitDatabase waits until the group's database answers.
func (g *haGroup) waitDatabase(timeout time.Duration) {
	t := g.t
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var one int
		if g.query(`SELECT 1`, nil, &one) == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("PostgreSQL did not answer within %s of starting", timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestHA_WrongDatabaseRefused starts a server on a second database with the
// same broker account, as a group's config copied with the wrong DSN would.
// The account's guard is held, and refreshed, by a server that is not a
// member of that database, so the newcomer must refuse to start, saying so,
// and the group's leader must be untouched.
func TestHA_WrongDatabaseRefused(t *testing.T) {
	rig := requireHARig(t)
	g := newHAGroup(t, rig, 1)
	leader := g.startGroup()
	_, epoch := g.waitLeader(time.Second)

	other := newHAGroupOn(t, rig, 1, storetest.PostgresEmptyDSN(t), "wrong")
	stray := other.nodes[0]
	p := other.start(stray)
	p.expect(1) // a startup refusal
	code, ok := p.waitExit(2 * time.Minute)
	if !ok {
		t.Fatalf("the server on the wrong database did not exit within 2m\n%s", stray.logTail())
	}
	if code == 0 || code == 75 {
		t.Fatalf("the server on the wrong database exited %d, want a refusal\n%s", code, stray.logTail())
	}
	const want = "is not a member of this database: is this server pointed at the wrong database?"
	if !strings.Contains(p.out.String(), want) {
		t.Fatalf("the refusal does not say %q:\n%s", want, stray.logTail())
	}
	if now, nowEpoch := g.waitLeader(time.Second); now != leader || nowEpoch != epoch {
		t.Fatalf("after the refusal %s leads at epoch %d, want %s at %d", now.name, nowEpoch, leader.name, epoch)
	}
}
