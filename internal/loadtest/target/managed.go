// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/marker"
	"github.com/uberware/sqi/internal/loadtest/netfault"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/pgdsn"
	"github.com/uberware/sqi/internal/store"
)

const (
	defaultReadyTimeout = 60 * time.Second
	defaultStopTimeout  = 30 * time.Second
	workerOnlineTimeout = 60 * time.Second
	cliTimeout          = 2 * time.Minute
	// cleanupMargin is added to the stop grace when a failed start cleans up.
	cleanupMargin = 30 * time.Second
	logTail       = 40
	pollInterval  = 100 * time.Millisecond

	bootstrapUser = "sqi-load"
	// joinTokenTTL is the CLI's maximum, so a soak run's restarted workers
	// can still enroll hours in.
	joinTokenTTL = "24h"
	// workerGrace is a real worker's shutdown grace period, kept inside the
	// harness's stop timeout.
	workerGrace = "10s"
)

// ManagedConfig configures StartManaged.
type ManagedConfig struct {
	// RunDir holds everything of the run: server.yaml, server.log, data/,
	// tls/ and workers/.
	RunDir string
	// RunID names the farm (load-<RunID>) and a created PostgreSQL database;
	// "" means RunDir's base name.
	RunID string
	// ServerBin, WorkerBin and NoopBin are the binaries the target runs.
	// ServerBin is required; WorkerBin and NoopBin are needed only when the
	// scenario has real workers. ResolveBinaries builds those not given.
	ServerBin, WorkerBin, NoopBin string
	// Scenario is the run's scenario. Each worker group's Real count is
	// started as real sqi-worker processes, of which only the group's name,
	// Queues, Tags and Heartbeat apply.
	Scenario *scenario.Scenario
	// PostgresDSN is the store when Scenario.Server.Store is postgres. A DSN
	// naming the maintenance database "postgres" is administrative: the
	// harness creates sqi_load_<RunID> through it and drops it on Close. Any
	// other DSN names an empty database the harness uses as it is.
	PostgresDSN string
	// Logger receives the target's own progress; nil discards it.
	Logger *slog.Logger
	// Seed is the run seed, mixed into each broker lane's jitter; 0 seeds a
	// lane with its group's index alone.
	Seed uint64

	readyTimeout time.Duration // how long to wait for /readyz; 0 is defaultReadyTimeout
	stopTimeout  time.Duration // grace between interrupt and kill; 0 is defaultStopTimeout
}

// managed is a target whose server, and real workers, the harness runs.
type managed struct {
	cfg          ManagedConfig
	logger       *slog.Logger
	runID        string
	readyTimeout time.Duration
	stopTimeout  time.Duration

	settings   serverSettings
	configPath string

	pgDSN      string // the run's DSN
	pgAdminDSN string // set when the harness created pgDB
	pgDB       string

	bootstrapPassword string
	joinToken         string
	tlsFiles          simworker.TLSFiles
	httpURL, natsURL  string
	apiKey            string
	client            *client.Client
	farm              farm
	creds             simworker.Credentials

	// lanes are the broker proxies per worker group, set before any worker
	// starts and never changed after; nil without a network fault.
	lanes     map[string]*netfault.Proxy
	markerDir string

	mu           sync.Mutex // guards what follows against Close and the fault controller
	server       *proc
	workers      []*realWorker
	incarnations []Incarnation
	closing      bool // Close has begun: nothing more is started
	// resumedGroups are the task groups resumeAll continued, which Close
	// ends.
	resumedGroups []int

	closeOnce sync.Once
	closeErr  error
}

// StartManaged starts an sqi-server configured for the scenario, waits for it
// to be ready, creates the run's farm, queues and usage pools, and starts the
// real workers. On any error it stops whatever it had started before
// returning.
func StartManaged(ctx context.Context, cfg ManagedConfig) (Target, error) {
	m, err := newManaged(cfg)
	if err != nil {
		return nil, err
	}
	if err := m.start(ctx); err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.stopTimeout+cleanupMargin)
		defer cancel()
		if cerr := m.Close(cctx); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return nil, err
	}
	return m, nil
}

