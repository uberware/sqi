// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/dbfault"
	"github.com/uberware/sqi/internal/loadtest/marker"
	"github.com/uberware/sqi/internal/loadtest/netfault"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/store/backend"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

// Faults is what a fault controller drives. Only the managed target has it.
type Faults interface {
	// KillServer kills sqi-server alone (SIGKILL, or TerminateProcess on
	// Windows) and returns once it is reaped.
	KillServer(ctx context.Context) error
	// TermServer stops sqi-server gracefully, the way Close does.
	TermServer(ctx context.Context) error
	// StallServer stops sqi-server with SIGSTOP (POSIX only).
	StallServer() error
	// ResumeServer continues a stalled sqi-server with SIGCONT.
	ResumeServer() error
	// RestartServer starts sqi-server again with the same server.yaml, so on
	// the same ports, and waits for /readyz. The server must not be running.
	RestartServer(ctx context.Context) error
	// RealWorkers returns the indexes (0-based, in start order) of the
	// group's real workers.
	RealWorkers(group string) []int
	// KillWorker kills real worker i's process alone, leaving the tasks it
	// started running, and returns once it is reaped.
	KillWorker(ctx context.Context, i int) error
	// TermWorker stops real worker i gracefully.
	TermWorker(ctx context.Context, i int) error
	// StallWorker stops real worker i's process with SIGSTOP (POSIX only),
	// then the process group of every task its current incarnation is
	// running, as the markers show them, so a sleeping machine's renders
	// stop too.
	StallWorker(i int) error
	// ResumeWorker continues what StallWorker stopped, in reverse order of
	// the stops: the tasks' groups first, then the worker.
	ResumeWorker(i int) error
	// RestartWorker starts real worker i again as a new process, a new
	// incarnation, on the same data directory, so with the same worker ID.
	// The worker must not be running: if it is, the error wraps
	// [ErrWorkerRunning].
	RestartWorker(ctx context.Context, i int) error
	// RealLive is the number of real worker processes running now.
	RealLive() int
	// Incarnations lists every real worker process the run started, in start
	// order.
	Incarnations() []Incarnation
	// Lane is the fault proxy between the group's workers and the broker,
	// nil when the scenario has no network fault.
	Lane(group string) Lane
	// MarkerDir is where real tasks write their markers, "" when no job class
	// has real_task.
	MarkerDir() string
	// StopForChecks stops the real workers, then the server, gracefully,
	// keeping the data directory and the database for the checks.
	StopForChecks(ctx context.Context) error
	// StoreOptions says how to open the run's store once the server is
	// stopped.
	StoreOptions() backend.Options
	// StallDatabase stalls the store: on SQLite an outside connection takes
	// the write lock, on PostgreSQL the proxy in front of the server's
	// connections forwards nothing.
	StallDatabase(ctx context.Context) error
	// DropDatabase cuts the server's PostgreSQL connections and refuses new
	// ones. PostgreSQL only: on SQLite it is an error.
	DropDatabase(ctx context.Context) error
	// HealDatabase releases the lock or heals the proxy. It does nothing when
	// no database fault is applied.
	HealDatabase(ctx context.Context) error
	// FillVolume fills the volume "server" or "workers" with ballast, leaving
	// headroom bytes free and, with inodes, no inodes. It is an error when the
	// target has no such volume. Filling a filled volume is a no-op.
	FillVolume(volume string, headroom int64, inodes bool) error
	// FreeVolume removes the volume's ballast. It does nothing when the volume
	// is not filled and is an error when the target has no such volume.
	FreeVolume(volume string) error
	// LimitServerFDs sets the running sqi-server's soft RLIMIT_NOFILE to the number
	// of files it has open plus headroom, leaving the hard limit, and returns the
	// open-file count it saw. Limiting a limited server is a no-op that returns
	// the first count. Linux only; elsewhere, with a server running, it returns
	// errFDLimitUnsupported.
	LimitServerFDs(headroom int) (open int, err error)
	// RestoreServerFDs puts back the soft limit LimitServerFDs replaced. It does
	// nothing when none is applied or the server is gone.
	RestoreServerFDs() error
	// ServerReady probes the server's /readyz: "ready", or the probe's error
	// text.
	ServerReady(ctx context.Context) (status string)
	// RealWorkerID is real worker i's worker ID, read from its data directory's
	// worker.id. It is an error until the worker has started once.
	RealWorkerID(i int) (string, error)
	// RevokeWorker removes real worker i's join-token file, then revokes its
	// credential through the REST API, and returns its worker ID. Without the
	// file a restarted worker cannot enroll again. The run must have broker
	// authentication.
	RevokeWorker(ctx context.Context, i int) (workerID string, err error)
	// ServerLog is the path of the server's log, which every incarnation
	// appends to.
	ServerLog() string
}

