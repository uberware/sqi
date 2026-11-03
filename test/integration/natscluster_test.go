// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// natscluster_test.go: the external broker mode against a real three-node
// nats-server cluster in Docker, run by `make test-nats-cluster`
// (scripts/test-nats-cluster.sh).
//
// sqi runs here as real binaries: sqi-server is the built binary reading a
// YAML config file in nats.mode: external, and sqi-worker is the built worker
// enrolling through a join token. The one exception is
// TestNATSCluster_ExpiryReconnectsThreeTimes, which needs a user JWT lifetime
// below the configurable floor and so runs sqi's broker client in process.
// The cluster is configured the way an operator configures one, from
// `sqi-server nats external init` and `sqi-server tls init`, and every client
// dials it over TLS with worker authentication on. The REST API stays plain
// HTTP: these tests cover TLS on the broker, which is what external mode adds.
//
// The tests share one cluster and run in order. Each starts its own
// sqi-server on a fresh database and stops it cleanly, which releases the
// account's one-server guard for the next test. TestNATSCluster_SurvivesNodeLoss
// starts the node it stopped again and waits for the cluster to be whole.
//
// Every test skips without SQI_TEST_NATS_URLS, which only the script sets, so
// `make test-integration` skips them all. CI asserts each test's PASS line by
// name, so a skip there fails the job.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store/backend"
)

// natsCluster is the cluster scripts/test-nats-cluster.sh started, as its
// environment describes it.
type natsCluster struct {
	urls  []string // client URLs, in node order
	nodes []string // node (Compose service and server) names, in node order

	dataSeed, calloutSeed, issuerSeed, systemSeed string
	caFile                                        string

	composeFile, composeProject string
	serverBin                   string

	// image is the nats-server image the nodes run, as nats:<version>@<digest>.
	image string
}

// clusterAccount is the account `nats external init` writes by default.
const clusterAccount = bus.DefaultAccount

// clusterReplicas is the stream replica count the tests run sqi-server with:
// every stream survives the loss of one node.
const clusterReplicas = 3

// serverConsumers are the durable consumers sqi-server creates, which a
// restore deletes.
var serverConsumers = map[string]string{
	"sqi-task-status-srv": bus.StreamTask,
	"sqi-task-logs-srv":   bus.StreamLogs,
	"sqi-worker-srv":      bus.StreamWorker,
}

// sqiStreams are the four streams a restore purges.
var sqiStreams = []string{bus.StreamTask, bus.StreamLogs, bus.StreamWorker, bus.StreamCancel}

var (
	clusterReadyOnce sync.Once
	errClusterReady  error
)

// requireNATSCluster returns the cluster under test, skipping without one.
// The first call waits until every node is up and JetStream is whole.
func requireNATSCluster(t *testing.T) *natsCluster {
	t.Helper()
	urls := os.Getenv("SQI_TEST_NATS_URLS")
	if urls == "" {
		t.Skip("SQI_TEST_NATS_URLS not set; run `make test-nats-cluster` for the real NATS cluster tests")
	}
	env := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			t.Fatalf("SQI_TEST_NATS_URLS is set but %s is not; run these tests through scripts/test-nats-cluster.sh", name)
		}
		return v
	}
	c := &natsCluster{
		urls:           strings.Split(urls, ","),
		nodes:          strings.Split(env("SQI_TEST_NATS_NODES"), ","),
		dataSeed:       env("SQI_TEST_NATS_DATA_SEED"),
		calloutSeed:    env("SQI_TEST_NATS_CALLOUT_SEED"),
		issuerSeed:     env("SQI_TEST_NATS_ISSUER_SEED"),
		systemSeed:     env("SQI_TEST_NATS_SYSTEM_SEED"),
		caFile:         env("SQI_TEST_NATS_CA_FILE"),
		composeFile:    env("SQI_TEST_NATS_COMPOSE_FILE"),
		composeProject: env("SQI_TEST_NATS_COMPOSE_PROJECT"),
		serverBin:      env("SQI_TEST_SERVER_BIN"),
		image:          env("SQI_TEST_NATS_IMAGE"),
	}
	if len(c.urls) != len(c.nodes) {
		t.Fatalf("SQI_TEST_NATS_URLS names %d nodes, SQI_TEST_NATS_NODES %d", len(c.urls), len(c.nodes))
	}
	clusterReadyOnce.Do(func() { errClusterReady = c.awaitHealthy(2 * time.Minute) })
	if errClusterReady != nil {
		t.Fatalf("the NATS cluster never became healthy: %v", errClusterReady)
	}
	return c
}