func newManaged(cfg ManagedConfig) (*managed, error) {
	if cfg.RunDir == "" {
		return nil, errors.New("target: ManagedConfig.RunDir is empty")
	}
	if cfg.Scenario == nil {
		return nil, errors.New("target: ManagedConfig has no scenario")
	}
	switch st := cfg.Scenario.Server.Store; {
	case st == "postgres" && cfg.PostgresDSN == "":
		return nil, errors.New("target: the scenario's store is postgres but ManagedConfig.PostgresDSN is empty")
	case st != "postgres" && cfg.PostgresDSN != "":
		// A DSN given for a SQLite run would be silently ignored, and the
		// record would describe SQLite while its reader expects PostgreSQL.
		return nil, fmt.Errorf("target: a PostgreSQL DSN was given but the scenario's store is %q; set server.store to postgres to run on it", cmp.Or(st, "sqlite"))
	}
	runDir, err := filepath.Abs(cfg.RunDir)
	if err != nil {
		return nil, fmt.Errorf("target: run directory: %w", err)
	}
	m := &managed{
		cfg:          cfg,
		logger:       discardIfNil(cfg.Logger).With(slog.String("target", "managed")),
		runID:        cfg.RunID,
		readyTimeout: orDefault(cfg.readyTimeout, defaultReadyTimeout),
		stopTimeout:  orDefault(cfg.stopTimeout, defaultStopTimeout),
		settings:     serverSettings{RunDir: runDir, Spec: cfg.Scenario.Server, BootstrapUser: bootstrapUser},
		configPath:   filepath.Join(runDir, "server.yaml"),
		creds:        simworker.NoCredentials{},
	}
	if m.runID == "" {
		m.runID = filepath.Base(runDir)
	}
	if slices.ContainsFunc(cfg.Scenario.Jobs, func(j scenario.JobClass) bool { return j.RealTask != nil }) {
		m.markerDir = filepath.Join(runDir, "markers")
	}
	return m, nil
}

// orDefault returns v, or def when v is not positive.
func orDefault(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

// ResolveBinaries fills in the binaries c does not name, building sqi-server,
// sqi-worker and the no-op task command into dir when one is missing:
// sqi-server always, and the other two when workers says the run starts real
// workers.
func (c *ManagedConfig) ResolveBinaries(ctx context.Context, dir string, workers bool) error {
	if c.ServerBin != "" && (!workers || (c.WorkerBin != "" && c.NoopBin != "")) {
		return nil
	}
	discardIfNil(c.Logger).InfoContext(ctx, "building sqi binaries")
	server, worker, noop, err := BuildBinaries(ctx, dir)
	if err != nil {
		return err
	}
	c.ServerBin = cmp.Or(c.ServerBin, server)
	c.WorkerBin = cmp.Or(c.WorkerBin, worker)
	c.NoopBin = cmp.Or(c.NoopBin, noop)
	return nil
}

func (m *managed) start(ctx context.Context) error {
	for _, dir := range []string{m.settings.dataDir(), filepath.Join(m.settings.RunDir, "workers"), m.markerDir} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("target: create %s: %w", dir, err)
		}
	}
	if err := m.writeNoopConfig(); err != nil {
		return err
	}
	steps := []func(context.Context) error{
		m.checkBinaries,
		m.preparePostgres,
		m.prepareTLS,
		m.startServer,
		m.authenticate,
		m.createFarm,
		m.startLanes,
		m.startWorkers,
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return err
		}
	}
	return nil
}

// checkBinaries rejects a config missing a binary the run needs.
func (m *managed) checkBinaries(context.Context) error {
	if m.cfg.ServerBin == "" {
		return errors.New("target: ManagedConfig.ServerBin is empty (ResolveBinaries builds it)")
	}
	if _, procs := m.cfg.Scenario.WorkerCounts(); procs > 0 && (m.cfg.WorkerBin == "" || m.cfg.NoopBin == "") {
		return errors.New("target: the scenario has real workers but ManagedConfig.WorkerBin or NoopBin is empty (ResolveBinaries builds them)")
	}
	return nil
}