// Lane is a worker group's connection to the broker, which a fault can cut,
// blackhole or shape. *netfault.Proxy implements it.
type Lane interface {
	Cut()
	Blackhole()
	Shape(delay, jitter time.Duration, bytesPerSec int)
	Heal()
}

// Incarnation is one real worker process.
type Incarnation struct {
	// ID is "<worker index>.<restart count>", e.g. "0.0", "0.1". Each real
	// worker process gets it as SQILOAD_INCARNATION.
	ID      string
	Worker  int
	Started time.Time
	// Ended is set when the harness kills or stops the process, or starts the
	// worker's next incarnation; until then it is zero, also for a process
	// that exited on its own.
	Ended time.Time
}

// ErrWorkerRunning is wrapped by the error of a RestartWorker call on a real
// worker whose process is still running.
var ErrWorkerRunning = errors.New("target: real worker is still running")

var (
	_ Faults = (*managed)(nil)
	_ Lane   = (*netfault.Proxy)(nil)
)

// errStallUnsupported is what stalling returns on Windows.
var errStallUnsupported = errors.New("target: stall needs SIGSTOP, which Windows does not have")

// errClosing refuses a restart once Close has begun.
var errClosing = errors.New("target: the target is closing")

// errNoDatabaseProxy is what a database fault returns on a PostgreSQL run whose
// scenario declared none, so no proxy fronts the server's connections.
var errNoDatabaseProxy = errors.New("target: the run has no database proxy; declare a database fault in the scenario")

// savedFDLimit is the limit LimitServerFDs replaced, on the process it
// replaced it on, and the open-file count it saw.
type savedFDLimit struct {
	pid  int
	old  fdLimit
	open int
}

// errUnsafeGroup refuses a group signal to a group id that is not safe to
// signal, such as one read from a corrupt marker.
var errUnsafeGroup = errors.New("target: refusing to signal process group")

// realWorker is one real worker slot, which its incarnations take in turn.
type realWorker struct {
	index int // 0-based, in start order
	group scenario.WorkerGroup
	// proc is the current incarnation's process, nil before the first start.
	proc *proc
	// started counts the incarnations started; the next one's restart count.
	started int
	// inc indexes the current incarnation in managed.incarnations.
	inc int
	// retired are the earlier incarnations' processes. Close releases them,
	// which ends what they left running (their job on Windows, their process
	// group elsewhere).
	retired []*proc
	// taskGroups are the process groups of the worker's tasks that
	// StallWorker stopped and nothing has continued yet, in stop order.
	taskGroups []int
}

// taskStaleAfter is how long a live exec's marker can go without a refresh
// (every marker.RefreshInterval) before it counts as gone: a task killed by a
// signal records no end.
const taskStaleAfter = 2 * time.Second

// taskSettle is how long StallWorker waits before looking for the worker's
// tasks a second time, for a task the worker started just before it was
// stopped that had not written its first marker yet.
const taskSettle = 300 * time.Millisecond

func incarnationID(worker, restarts int) string {
	return strconv.Itoa(worker) + "." + strconv.Itoa(restarts)
}