// dial connects to the cluster over TLS as the nkey user whose seed is in
// seedFile. An empty seedFile connects with no credential at all.
func (c *natsCluster) dial(seedFile string, extra ...nats.Option) (*nats.Conn, error) {
	opts := []nats.Option{nats.RootCAs(c.caFile), nats.Timeout(5 * time.Second), nats.Name("sqi integration test")}
	if seedFile != "" {
		nkey, err := nats.NkeyOptionFromSeed(seedFile)
		if err != nil {
			return nil, err
		}
		opts = append(opts, nkey)
	}
	return nats.Connect(strings.Join(c.urls, ","), append(opts, extra...)...)
}

// mustDial is [natsCluster.dial], failing the test on error and closing the
// connection when the test ends.
func (c *natsCluster) mustDial(t *testing.T, seedFile string) *nats.Conn {
	t.Helper()
	nc, err := c.dial(seedFile)
	if err != nil {
		t.Fatalf("connect to the cluster: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// jetStream returns a JetStream handle on sqi's account, as sqi's data user.
func (c *natsCluster) jetStream(t *testing.T) jetstream.JetStream {
	t.Helper()
	js, err := jetstream.New(c.mustDial(t, c.dataSeed))
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	return js
}

// compose runs `docker compose <args>` on the cluster's project. The script
// exported the variables the Compose file reads.
func (c *natsCluster) compose(t *testing.T, args ...string) {
	t.Helper()
	full := append([]string{"compose", "-p", c.composeProject, "-f", c.composeFile}, args...)
	out, err := exec.CommandContext(context.Background(), "docker", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(full, " "), err, out)
	}
}

// awaitHealthy waits until every node answers and every stream in sqi's
// account has a leader and all its replicas current.
func (c *natsCluster) awaitHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := c.healthy()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("after %s: %w", timeout, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// nodeVersions asks every node for its statistics as the system user and
// returns each answering node's nats-server version by node name. It fails
// unless every node answers.
func (c *natsCluster) nodeVersions() (map[string]string, error) {
	sys, err := c.dial(c.systemSeed)
	if err != nil {
		return nil, fmt.Errorf("system user connect: %w", err)
	}
	defer sys.Close()
	versions := map[string]string{}
	err = c.askEveryNode(sys, "$SYS.REQ.SERVER.PING", nil, func(m *nats.Msg) {
		var resp struct {
			Server struct {
				Name    string `json:"name"`
				Version string `json:"ver"`
			} `json:"server"`
		}
		if json.Unmarshal(m.Data, &resp) == nil && resp.Server.Name != "" {
			versions[resp.Server.Name] = resp.Server.Version
		}
	})
	if err != nil {
		return nil, err
	}
	if len(versions) != len(c.nodes) {
		return nil, fmt.Errorf("%d of %d nodes answered (%v)", len(versions), len(c.nodes), versions)
	}
	return versions, nil
}

func (c *natsCluster) healthy() error {
	if _, err := c.nodeVersions(); err != nil {
		return err
	}

	data, err := c.dial(c.dataSeed)
	if err != nil {
		return fmt.Errorf("data user connect: %w", err)
	}
	defer data.Close()
	js, err := jetstream.New(data)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := js.AccountInfo(ctx); err != nil {
		return fmt.Errorf("account info: %w", err)
	}
	streams := js.ListStreams(ctx)
	for info := range streams.Info() {
		if err := streamCurrent(info); err != nil {
			return err
		}
	}
	return streams.Err()
}

// streamCurrent reports why a stream is not fully replicated, or nil.
func streamCurrent(info *jetstream.StreamInfo) error {
	name := info.Config.Name
	if info.Cluster == nil || info.Cluster.Leader == "" {
		return fmt.Errorf("stream %s has no leader", name)
	}
	if got := len(info.Cluster.Replicas) + 1; got != info.Config.Replicas {
		return fmt.Errorf("stream %s lists %d of its %d replicas", name, got, info.Config.Replicas)
	}
	for _, r := range info.Cluster.Replicas {
		if r.Offline || !r.Current {
			return fmt.Errorf("stream %s replica on %s is not current (offline=%v)", name, r.Name, r.Offline)
		}
	}
	return nil
}

// askEveryNode publishes a system request to every node and passes each
// answer to handle, until every node has answered or two seconds pass.
func (c *natsCluster) askEveryNode(sys *nats.Conn, subject string, body []byte, handle func(*nats.Msg)) error {
	replies := make(chan *nats.Msg, 4*len(c.nodes))
	inbox := sys.NewInbox()
	sub, err := sys.ChanSubscribe(inbox, replies)
	if err != nil {
		return err
	}
	defer sub.Unsubscribe() //nolint:errcheck // the inbox is this call's alone
	if err := sys.PublishRequest(subject, inbox, body); err != nil {
		return err
	}
	window := time.After(2 * time.Second)
	for answered := 0; answered < len(c.nodes); {
		select {
		case m := <-replies:
			if len(m.Data) == 0 { // a no-responders status, not an answer
				continue
			}
			answered++
			handle(m)
		case <-window:
			return nil
		}
	}
	return nil
}

// workerConns returns the connections the cluster holds for workerID, as
// the system user sees them on every node, and fails the test when a node
// does not answer: an unanswered node could be holding one.
func (c *natsCluster) workerConns(t *testing.T, workerID string) []*natsserver.ConnInfo {
	t.Helper()
	sys := c.mustDial(t, c.systemSeed)
	defer sys.Close()
	req, err := json.Marshal(natsserver.ConnzEventOptions{ConnzOptions: natsserver.ConnzOptions{
		Username: true,
		User:     workerID,
		Account:  clusterAccount,
		Limit:    math.MaxInt32,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var conns []*natsserver.ConnInfo
	answered := 0
	err = c.askEveryNode(sys, "$SYS.REQ.SERVER.PING.CONNZ", req, func(m *nats.Msg) {
		var resp natsserver.ServerAPIConnzResponse
		if json.Unmarshal(m.Data, &resp) != nil || resp.Error != nil || resp.Data == nil {
			return
		}
		answered++
		for _, ci := range resp.Data.Conns {
			if ci.AuthorizedUser == workerID {
				conns = append(conns, ci)
			}
		}
	})
	if err != nil {
		t.Fatalf("list the cluster's connections: %v", err)
	}
	if answered != len(c.nodes) {
		t.Fatalf("only %d of %d nodes listed their connections", answered, len(c.nodes))
	}
	return conns
}

// ── sqi-server, the real binary ─────────────────────────────────────────────

// lockedBuffer is an io.Writer safe for a subprocess's output goroutine and
// the test reading it at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// clusterServer is a running sqi-server binary in external mode.
type clusterServer struct {
	*testServer

	configPath string
	cmd        *exec.Cmd
	out        *lockedBuffer
	exited     chan struct{}
	waitErr    error
	stopOnce   sync.Once
}

// writeServerConfig writes the sqi-server.yaml for a server on httpAddr and
// the SQLite database at dbPath into dir, and returns its path. It is the
// block `nats external init` prints, filled in with this cluster's URLs, R3
// streams and the cluster's CA.
func (c *natsCluster) writeServerConfig(t *testing.T, dir, httpAddr, dbPath string) string {
	t.Helper()
	quoted := make([]string, len(c.urls))
	for i, u := range c.urls {
		quoted[i] = strconv.Quote(u)
	}
	cfg := fmt.Sprintf(`http:
  addr: %q
store:
  sqlite_path: %q
discovery:
  enabled: false
scheduler:
  tick_interval: 100ms
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
`, httpAddr, dbPath, strings.Join(quoted, ", "), clusterAccount, c.dataSeed, clusterReplicas,
		c.caFile, c.calloutSeed, c.issuerSeed, c.systemSeed)
	path := filepath.Join(dir, "sqi-server.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// serverCLI returns an sqi-server command reading configPath. Its
// environment carries no SQI_ variable, so the config file is all it reads,
// and it runs in dir, so it finds no config of the repository's.
func (c *natsCluster) serverCLI(dir, configPath string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(context.Background(), c.serverBin,
		append([]string{"--config", configPath, "--log-format", "text", "--log-level", "warn"}, args...)...)
	cmd.Dir = dir
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "SQI_") })
	return cmd
}

// launchServer starts sqi-server serve on a fresh HTTP port with the
// database at dbPath, without waiting for it.
func (c *natsCluster) launchServer(t *testing.T, dbPath string) *clusterServer {
	t.Helper()
	dir := t.TempDir()
	httpAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	configPath := c.writeServerConfig(t, dir, httpAddr, dbPath)
	s := &clusterServer{
		testServer: &testServer{
			HTTPAddr: httpAddr,
			NATSAddr: strings.TrimPrefix(c.urls[0], "tls://"),
			Store:    backend.Options{Driver: backend.DriverSQLite, SQLitePath: dbPath},
			DBPath:   dbPath,
		},
		configPath: configPath,
		out:        &lockedBuffer{},
		exited:     make(chan struct{}),
	}
	s.cmd = c.serverCLI(dir, configPath, "serve")
	s.cmd.Stdout = io.MultiWriter(os.Stderr, s.out)
	s.cmd.Stderr = s.cmd.Stdout
	if err := s.cmd.Start(); err != nil {
		t.Fatalf("start sqi-server: %v", err)
	}
	go func() {
		s.waitErr = s.cmd.Wait()
		close(s.exited)
	}()
	t.Cleanup(func() {
		if err := s.stop(); err != nil {
			t.Errorf("stop sqi-server (pid %d): %v", s.cmd.Process.Pid, err)
		}
	})
	return s
}

// startServer starts sqi-server and waits until it is ready.
func (c *natsCluster) startServer(t *testing.T, dbPath string) *clusterServer {
	t.Helper()
	s := c.launchServer(t, dbPath)
	deadline := time.Now().Add(90 * time.Second)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case <-s.exited:
			t.Fatalf("sqi-server exited during startup (%v):\n%s", s.waitErr, s.out.String())
		default:
		}
		if readyzOK(client, s.HTTPAddr) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("sqi-server not ready within 90s:\n%s", s.out.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func readyzOK(client *http.Client, httpAddr string) bool {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+httpAddr+"/readyz", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// stop interrupts the server and waits for it to exit. A clean exit is what
// releases the account's guard at once, so anything else is an error. A
// server that had already exited is not stopped here: a test that expects
// one to exit reads its exit status itself.
func (s *clusterServer) stop() error {
	var err error
	s.stopOnce.Do(func() {
		select {
		case <-s.exited:
			return
		default:
		}
		if sigErr := s.cmd.Process.Signal(os.Interrupt); sigErr != nil {
			err = sigErr
			return
		}
		select {
		case <-s.exited:
			err = s.waitErr
		case <-time.After(45 * time.Second):
			_ = s.cmd.Process.Kill() //nolint:errcheck // already failing
			<-s.exited
			err = errors.New("did not exit within 45s of an interrupt")
		}
	})
	return err
}

// Stop stops the server, failing the test unless it exits cleanly.
func (s *clusterServer) Stop(t *testing.T) {
	t.Helper()
	if err := s.stop(); err != nil {
		t.Fatalf("stop sqi-server: %v\n%s", err, s.out.String())
	}
}

// ── sqi-worker, the real binary ─────────────────────────────────────────────

// startClusterWorker enrolls a real sqi-worker with a join token over REST
// and connects it to every node over TLS, and returns its ID once it is
// online. The join token must have been seeded before the server started.
func (c *natsCluster) startClusterWorker(t *testing.T, s *clusterServer, farmID, queueID, joinToken string) string {
	t.Helper()
	startRealWorkerNoWait(t, s.testServer, farmID, queueID, nil, []string{
		"SQI_WORKER_NATS_URL=" + strings.Join(c.urls, ","),
		"SQI_WORKER_NATS_TLS_CA_FILE=" + c.caFile,
		"SQI_WORKER_NATS_SERVER_URL=http://" + s.HTTPAddr,
		"SQI_WORKER_NATS_JOIN_TOKEN=" + joinToken,
	}, false)
	id := findOnlineWorker(t, s.testServer, farmID, 60*time.Second)
	if id == "" {
		t.Fatalf("the worker never came online: enrollment, the TLS connection or the auth callout failed\n%s", s.out.String())
	}
	return id
}

// clusterFarm starts a server on a fresh database with a join token seeded,
// a farm and queue, and an enrolled worker.
func (c *natsCluster) clusterFarm(t *testing.T) (s *clusterServer, farmID, queueID, workerID string) {
	t.Helper()
	dbPath := t.TempDir() + "/test.db"
	so := backend.Options{Driver: backend.DriverSQLite, SQLitePath: dbPath}
	joinToken := seedJoinToken(t, so, "nats-cluster")
	s = c.startServer(t, dbPath)
	farmID, queueID = seedFarmAndQueue(t, s.testServer)
	workerID = c.startClusterWorker(t, s, farmID, queueID, joinToken)
	return s, farmID, queueID, workerID
}

// ── The tests ────────────────────────────────────────────────────────────────

// clusterEchoJob prints a line unique to the test that submits it.
func clusterEchoJob(sentinel string) string {
	return fmt.Sprintf(`specificationVersion: "jobtemplate-2023-09"
name: NATS cluster job
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: echo
          args: [%q]
`, sentinel)
}

// TestNATSCluster_FarmRunsJobWithAuthAndTLS runs a job end to end through the
// cluster: a real sqi-server in external mode with worker authentication on,
// a real worker enrolled by join token, every connection over TLS. It also
// checks the cluster side of that claim: an anonymous client is refused, the
// worker's connection is TLS and named after its worker ID (which only the
// auth callout's JWT does), and the streams are R3 in sqi's account.
func TestNATSCluster_FarmRunsJobWithAuthAndTLS(t *testing.T) {
	c := requireNATSCluster(t)
	s, farmID, queueID, workerID := c.clusterFarm(t)

	// Every node runs the image under test, so a pass is a pass on that
	// nats-server version.
	want := strings.TrimPrefix(c.image, "nats:")
	want, _, _ = strings.Cut(want, "@")
	versions, err := c.nodeVersions()
	if err != nil {
		t.Fatalf("ask the nodes for their versions: %v", err)
	}
	for node, got := range versions {
		if got != want {
			t.Fatalf("node %s runs nats-server %s, want %s (%s)", node, got, want, c.image)
		}
	}
	t.Logf("all %d nodes run nats-server %s", len(versions), want)

	if nc, err := c.dial(""); err == nil {
		nc.Close()
		t.Fatal("an anonymous client connected to the cluster; worker authentication is not in force")
	}

	conns := c.workerConns(t, workerID)
	if len(conns) != 1 {
		t.Fatalf("the cluster holds %d connections for worker %s, want 1", len(conns), workerID)
	}
	if conns[0].TLSVersion == "" {
		t.Errorf("worker %s's connection is not TLS", workerID)
	}
	if conns[0].Account != clusterAccount {
		t.Errorf("worker %s's connection is in account %q, want %q", workerID, conns[0].Account, clusterAccount)
	}

	sentinel := fmt.Sprintf("nats-cluster-%d", time.Now().UnixNano())
	jobID := submitJobCustomYAML(t, s.testServer, farmID, queueID, clusterEchoJob(sentinel))
	if got := pollJobStatus(t, s.testServer, jobID, []string{"completed", "failed", "canceled"}, 60*time.Second); got != "completed" {
		t.Fatalf("job %s ended %q, want completed\n%s", jobID, got, s.out.String())
	}
	taskID := firstTaskID(t, s.testServer, jobID)
	if logs := pollTaskLogs(t, s.testServer, taskID, sentinel, 30*time.Second); !strings.Contains(logs, sentinel) {
		t.Fatalf("task %s's logs do not contain %q: %q", taskID, sentinel, logs)
	}

	js := c.jetStream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, name := range sqiStreams {
		st, err := js.Stream(ctx, name)
		if err != nil {
			t.Fatalf("stream %s in account %s: %v", name, clusterAccount, err)
		}
		if got := st.CachedInfo().Config.Replicas; got != clusterReplicas {
			t.Errorf("stream %s has %d replicas, want %d", name, got, clusterReplicas)
		}
	}
}

// TestNATSCluster_SurvivesNodeLoss stops a node of the cluster while a job's
// tasks run, and the job still completes with every task's status and log
// line stored: with R3 streams, no message is lost to one node going away.
//
// The node stopped is the leader of SQI_TASK, the stream every status report
// goes through, so a new leader has to be elected while the tasks report.
func TestNATSCluster_SurvivesNodeLoss(t *testing.T) {
	c := requireNATSCluster(t)
	s, farmID, queueID, _ := c.clusterFarm(t)

	jobID := submitJobCustomYAML(t, s.testServer, farmID, queueID, nodeLossJob)
	waitTaskRunning(t, s.testServer, jobID, 60*time.Second)

	js := c.jetStream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := js.Stream(ctx, bus.StreamTask)
	if err != nil {
		t.Fatalf("stream %s: %v", bus.StreamTask, err)
	}
	leader := st.CachedInfo().Cluster.Leader
	if !slices.Contains(c.nodes, leader) {
		t.Fatalf("stream %s's leader is %q, not one of the nodes %v", bus.StreamTask, leader, c.nodes)
	}

	t.Logf("stopping %s, the leader of %s, with the job's tasks running", leader, bus.StreamTask)
	c.compose(t, "stop", leader)
	t.Cleanup(func() {
		c.compose(t, "start", leader)
		if err := c.awaitHealthy(2 * time.Minute); err != nil {
			t.Errorf("the cluster did not recover after %s restarted: %v", leader, err)
		}
	})
	// The node must be gone while the job still has work to report.
	unfinished := 0
	for _, task := range jobTasks(t, s.testServer, jobID) {
		if task.Status != "succeeded" && task.Status != "failed" && task.Status != "canceled" {
			unfinished++
		}
	}
	if unfinished == 0 {
		t.Fatalf("every task of job %s finished before %s stopped; lengthen nodeLossJob", jobID, leader)
	}
	t.Logf("%s stopped with %d of %d tasks unfinished", leader, unfinished, sleepJobTasks)

	if got := pollJobStatus(t, s.testServer, jobID, []string{"completed", "failed", "canceled"}, 3*time.Minute); got != "completed" {
		t.Fatalf("job %s ended %q with %s down, want completed\n%s", jobID, got, leader, s.out.String())
	}
	for _, task := range jobTasks(t, s.testServer, jobID) {
		if task.Status != "succeeded" {
			t.Errorf("task %s is %q, want succeeded", task.ID, task.Status)
		}
	}
	waitJobLogs(t, s.testServer, jobID)
}

// nodeLossJob is [sleepJob] with tasks long enough to outlast stopping a
// node (a few hundred milliseconds), even if the worker runs all of them at
// once: sleepJobTasks tasks, each printing "done N", so [waitJobLogs] applies.
const nodeLossJob = `specificationVersion: jobtemplate-2023-09
name: nats-cluster-node-loss
steps:
  - name: work
    parameterSpace:
      taskParameterDefinitions:
        - name: N
          type: INT
          range: "1-6"
    script:
      actions:
        onRun:
          command: /bin/sh
          args: ['-c', 'sleep 5; printf "done %s\n" {{Task.Param.N}}']
`

// waitTaskRunning waits until one of jobID's tasks is running.
func waitTaskRunning(t *testing.T, ts *testServer, jobID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, task := range jobTasks(t, ts, jobID) {
			if task.Status == "running" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no task of job %s started running within %s", jobID, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestNATSCluster_RevokeWithSystemCreds revokes a connected worker's
// credential over REST and checks, as the cluster's system user sees it, that
// its connection is gone from every node when the request returns and that
// its reconnect is refused. That is the system credential's job: without it
// the connection would last until its user JWT expired, an hour here.
func TestNATSCluster_RevokeWithSystemCreds(t *testing.T) {
	c := requireNATSCluster(t)
	s, _, _, workerID := c.clusterFarm(t)

	if conns := c.workerConns(t, workerID); len(conns) != 1 {
		t.Fatalf("before revoking, the cluster holds %d connections for worker %s, want 1", len(conns), workerID)
	}

	began := time.Now()
	revokeWorkerCredential(t, s.testServer, workerID)
	took := time.Since(began)
	t.Logf("DELETE /api/v1/workers/%s/credential returned after %s", workerID, took.Round(time.Millisecond))
	// Revoke waits out the cluster's auth timeout and checks again, so it
	// takes seconds; without the system credential the bound is the
	// hour-long user_ttl.
	if took > 30*time.Second {
		t.Errorf("revocation took %s, want well under nats.auth.user_ttl (1h)", took)
	}
	if conns := c.workerConns(t, workerID); len(conns) != 0 {
		t.Fatalf("after revoking, the cluster still holds %d connections for worker %s", len(conns), workerID)
	}

	// The worker keeps redialing, and the callout refuses it every time.
	time.Sleep(5 * time.Second)
	if conns := c.workerConns(t, workerID); len(conns) != 0 {
		t.Fatalf("5s after revoking, worker %s is connected again (%d connections); its reconnect was not refused",
			workerID, len(conns))
	}
}

// TestNATSCluster_ExpiryReconnectsThreeTimes checks, on the real cluster and
// so on each tested nats-server version, that a worker's user JWT expiring
// costs a reconnect and nothing more: the broker ends the session, the
// worker reconnects through the auth callout, gets a fresh JWT and carries on,
// three times over on one connection, and nats.go never closes it for good
// under its rule for the same authorization error twice in a row.
//
// The expiry is nats.auth.user_ttl, whose configured floor is a minute, so
// this runs sqi's own broker client (bus.Remote) in process with a 2s TTL
// rather than the sqi-server binary. It connects with the seeds and CA the
// script generated, as the binary does.
func TestNATSCluster_ExpiryReconnectsThreeTimes(t *testing.T) {
	c := requireNATSCluster(t)
	const workerID = "nats-cluster-expiry"
	seed, pub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(_ context.Context, key string) (string, error) {
		if key == pub {
			return workerID, nil
		}
		return "", bus.ErrUnknownCredential
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	r := bus.NewRemote(bus.RemoteConfig{
		URLs:           c.urls,
		Account:        clusterAccount,
		NkeySeedFile:   c.dataSeed,
		StreamReplicas: clusterReplicas,
		ConnectTimeout: 30 * time.Second,
		TLS:            bus.ClientTLSConfig{CAFile: c.caFile},
		Auth: bus.RemoteAuthConfig{
			AuthConfig:          bus.AuthConfig{Enabled: true, UserTTL: 2 * time.Second, Lookup: lookup},
			CalloutNkeySeedFile: c.calloutSeed,
			IssuerSeedFile:      c.issuerSeed,
			SystemCredsFile:     c.systemSeed,
		},
	}, logger)
	if err := r.Start(t.Context()); err != nil {
		t.Fatalf("start sqi's broker client on the cluster: %v", err)
	}
	t.Cleanup(r.Shutdown)

	var reconnects, expiries atomic.Int32
	nc, err := nats.Connect(strings.Join(c.urls, ","),
		brokerauth.NkeyOption(pub, seed),
		nats.RootCAs(c.caFile),
		nats.CustomInboxPrefix(brokerauth.InboxPrefix(workerID)),
		nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond),
		nats.ReconnectHandler(func(*nats.Conn) { reconnects.Add(1) }),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			if errors.Is(err, nats.ErrAuthExpired) {
				expiries.Add(1)
			}
		}))
	if err != nil {
		t.Fatalf("connect as the worker: %v", err)
	}
	t.Cleanup(nc.Close)

	deadline := time.Now().Add(30 * time.Second)
	for reconnects.Load() < 3 || expiries.Load() < 3 {
		if nc.Status() == nats.CLOSED {
			t.Fatalf("the connection was closed for good after %d expiries and %d reconnects: %v",
				expiries.Load(), reconnects.Load(), nc.LastError())
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 30s: %d expiries and %d reconnects, want 3 of each (status %s, last error %v)",
				expiries.Load(), reconnects.Load(), nc.Status(), nc.LastError())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("%d expiries, %d reconnects on one connection", expiries.Load(), reconnects.Load())

	waitUntil := time.Now().Add(10 * time.Second)
	for nc.Status() != nats.CONNECTED {
		if nc.Status() == nats.CLOSED || time.Now().After(waitUntil) {
			t.Fatalf("after three expiries the connection is %s, want CONNECTED: %v", nc.Status(), nc.LastError())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush after three expiries: %v", err)
	}
}

// TestNATSCluster_SecondServerRefused starts a second sqi-server on the same
// account, with a database of its own, as a staging server pointed at a
// production cluster by a copied config would be. It must refuse to start,
// naming the server that holds the account, and leave that server running.
func TestNATSCluster_SecondServerRefused(t *testing.T) {
	c := requireNATSCluster(t)
	first := c.startServer(t, t.TempDir()+"/test.db")

	second := c.launchServer(t, t.TempDir()+"/test.db")
	select {
	case <-second.exited:
	case <-time.After(90 * time.Second):
		t.Fatalf("a second sqi-server on the account did not exit within 90s:\n%s", second.out.String())
	}
	if second.waitErr == nil {
		t.Fatalf("the second sqi-server exited 0; want a refusal:\n%s", second.out.String())
	}
	out := second.out.String()
	for _, want := range []string{
		"another sqi-server holds this broker account",
		fmt.Sprintf("pid %d", first.cmd.Process.Pid),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the second server's refusal does not contain %q:\n%s", want, out)
		}
	}
	if readyzOK(&http.Client{Timeout: 5 * time.Second}, second.HTTPAddr) {
		t.Error("the refused server is serving HTTP")
	}
	if !readyzOK(&http.Client{Timeout: 5 * time.Second}, first.HTTPAddr) {
		t.Errorf("the first server is not ready after the second was refused:\n%s", first.out.String())
	}
}

// TestNATSCluster_RestorePurges runs `sqi-server restore` in external mode,
// as the binary, against a backup of a stopped server's database. First,
// while that server still runs, a restore is refused by the account's guard
// with nothing changed. Then, with the server stopped and a message left in
// each of sqi's four streams, the restore purges every one of them and
// deletes the server's three durable consumers, and the server starts again
// afterwards, so the restore gave the account back.
func TestNATSCluster_RestorePurges(t *testing.T) {
	c := requireNATSCluster(t)
	dir := t.TempDir()
	dbPath := dir + "/test.db"
	s := c.startServer(t, dbPath)
	backup := dir + "/backup.db"
	runServerCLI(t, c, s.configPath, "backup", "--db", dbPath, "--out", backup)

	js := c.jetStream(t)

	// Refused while the server holds the account. The target is a database
	// of its own, so the broker's guard is the only thing that can refuse.
	other := dir + "/other.db"
	out, err := c.serverCLI(dir, s.configPath, "restore", backup, "--db", other).CombinedOutput()
	if err == nil {
		t.Fatalf("restore succeeded while an sqi-server held the account:\n%s", out)
	}
	if !strings.Contains(string(out), "another sqi-server holds this broker account") {
		t.Errorf("restore's refusal does not name the guard:\n%s", out)
	}
	if _, statErr := os.Stat(other); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the refused restore wrote %s (stat: %v)", other, statErr)
	}
	for name, stream := range serverConsumers {
		requireConsumer(t, js, stream, name, true)
	}

	s.Stop(t)
	leaveOneMessageInEachStream(t, js)

	out, err = c.serverCLI(dir, s.configPath, "restore", backup, "--db", dbPath).CombinedOutput()
	if err != nil {
		t.Fatalf("sqi-server restore: %v\n%s", err, out)
	}
	t.Logf("sqi-server restore:\n%s", out)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, name := range sqiStreams {
		st, err := js.Stream(ctx, name)
		if err != nil {
			t.Fatalf("stream %s after restore: %v", name, err)
		}
		info, err := st.Info(ctx)
		if err != nil {
			t.Fatalf("stream %s info after restore: %v", name, err)
		}
		if info.State.Msgs != 0 {
			t.Errorf("stream %s holds %d messages after restore, want 0", name, info.State.Msgs)
		}
	}
	for name, stream := range serverConsumers {
		requireConsumer(t, js, stream, name, false)
	}

	c.startServer(t, dbPath)
}

// runServerCLI runs an sqi-server subcommand reading configPath, failing the
// test if it fails.
func runServerCLI(t *testing.T, c *natsCluster, configPath string, args ...string) {
	t.Helper()
	out, err := c.serverCLI(t.TempDir(), configPath, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("sqi-server %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// leaveOneMessageInEachStream publishes a message to each of sqi's streams
// with no server running to consume it, and checks each stream holds it.
func leaveOneMessageInEachStream(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	subjects := map[string]string{
		bus.StreamTask:   bus.SubjectTaskStatusPrefix + ".stale-worker.stale-job",
		bus.StreamLogs:   bus.SubjectTaskLogsPrefix + ".stale-worker.stale-task",
		bus.StreamWorker: bus.SubjectWorkerHeartbeatPrefix + ".stale-worker",
		bus.StreamCancel: bus.SubjectTaskCancelPrefix + ".stale-task",
	}
	for stream, subject := range subjects {
		if _, err := js.Publish(ctx, subject, []byte(`{}`)); err != nil {
			t.Fatalf("publish to %s: %v", subject, err)
		}
		st, err := js.Stream(ctx, stream)
		if err != nil {
			t.Fatalf("stream %s: %v", stream, err)
		}
		info, err := st.Info(ctx)
		if err != nil {
			t.Fatalf("stream %s info: %v", stream, err)
		}
		if info.State.Msgs == 0 {
			t.Fatalf("stream %s is empty after a publish; the purge assertion would prove nothing", stream)
		}
	}
}

// requireConsumer fails the test unless durable consumer name on stream
// exists (want true) or does not (want false).
func requireConsumer(t *testing.T, js jetstream.JetStream, stream, name string, want bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := js.Consumer(ctx, stream, name)
	switch {
	case want && err != nil:
		t.Errorf("consumer %s on %s: %v, want it to exist", name, stream, err)
	case !want && err == nil:
		t.Errorf("consumer %s on %s still exists", name, stream)
	case !want && !errors.Is(err, jetstream.ErrConsumerNotFound):
		t.Errorf("consumer %s on %s: %v, want %v", name, stream, err, jetstream.ErrConsumerNotFound)
	}
}