// preparePostgres decides the run's database, creating it through an
// administrative DSN.
func (m *managed) preparePostgres(ctx context.Context) error {
	if m.settings.Spec.Store != "postgres" {
		return nil
	}
	admin, err := isAdminDSN(m.cfg.PostgresDSN)
	if err != nil {
		return err
	}
	if !admin {
		// The harness uses this database as it is and leaves the run in it,
		// so it must be one made for the run: a server's own database would
		// be migrated forward and filled with the load farm.
		n, err := publicTables(ctx, m.cfg.PostgresDSN)
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("target: PostgreSQL database %s is not empty (%d tables in its public schema); give an empty database, or a DSN naming the postgres database for a throwaway one",
				pgdsn.Redact(m.cfg.PostgresDSN), n)
		}
		m.pgDSN = m.cfg.PostgresDSN
		return nil
	}
	name := databaseName(m.runID)
	dsn, err := withDatabase(m.cfg.PostgresDSN, name)
	if err != nil {
		return err
	}
	if err := createDatabase(ctx, m.cfg.PostgresDSN, name); err != nil {
		return err
	}
	m.pgAdminDSN, m.pgDB, m.pgDSN = m.cfg.PostgresDSN, name, dsn
	m.logger.InfoContext(ctx, "created the run's PostgreSQL database", slog.String("database", name))
	return nil
}

// prepareTLS generates a farm CA and a server certificate for 127.0.0.1 with
// sqi-server tls init.
func (m *managed) prepareTLS(ctx context.Context) error {
	if !m.settings.Spec.TLS {
		return nil
	}
	dir := m.settings.tlsDir()
	if _, err := m.runCLI(ctx, "tls", "init", "--out", dir, "--host", "127.0.0.1", "--host", "localhost"); err != nil {
		return err
	}
	m.tlsFiles = simworker.TLSFiles{Enabled: "true", CAFile: filepath.Join(dir, "ca.crt")}
	return nil
}

// startServer writes server.yaml and starts sqi-server, retrying once with
// fresh ports when one of the ports it was given was taken in the meantime.
func (m *managed) startServer(ctx context.Context) error {
	if m.settings.Spec.HTTPAuth {
		pw, err := randomHex(24)
		if err != nil {
			return err
		}
		m.bootstrapPassword = pw
	}
	for attempt := 1; ; attempt++ {
		if err := m.writeConfig(); err != nil {
			return err
		}
		if attempt == 1 && m.settings.Spec.BrokerAuth {
			if err := m.issueJoinToken(ctx); err != nil {
				return err
			}
		}
		err := m.spawnServer(ctx)
		if err == nil {
			return nil
		}
		if attempt > 1 || !m.serverExited() || !isBindError(tailLines(m.serverLog(), logTail)) {
			return err
		}
		m.logger.WarnContext(ctx, "sqi-server could not bind its ports; retrying with new ones", slog.Any("error", err))
	}
}

func (m *managed) serverLog() string { return filepath.Join(m.settings.RunDir, "server.log") }

func (m *managed) serverExited() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.server != nil && m.server.exited()
}

// writeConfig picks two free loopback ports and writes server.yaml.
func (m *managed) writeConfig() error {
	ports, err := freePorts(2)
	if err != nil {
		return err
	}
	m.settings.HTTPAddr = "127.0.0.1:" + strconv.Itoa(ports[0])
	m.settings.NATSAddr = "127.0.0.1:" + strconv.Itoa(ports[1])
	scheme := "http://"
	if m.settings.Spec.TLS {
		scheme = "https://"
	}
	m.httpURL = scheme + m.settings.HTTPAddr
	m.natsURL = "nats://" + m.settings.NATSAddr
	data, err := renderServerConfig(m.settings)
	if err != nil {
		return err
	}
	if err := os.WriteFile(m.configPath, data, 0o600); err != nil {
		return fmt.Errorf("target: write server.yaml: %w", err)
	}
	return nil
}

// issueJoinToken creates the database and mints a reusable join token with
// the server's own CLI, before the server starts (SQLite allows one server
// process at a time). The token goes to a file, as a worker reads it.
func (m *managed) issueJoinToken(ctx context.Context) error {
	if _, err := m.runCLI(ctx, "-c", m.configPath, "migrate", "up"); err != nil {
		return err
	}
	out, err := m.runCLI(ctx, "-c", m.configPath, "worker", "token", "issue", "--ttl", joinTokenTTL, "--name", "sqi-load")
	if err != nil {
		return err
	}
	m.joinToken = strings.TrimSpace(out)
	if m.joinToken == "" {
		return errors.New("target: sqi-server worker token issue printed no token")
	}
	if err := os.WriteFile(m.joinTokenFile(), []byte(m.joinToken), 0o600); err != nil {
		return fmt.Errorf("target: write join token: %w", err)
	}
	return nil
}