// laneSeed is the jitter seed of the lane of worker group i: i+1, mixed with
// the run seed when there is one.
func laneSeed(runSeed uint64, i int) uint64 {
	s := uint64(i) + 1 //nolint:gosec // G115: a group index is never negative
	if runSeed == 0 {
		return s
	}
	return s ^ (runSeed * 0x9e3779b97f4a7c15)
}

// startLanes starts one fault proxy in front of the broker per worker group,
// when the scenario has a network fault.
func (m *managed) startLanes(ctx context.Context) error {
	if !m.cfg.Scenario.HasNetworkFault() {
		return nil
	}
	m.lanes = map[string]*netfault.Proxy{}
	for i, g := range m.cfg.Scenario.Workers {
		if _, ok := m.lanes[g.Name]; ok {
			continue
		}
		p, err := netfault.Listen(m.settings.NATSAddr, laneSeed(m.cfg.Seed, i))
		if err != nil {
			return fmt.Errorf("target: start the broker lane of worker group %s: %w", g.Name, err)
		}
		m.lanes[g.Name] = p
		m.logger.InfoContext(ctx, "started broker lane", slog.String("group", g.Name), slog.String("addr", p.Addr()))
	}
	return nil
}

func (m *managed) closeLanes() error {
	var errs []error
	if m.dbProxy != nil {
		if err := m.dbProxy.Close(); err != nil {
			errs = append(errs, fmt.Errorf("target: close the database proxy: %w", err))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(m.lanes)) {
		if err := m.lanes[name].Close(); err != nil {
			errs = append(errs, fmt.Errorf("target: close the broker lane of worker group %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// Lane returns the group's lane, or an untyped nil.
func (m *managed) Lane(group string) Lane {
	p, ok := m.lanes[group]
	if !ok {
		return nil
	}
	return p
}

// NATSURLFor is the broker URL the group's workers dial: its lane's, or the
// broker's own when the group has no lane.
func (m *managed) NATSURLFor(group string) string {
	if p, ok := m.lanes[group]; ok {
		return "nats://" + p.Addr()
	}
	return m.natsURL
}

func (m *managed) MarkerDir() string { return m.markerDir }

// StoreOptions opens the run's database without migrating it.
func (m *managed) StoreOptions() backend.Options {
	if m.settings.Spec.Store == backend.DriverPostgres {
		return backend.Options{Driver: backend.DriverPostgres, PostgresDSN: m.pgDSN, Logger: m.logger}
	}
	return backend.Options{Driver: backend.DriverSQLite, SQLitePath: m.settings.sqlitePath()}
}

// StallDatabase takes the SQLite write lock from outside the server, or
// blackholes the PostgreSQL proxy. Stalling an already stalled database is a
// no-op.
func (m *managed) StallDatabase(ctx context.Context) error {
	m.dbMu.Lock()
	defer m.dbMu.Unlock()
	if m.isClosing() {
		return errClosing
	}
	if m.settings.Spec.Store == backend.DriverPostgres {
		if m.dbProxy == nil {
			return errNoDatabaseProxy
		}
		m.logger.InfoContext(ctx, "stalling the database")
		m.dbProxy.Blackhole()
		return nil
	}
	if m.dbLock != nil {
		return nil
	}
	m.logger.InfoContext(ctx, "stalling the database: taking the SQLite write lock")
	l, err := dbfault.LockSQLite(ctx, m.settings.sqlitePath())
	if err != nil {
		return fmt.Errorf("target: stall the database: %w", err)
	}
	m.dbLock = l
	return nil
}

// DropDatabase cuts the PostgreSQL proxy.
func (m *managed) DropDatabase(ctx context.Context) error {
	m.dbMu.Lock()
	defer m.dbMu.Unlock()
	if m.isClosing() {
		return errClosing
	}
	if m.settings.Spec.Store != backend.DriverPostgres {
		return errors.New("target: dropping the database's connections needs PostgreSQL; SQLite has none to drop")
	}
	if m.dbProxy == nil {
		return errNoDatabaseProxy
	}
	m.logger.InfoContext(ctx, "dropping the database connections")
	m.dbProxy.Cut()
	return nil
}

// HealDatabase releases the SQLite lock and heals the PostgreSQL proxy,
// whichever is applied.
func (m *managed) HealDatabase(ctx context.Context) error {
	m.dbMu.Lock()
	defer m.dbMu.Unlock()
	var err error
	if m.dbLock != nil {
		m.logger.InfoContext(ctx, "healing the database: releasing the SQLite write lock")
		err = m.dbLock.Release()
		m.dbLock = nil
	}
	if m.dbProxy != nil {
		m.dbProxy.Heal()
	}
	if err != nil {
		return fmt.Errorf("target: heal the database: %w", err)
	}
	return nil
}

// volumeDir is the directory FillVolume fills: the one the volume's data lives
// in. It is an error when the target has no such volume.
func (m *managed) volumeDir(volume string) (string, error) {
	switch {
	case volume == scenario.VolumeServer && m.settings.Volume != "":
		return m.settings.dataDir(), nil
	case volume == scenario.VolumeWorkers && m.workerVolume != "":
		return filepath.Join(m.workerVolume, m.runID), nil
	case volume == scenario.VolumeServer:
		return "", errors.New("target: the run has no server volume; set ManagedConfig.ServerVolume")
	case volume == scenario.VolumeWorkers:
		return "", errors.New("target: the run has no workers volume; set ManagedConfig.WorkerVolume")
	}
	return "", fmt.Errorf("target: unknown volume %q; want %q or %q", volume, scenario.VolumeServer, scenario.VolumeWorkers)
}

// FillVolume writes the ballast. A failed fill leaves nothing behind (Fill
// removes what it wrote), so no fault stays applied after an error.
func (m *managed) FillVolume(volume string, headroom int64, inodes bool) error {
	dir, err := m.volumeDir(volume)
	if err != nil {
		return err
	}
	m.volMu.Lock()
	defer m.volMu.Unlock()
	if m.isClosing() {
		return errClosing
	}
	if _, ok := m.ballast[volume]; ok {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("target: fill the %s volume: %w", volume, err)
	}
	m.logger.InfoContext(context.Background(), "filling a volume", slog.String("volume", volume), slog.String("dir", dir), slog.Int64("headroom", headroom), slog.Bool("inodes", inodes))
	b, err := m.fill(dir, headroom, inodes)
	if err != nil {
		return fmt.Errorf("target: fill the %s volume: %w", volume, err)
	}
	m.ballast[volume] = b
	return nil
}

// FreeVolume removes the volume's ballast. When removing fails the ballast is
// kept, so Close tries again.
func (m *managed) FreeVolume(volume string) error {
	if _, err := m.volumeDir(volume); err != nil {
		return err
	}
	m.volMu.Lock()
	defer m.volMu.Unlock()
	b, ok := m.ballast[volume]
	if !ok {
		return nil
	}
	m.logger.InfoContext(context.Background(), "freeing a volume", slog.String("volume", volume))
	if err := b.Free(); err != nil {
		return fmt.Errorf("target: free the %s volume: %w", volume, err)
	}
	delete(m.ballast, volume)
	return nil
}

// freeVolumes frees every volume still filled; Close calls it once the server
// and the workers are stopped.
func (m *managed) freeVolumes(ctx context.Context) error {
	m.volMu.Lock()
	defer m.volMu.Unlock()
	var errs []error
	for _, v := range slices.Sorted(maps.Keys(m.ballast)) {
		m.logger.InfoContext(ctx, "freeing a volume", slog.String("volume", v))
		if err := m.ballast[v].Free(); err != nil {
			errs = append(errs, fmt.Errorf("target: free the %s volume: %w", v, err))
			continue
		}
		delete(m.ballast, v)
	}
	return errors.Join(errs...)
}

// LimitServerFDs lowers the server's soft open-file limit to what it has open
// plus headroom. The limit is put back by RestoreServerFDs, StopForChecks and
// Close.
func (m *managed) LimitServerFDs(headroom int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.closing:
		return 0, errClosing
	case m.server == nil:
		return 0, errors.New("target: no sqi-server has been started")
	case m.server.exited():
		return 0, errors.New("target: sqi-server is not running")
	}
	pid := m.server.pid()
	if s := m.fdSaved; s != nil && s.pid == pid {
		return s.open, nil
	}
	m.logger.InfoContext(context.Background(), "limiting the server's open files", slog.Int("pid", pid), slog.Int("headroom", headroom))
	open, old, err := limitFDs(pid, headroom)
	if err != nil {
		return 0, err
	}
	m.fdSaved = &savedFDLimit{pid: pid, old: old, open: open}
	return open, nil
}

// RestoreServerFDs puts back the limit LimitServerFDs replaced. With nothing
// limited, which is always so off Linux, it does nothing. A server that is
// gone has nothing to restore either. When putting the limit back fails it
// stays saved, so the next call tries again.
func (m *managed) RestoreServerFDs() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.fdSaved
	if s == nil {
		return nil
	}
	m.logger.InfoContext(context.Background(), "restoring the server's open-file limit", slog.Int("pid", s.pid))
	if err := restoreFDs(s.pid, s.old); err != nil {
		return err
	}
	m.fdSaved = nil
	return nil
}

// readyProbeTimeout bounds one ServerReady probe.
const readyProbeTimeout = 2 * time.Second

// ServerReady asks the server's /readyz once.
func (m *managed) ServerReady(ctx context.Context) string {
	if m.httpURL == "" {
		return "no sqi-server has been started"
	}
	probe, err := m.probeClient()
	if err != nil {
		return err.Error()
	}
	ctx, cancel := context.WithTimeout(ctx, readyProbeTimeout)
	defer cancel()
	if err := probe.Ready(ctx); err != nil {
		return err.Error()
	}
	return "ready"
}

// probeClient is the client ServerReady probes with, built once.
func (m *managed) probeClient() (*client.Client, error) {
	m.probeMu.Lock()
	defer m.probeMu.Unlock()
	if m.readyProbe == nil {
		c, err := newClient(m.httpURL, "", m.tlsFiles)
		if err != nil {
			return nil, err
		}
		m.readyProbe = c
	}
	return m.readyProbe, nil
}

// ServerLog is the path of server.log.
func (m *managed) ServerLog() string { return m.serverLog() }

// RealWorkerID reads real worker i's ID from its data directory.
func (m *managed) RealWorkerID(i int) (string, error) {
	m.mu.Lock()
	n := len(m.workers)
	m.mu.Unlock()
	if i < 0 || i >= n {
		return "", fmt.Errorf("target: no real worker %d (the run has %d)", i, n)
	}
	path := workerconfig.WorkerIDFilePath(m.workerDir(i + 1))
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("target: real worker %d has not started: it has no %s", i, path)
	}
	if err != nil {
		return "", fmt.Errorf("target: read the ID of real worker %d: %w", i, err)
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", fmt.Errorf("target: %s is empty", path)
	}
	return id, nil
}

// RevokeWorker takes the join token away before it revokes, so the worker cannot
// enroll again in the gap between the two. The error of a failed REST call
// comes with the ID, and the token stays gone.
func (m *managed) RevokeWorker(ctx context.Context, i int) (string, error) {
	if m.isClosing() {
		return "", errClosing
	}
	if m.joinToken == "" {
		return "", errors.New("target: the run has no broker authentication, so there is no credential to revoke")
	}
	if m.client == nil {
		return "", errors.New("target: the target has no REST client yet")
	}
	id, err := m.RealWorkerID(i)
	if err != nil {
		return "", err
	}
	if err := os.Remove(m.workerJoinTokenFile(i + 1)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return id, fmt.Errorf("target: remove the join token of real worker %d: %w", i, err)
	}
	m.logger.InfoContext(ctx, "revoking a real worker's credential", slog.Int("worker", i), slog.String("worker_id", id))
	if err := m.client.RevokeWorkerCredential(ctx, id); err != nil {
		return id, fmt.Errorf("target: revoke the credential of real worker %d (%s): %w", i, id, err)
	}
	return id, nil
}

func (m *managed) isClosing() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closing
}

func (m *managed) currentServer() (*proc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.server == nil {
		return nil, errors.New("target: no sqi-server has been started")
	}
	return m.server, nil
}

func (m *managed) KillServer(ctx context.Context) error {
	p, err := m.currentServer()
	if err != nil {
		return err
	}
	m.logger.InfoContext(ctx, "killing sqi-server", slog.Int("pid", p.pid()))
	return p.killOne(ctx)
}

func (m *managed) TermServer(ctx context.Context) error {
	p, err := m.currentServer()
	if err != nil {
		return err
	}
	m.logger.InfoContext(ctx, "stopping sqi-server", slog.Int("pid", p.pid()))
	return p.stopCtx(ctx, m.stopTimeout)
}

func (m *managed) StallServer() error {
	p, err := m.currentServer()
	if err != nil {
		return err
	}
	return p.stall()
}

func (m *managed) ResumeServer() error {
	p, err := m.currentServer()
	if err != nil {
		return err
	}
	return p.resume()
}

// RestartServer never writes server.yaml again, so the server binds the
// addresses it had; when one is taken the error names it.
func (m *managed) RestartServer(ctx context.Context) error {
	m.mu.Lock()
	closing, running := m.closing, m.server != nil && !m.server.exited()
	m.mu.Unlock()
	switch {
	case closing:
		return errClosing
	case running:
		return errors.New("target: sqi-server is still running; kill or term it before restarting it")
	}
	err := m.spawnServer(ctx)
	if err != nil && isBindError(tailLines(m.serverLog(), logTail)) {
		return fmt.Errorf("target: restarted sqi-server could not bind its addresses (http %s, nats %s), which a restart keeps: %w",
			m.settings.HTTPAddr, m.settings.NATSAddr, err)
	}
	return err
}

// addWorkerSlots makes the real worker slots of the scenario, once.
func (m *managed) addWorkerSlots() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workers != nil {
		return
	}
	m.workers = []*realWorker{}
	for _, g := range m.cfg.Scenario.Workers {
		for range g.Real {
			m.workers = append(m.workers, &realWorker{index: len(m.workers), group: g, inc: -1})
		}
	}
}

func (m *managed) RealWorkers(group string) []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []int
	for _, w := range m.workers {
		if w.group.Name == group {
			out = append(out, w.index)
		}
	}
	return out
}