func (m *managed) joinTokenFile() string { return filepath.Join(m.settings.RunDir, "join-token") }

// serverEnv is the server's environment: the harness's own without any SQI_
// variable, plus the secrets server.yaml leaves out.
func (m *managed) serverEnv() []string {
	var extra []string
	if m.bootstrapPassword != "" {
		extra = append(extra, "SQI_AUTH_BOOTSTRAP_PASSWORD="+m.bootstrapPassword)
	}
	if m.pgDSN != "" {
		extra = append(extra, "SQI_STORE_POSTGRES_DSN="+m.pgDSN)
	}
	return childEnv(os.Environ(), extra...)
}

// runCLI runs an sqi-server subcommand to completion and returns its
// standard output. The error carries standard error only, since standard
// output can hold a secret.
func (m *managed) runCLI(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.cfg.ServerBin, args...) //nolint:gosec // G204: the binary the harness built or was given
	cmd.Env = m.serverEnv()
	cmd.Dir = m.settings.RunDir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("target: sqi-server %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out), nil
}

// spawnServer starts sqi-server and waits for /readyz.
func (m *managed) spawnServer(ctx context.Context) error {
	if err := m.stopServer(ctx); err != nil { // a previous attempt's
		return err
	}
	p, err := startProc(ctx, procSpec{
		name:    "sqi-server",
		bin:     m.cfg.ServerBin,
		args:    []string{"-c", m.configPath, "serve"},
		env:     m.serverEnv(),
		logPath: m.serverLog(),
	}, m.logger)
	if err != nil {
		return err
	}
	m.mu.Lock()
	closing := m.closing
	if !closing {
		m.server = p
	}
	m.mu.Unlock()
	if closing { // a restart raced Close, which no longer sees this process
		return errors.Join(errClosing, p.stopCtx(ctx, m.stopTimeout))
	}
	m.logger.InfoContext(ctx, "started sqi-server", slog.Int("pid", p.pid()), slog.String("http", m.httpURL), slog.String("nats", m.natsURL))

	probe, err := newClient(m.httpURL, "", m.tlsFiles)
	if err != nil {
		return err
	}
	return m.waitReady(ctx, p, probe)
}

func (m *managed) stopServer(ctx context.Context) error {
	m.mu.Lock()
	p := m.server
	m.server = nil
	m.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.stopCtx(ctx, m.stopTimeout)
}

// waitReady polls /readyz until it answers 200, the server exits, or
// readyTimeout passes.
func (m *managed) waitReady(ctx context.Context, p *proc, probe *client.Client) error {
	deadline := time.Now().Add(m.readyTimeout)
	for {
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := probe.Ready(rctx)
		cancel()
		if err == nil {
			return nil
		}
		if p.exited() {
			return fmt.Errorf("target: sqi-server exited before it was ready (%w); last lines of %s:\n%s",
				p.exitError(), m.serverLog(), tailLines(m.serverLog(), logTail))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("target: sqi-server not ready after %s (%w); last lines of %s:\n%s",
				m.readyTimeout, err, m.serverLog(), tailLines(m.serverLog(), logTail))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("target: waiting for sqi-server: %w", ctx.Err())
		case <-p.done:
		case <-time.After(pollInterval):
		}
	}
}

// authenticate logs in as the bootstrap admin and creates the run's API key
// when the auth gate is on, then builds the target's client.
func (m *managed) authenticate(ctx context.Context) error {
	if m.settings.Spec.HTTPAuth {
		session, err := newClient(m.httpURL, "", m.tlsFiles)
		if err != nil {
			return err
		}
		if _, err := session.Login(ctx, bootstrapUser, m.bootstrapPassword); err != nil {
			return fmt.Errorf("target: log in as the bootstrap admin: %w", err)
		}
		key, err := session.CreateAPIKey(ctx, "sqi-load "+m.runID)
		if err != nil {
			return fmt.Errorf("target: create the run's API key: %w", err)
		}
		m.apiKey = key
	}
	c, err := newClient(m.httpURL, m.apiKey, m.tlsFiles)
	if err != nil {
		return err
	}
	m.client = c
	if m.joinToken != "" {
		m.creds = simworker.JoinTokenCredentials(c, m.joinToken)
	}
	return nil
}

func (m *managed) createFarm(ctx context.Context) error {
	f, err := setupFarm(ctx, m.client, farmName(m.runID), m.cfg.Scenario.Farm, samePoolName, m.logger)
	m.farm = f
	return err
}

// startWorkers starts the real workers and waits until all are online.
func (m *managed) startWorkers(ctx context.Context) error {
	m.addWorkerSlots()
	m.mu.Lock()
	workers := slices.Clone(m.workers)
	m.mu.Unlock()
	for _, w := range workers {
		if err := m.spawnWorker(ctx, w); err != nil {
			return err
		}
	}
	if len(workers) == 0 {
		return nil
	}
	return m.waitWorkersOnline(ctx, len(workers))
}

// spawnWorker starts w's next incarnation. Its data directory, and so its
// worker ID, is the slot's; its log is appended to the slot's.
func (m *managed) spawnWorker(ctx context.Context, w *realWorker) error {
	m.mu.Lock()
	restarts := w.started
	m.mu.Unlock()
	n := w.index + 1 // the directory and log names count from 1
	id := incarnationID(w.index, restarts)
	env, err := m.workerEnv(w.group, n, id)
	if err != nil {
		return err
	}
	// Held across the start, so Close sees the process or it never starts.
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.closing:
		return errClosing
	case w.proc != nil && !w.proc.exited():
		return fmt.Errorf("target: real worker %d is still running; kill or term it before restarting it", w.index)
	case w.started != restarts:
		return fmt.Errorf("target: real worker %d was restarted concurrently", w.index)
	}
	p, err := startProc(ctx, procSpec{
		name:    "sqi-worker " + strconv.Itoa(n) + " (" + id + ")",
		bin:     m.cfg.WorkerBin,
		args:    []string{"start"},
		env:     env,
		logPath: m.workerLog(w.index),
	}, m.logger)
	if err != nil {
		return err
	}
	if w.proc != nil {
		// A process that exited on its own has no end yet; it is not running.
		if inc := &m.incarnations[w.inc]; inc.Ended.IsZero() {
			inc.Ended = time.Now()
		}
		w.retired = append(w.retired, w.proc)
	}
	w.proc = p
	w.started++
	m.incarnations = append(m.incarnations, Incarnation{ID: id, Worker: w.index, Started: time.Now()})
	w.inc = len(m.incarnations) - 1
	return nil
}

// workerLog is real worker i's log, which every incarnation appends to.
func (m *managed) workerLog(i int) string {
	return filepath.Join(m.settings.RunDir, "workers", strconv.Itoa(i+1)+".log")
}