// workerProc returns real worker i's current process.
func (m *managed) workerProc(i int) (*proc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.workers) {
		return nil, fmt.Errorf("target: no real worker %d (the run has %d)", i, len(m.workers))
	}
	if m.workers[i].proc == nil {
		return nil, fmt.Errorf("target: real worker %d has not been started", i)
	}
	return m.workers[i].proc, nil
}

// markEnded records that the harness ended real worker i's incarnation p,
// unless that was already recorded. It does nothing when p is no longer the
// worker's current process: spawnWorker recorded its end when it replaced it.
func (m *managed) markEnded(i int, p *proc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.workers[i]
	if w.proc != p || w.inc < 0 {
		return
	}
	if m.incarnations[w.inc].Ended.IsZero() {
		m.incarnations[w.inc].Ended = time.Now()
	}
}

func (m *managed) KillWorker(ctx context.Context, i int) error {
	p, err := m.workerProc(i)
	if err != nil {
		return err
	}
	m.logger.InfoContext(ctx, "killing real worker", slog.Int("worker", i), slog.Int("pid", p.pid()))
	err = p.killOne(ctx)
	if p.exited() {
		m.markEnded(i, p)
	}
	return err
}

// TermWorker leaves the worker's supervision in place, as KillWorker does, so
// what it left running (in its job on Windows, its process group elsewhere)
// is ended only when the target closes.
func (m *managed) TermWorker(ctx context.Context, i int) error {
	p, err := m.workerProc(i)
	if err != nil {
		return err
	}
	m.logger.InfoContext(ctx, "stopping real worker", slog.Int("worker", i), slog.Int("pid", p.pid()))
	err = p.stop(ctx, m.stopTimeout)
	if p.exited() {
		m.markEnded(i, p)
	}
	return err
}