// workerEnv is real worker n's environment, as the integration suite
// configures a real worker, for incarnation inc.
func (m *managed) workerEnv(g scenario.WorkerGroup, n int, inc string) ([]string, error) {
	ports, err := freePorts(1)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(m.settings.RunDir, "workers", strconv.Itoa(n))
	name := g.Name
	if name == "" {
		name = "real"
	}
	env := []string{
		"SQI_WORKER_NATS_URL=" + m.NATSURLFor(g.Name),
		"SQI_WORKER_DISCOVERY_ENABLE_MDNS=false",
		"SQI_WORKER_FARM_ID=" + m.farm.id,
		"SQI_WORKER_NAME=" + name + "-" + strconv.Itoa(n),
		"SQI_WORKER_DATA_DIR=" + dir,
		"SQI_WORKER_SESSION_DIR=" + dir + "-sessions",
		"SQI_WORKER_ALLOW_ROOT=true",
		"SQI_WORKER_LOG_FORMAT=text",
		"SQI_WORKER_LOG_LEVEL=info",
		"SQI_WORKER_SHUTDOWN_GRACE_PERIOD=" + workerGrace,
		"SQI_WORKER_METRICS_ADDR=127.0.0.1:" + strconv.Itoa(ports[0]),
		// The harness's own variables are not SQI_-prefixed: they configure
		// nothing in the worker, and childEnv would strip them from a child
		// of the harness.
		"SQILOAD_INCARNATION=" + inc,
	}
	if m.markerDir != "" {
		env = append(env, "SQILOAD_MARKER_DIR="+m.markerDir)
	}
	if len(g.Queues) > 0 {
		ids := make([]string, len(g.Queues))
		for i, q := range g.Queues {
			id, ok := m.farm.queueIDs[q]
			if !ok {
				return nil, fmt.Errorf("target: real worker group %s names queue %q, which the scenario's farm does not declare", name, q)
			}
			ids[i] = id
		}
		env = append(env, "SQI_WORKER_QUEUE_IDS="+strings.Join(ids, ","))
	}
	if tags := capabilityTags(g.Tags); tags != "" {
		env = append(env, "SQI_WORKER_CAPABILITY_TAGS="+tags)
	}
	if g.Heartbeat.Duration > 0 {
		env = append(env, "SQI_WORKER_HEARTBEAT_INTERVAL="+g.Heartbeat.String())
	}
	if m.joinToken != "" {
		env = append(env, "SQI_WORKER_NATS_JOIN_TOKEN_FILE="+m.joinTokenFile(), "SQI_WORKER_NATS_SERVER_URL="+m.httpURL)
	}
	if m.tlsFiles.CAFile != "" {
		env = append(
			env,
			"SQI_WORKER_NATS_TLS_ENABLED=true",
			"SQI_WORKER_NATS_TLS_CA_FILE="+m.tlsFiles.CAFile,
			"SQI_WORKER_NATS_SERVER_TLS_CA_FILE="+m.tlsFiles.CAFile,
		)
	}
	return childEnv(os.Environ(), env...), nil
}

// capabilityTags renders tags as sqi-worker's comma-separated key=value
// list, in a stable order.
func capabilityTags(tags map[string]string) string {
	keys := slices.Sorted(maps.Keys(tags))
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + tags[k]
	}
	return strings.Join(parts, ",")
}