func (m *managed) StallWorker(i int) error {
	p, err := m.workerProc(i)
	if err != nil {
		return err
	}
	if err := p.stall(); err != nil {
		return err
	}
	if m.markerDir == "" {
		return nil
	}
	inc := m.currentIncarnation(i)
	err = m.stopTaskGroups(i, inc)
	time.Sleep(taskSettle)
	return errors.Join(err, m.stopTaskGroups(i, inc))
}

func (m *managed) ResumeWorker(i int) error {
	p, err := m.workerProc(i)
	if err != nil {
		return err
	}
	_, err = m.contTaskGroups(i)
	return errors.Join(err, p.resume())
}

// currentIncarnation is the ID of real worker i's current incarnation, which
// has started.
func (m *managed) currentIncarnation(i int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.incarnations[m.workers[i].inc].ID
}

// liveTaskGroups returns the process groups of the execs the markers show
// running: no end, refreshed within taskStaleAfter, and, unless inc is "",
// of incarnation inc. Each group appears once, in marker order.
func (m *managed) liveTaskGroups(inc string) ([]int, error) {
	execs, _, err := marker.ReadAll(m.markerDir)
	if err != nil {
		return nil, fmt.Errorf("target: read the task markers: %w", err)
	}
	now := time.Now()
	var groups []int
	for _, e := range execs {
		live := e.End == nil && safeGroup(e.PGID) && now.Sub(e.LastSeen) <= taskStaleAfter
		if live && (inc == "" || e.Incarnation == inc) && !slices.Contains(groups, e.PGID) {
			groups = append(groups, e.PGID)
		}
	}
	return groups, nil
}

// stopTaskGroups stops the process group of every running task of
// incarnation inc of real worker i not stopped yet, and records it.
func (m *managed) stopTaskGroups(i int, inc string) error {
	groups, err := m.liveTaskGroups(inc)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.workers[i]
	var errs []error
	for _, g := range groups {
		if slices.Contains(w.taskGroups, g) {
			continue
		}
		if err := stopGroup(g); err != nil {
			if !groupGone(err) {
				errs = append(errs, fmt.Errorf("target: stop task process group %d of real worker %d: %w", g, i, err))
			}
			continue
		}
		w.taskGroups = append(w.taskGroups, g)
	}
	return errors.Join(errs...)
}

// contTaskGroups continues the task groups StallWorker stopped for real
// worker i, in reverse order of the stops, forgets them and returns them.
func (m *managed) contTaskGroups(i int) ([]int, error) {
	m.mu.Lock()
	w := m.workers[i]
	groups := w.taskGroups
	w.taskGroups = nil
	m.mu.Unlock()
	var errs []error
	for _, g := range slices.Backward(groups) {
		if err := contGroup(g); err != nil && !groupGone(err) {
			errs = append(errs, fmt.Errorf("target: continue task process group %d of real worker %d: %w", g, i, err))
		}
	}
	return groups, errors.Join(errs...)
}