// waitWorkersOnline polls the farm's workers until n are online, failing fast
// when a worker process exits.
func (m *managed) waitWorkersOnline(ctx context.Context, n int) error {
	deadline := time.Now().Add(workerOnlineTimeout)
	online := 0
	for {
		for i, p := range m.workerProcs() {
			if p != nil && p.exited() {
				log := m.workerLog(i)
				return fmt.Errorf("target: real worker: %w; last lines of %s:\n%s", p.exitError(), log, tailLines(log, logTail))
			}
		}
		page, err := m.client.ListWorkers(ctx, m.farm.id, 1000, 0)
		if err == nil {
			online = 0
			for _, w := range page.Items {
				if w.Status == string(store.WorkerStatusOnline) {
					online++
				}
			}
			if online >= n {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return errors.Join(fmt.Errorf("target: %d of %d real workers online after %s", online, n, workerOnlineTimeout), err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("target: waiting for real workers: %w", ctx.Err())
		case <-time.After(5 * pollInterval):
		}
	}
}

// workerProcs returns each real worker's current process, in start order; an
// entry is nil for a worker not started yet.
func (m *managed) workerProcs() []*proc {
	m.mu.Lock()
	defer m.mu.Unlock()
	procs := make([]*proc, len(m.workers))
	for i, w := range m.workers {
		procs[i] = w.proc
	}
	return procs
}

func (m *managed) HTTPURL() string                    { return m.httpURL }
func (m *managed) NATSURL() string                    { return m.natsURL }
func (m *managed) APIKey() string                     { return m.apiKey }
func (m *managed) FarmID() string                     { return m.farm.id }
func (m *managed) QueueIDs() map[string]string        { return copyMap(m.farm.queueIDs) }
func (m *managed) DataDir() string                    { return m.settings.dataDir() }
func (m *managed) Credentials() simworker.Credentials { return m.creds }
func (m *managed) TLS() simworker.TLSFiles            { return m.tlsFiles }

// NoopCommand is the no-op binary, or a placeholder when the run has no real
// workers and was given no no-op binary. When a job class has real_task its
// first argument names noop.json.
func (m *managed) NoopCommand() scenario.Command {
	if m.cfg.NoopBin == "" {
		return scenario.Command{Path: placeholderCommand}
	}
	cmd := scenario.Command{Path: m.cfg.NoopBin}
	if m.markerDir != "" {
		cmd.Args = []string{marker.ConfigArg(m.noopConfigPath())}
	}
	return cmd
}

func (m *managed) noopConfigPath() string { return filepath.Join(m.settings.RunDir, "noop.json") }

// writeNoopConfig writes noop.json, which tells the no-op command where the
// markers go and how each class with real_task behaves, when there is one.
func (m *managed) writeNoopConfig() error {
	if m.markerDir == "" {
		return nil
	}
	cfg := marker.NoopConfig{MarkerDir: m.markerDir, Classes: map[string]marker.NoopClass{}}
	for _, j := range m.cfg.Scenario.Jobs {
		if rt := j.RealTask; rt != nil {
			cfg.Classes[j.Name] = marker.NoopClass{
				DurationMS:  int(rt.Duration.Milliseconds()),
				LogEveryMS:  int(rt.LogEvery.Milliseconds()),
				ChattyChild: rt.ChattyChild,
				SilentChild: rt.SilentChild,
			}
		}
	}
	if err := marker.WriteConfig(m.noopConfigPath(), cfg); err != nil {
		return fmt.Errorf("target: %w", err)
	}
	return nil
}

// PoolName is name itself: the server is the run's own, so its usage pools
// take the scenario's names.
func (*managed) PoolName(name string) string { return name }

func (m *managed) PostgresDSN() string { return m.pgDSN }

// StoreLabel is the scenario's store, which the harness configured.
func (m *managed) StoreLabel() string { return m.settings.Spec.Store }

// PIDs returns the process IDs of the processes the target is running, the
// server first and then its real workers. Call it while the target runs:
// Close forgets the server it stopped. It is not part of Target; the tests
// use it to check that no process outlives its run.
func (m *managed) PIDs() []int {
	var pids []int
	if pid := m.serverPID(); pid != 0 {
		pids = append(pids, pid)
	}
	return append(pids, m.workerPIDs()...)
}

func (m *managed) serverPID() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.server == nil {
		return 0
	}
	return m.server.pid()
}

// workerPIDs returns the current process IDs of the real workers started.
func (m *managed) workerPIDs() []int {
	var pids []int
	for _, p := range m.workerProcs() {
		if p != nil {
			pids = append(pids, p.pid())
		}
	}
	return pids
}

// Close first continues every task group and process the harness stalled,
// then stops the real workers (together), then ends the task process groups
// the markers still show running, then stops the server: each process gets
// an interrupt and the stop grace (30 s by default) to exit before it is
// killed, and a done ctx kills at once. It then drops a database the harness
// created and closes the broker lanes. Every process is reaped before Close
// returns, whatever the error, and nothing is started once it has begun.
func (m *managed) Close(ctx context.Context) error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closing = true
		m.mu.Unlock()
		m.resumeAll(ctx)
		errs := m.stopWorkers(ctx, true)
		errs = append(errs, m.endTaskGroups(ctx), m.stopServer(ctx))
		if m.pgDB != "" {
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cliTimeout)
			if err := dropDatabase(dctx, m.pgAdminDSN, m.pgDB); err != nil {
				errs = append(errs, err)
			}
			cancel()
		}
		errs = append(errs, m.closeLanes())
		m.closeErr = errors.Join(errs...)
	})
	return m.closeErr
}

// freePorts returns n distinct loopback ports the OS reported free. Another
// process can take one before the server binds it; startServer retries once.
func freePorts(n int) ([]int, error) {
	var lc net.ListenConfig
	listeners := make([]net.Listener, 0, n)
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	ports := make([]int, 0, n)
	for range n {
		// Held open until all are drawn, so the n ports differ.
		l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("target: find a free port: %w", err)
		}
		listeners = append(listeners, l)
		addr, ok := l.Addr().(*net.TCPAddr)
		if !ok {
			return nil, fmt.Errorf("target: unexpected listener address %T", l.Addr())
		}
		ports = append(ports, addr.Port)
	}
	return ports, nil
}

// isBindError reports whether a server log shows a listener failing to bind
// its address.
func isBindError(log string) bool {
	l := strings.ToLower(log)
	return strings.Contains(l, "bind:") || strings.Contains(l, "address already in use")
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("target: random: %w", err)
	}
	return hex.EncodeToString(b), nil
}