// endTaskGroups kills the process group of every task the markers show still
// running, whichever incarnation started it: what a killed or stopped worker
// left behind. Close calls it once the workers are stopped. The groups
// resumeAll continued are ended too, whatever their markers say: the markers
// went stale while the groups were stopped and may not be refreshed yet.
func (m *managed) endTaskGroups(ctx context.Context) error {
	if m.markerDir == "" {
		return nil
	}
	groups, err := m.liveTaskGroups("")
	m.mu.Lock()
	continued := slices.Clone(m.resumedGroups)
	m.mu.Unlock()
	for _, g := range continued {
		if !slices.Contains(groups, g) {
			groups = append(groups, g)
		}
	}
	for _, g := range groups {
		m.logger.InfoContext(ctx, "ending a task process group a worker left running", slog.Int("pgid", g))
		if err := killGroup(g); err != nil && !groupGone(err) {
			m.logger.WarnContext(ctx, "could not end a task process group", slog.Int("pgid", g), slog.Any("error", err))
		}
	}
	return err
}

func (m *managed) RestartWorker(ctx context.Context, i int) error {
	m.mu.Lock()
	if i < 0 || i >= len(m.workers) {
		n := len(m.workers)
		m.mu.Unlock()
		return fmt.Errorf("target: no real worker %d (the run has %d)", i, n)
	}
	w := m.workers[i]
	m.mu.Unlock()
	return m.spawnWorker(ctx, w)
}

func (m *managed) RealLive() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, w := range m.workers {
		if w.proc != nil && !w.proc.exited() {
			n++
		}
	}
	return n
}

func (m *managed) Incarnations() []Incarnation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.incarnations)
}

// StopForChecks puts back the server's open-file limit, then stops the real
// workers together and the server, each gracefully. The data directory, the
// database and the lanes stay; Close releases them.
func (m *managed) StopForChecks(ctx context.Context) error {
	errs := []error{m.RestoreServerFDs()}
	m.resumeAll(ctx)
	errs = append(errs, m.stopWorkers(ctx, false)...)
	if p, err := m.currentServer(); err == nil {
		errs = append(errs, p.stop(ctx, m.stopTimeout))
	}
	return errors.Join(errs...)
}

// resumeAll continues every task group and process the harness stalled, the
// task groups first, so none of them waits out its stop grace. It records the
// task groups it continued for Close's endTaskGroups.
func (m *managed) resumeAll(ctx context.Context) {
	m.mu.Lock()
	procs := []*proc{m.server}
	for _, w := range m.workers {
		procs = append(procs, w.proc)
	}
	n := len(m.workers)
	m.mu.Unlock()
	for i := range n {
		groups, err := m.contTaskGroups(i)
		if err != nil {
			m.logger.WarnContext(ctx, "could not continue a stalled task", slog.Any("error", err))
		}
		m.mu.Lock()
		m.resumedGroups = append(m.resumedGroups, groups...)
		m.mu.Unlock()
	}
	for _, p := range procs {
		if p == nil || !p.isStalled() {
			continue
		}
		if err := p.resume(); err != nil {
			m.logger.WarnContext(ctx, "could not continue a stalled process", slog.String("process", p.name), slog.Any("error", err))
		}
	}
}

// stopWorkers stops every real worker's current process together, recording
// their incarnations' ends. With release it also releases them and every
// retired process.
func (m *managed) stopWorkers(ctx context.Context, release bool) []error {
	m.mu.Lock()
	workers := slices.Clone(m.workers)
	m.mu.Unlock()
	errs := make([]error, len(workers))
	var wg sync.WaitGroup
	for i, w := range workers {
		m.mu.Lock()
		p, retired := w.proc, slices.Clone(w.retired)
		m.mu.Unlock()
		if p == nil {
			continue
		}
		wg.Go(func() {
			if release {
				errs[i] = p.stopCtx(ctx, m.stopTimeout)
				for _, r := range retired {
					errs[i] = errors.Join(errs[i], r.stopCtx(ctx, m.stopTimeout))
				}
			} else {
				errs[i] = p.stop(ctx, m.stopTimeout)
			}
			if p.exited() {
				m.markEnded(i, p)
			}
		})
	}
	wg.Wait()
	return errs
}
