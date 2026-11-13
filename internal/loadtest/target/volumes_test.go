// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/uberware/sqi/internal/loadtest/diskfill"
)

// fakeFiller records what the target fills, so the tests need no real volume.
// Each fill leaves a ballast file in the directory, which Free removes.
type fakeFiller struct {
	mu     sync.Mutex
	filled []fillCall
	err    error
}

type fillCall struct {
	dir      string
	headroom int64
	inodes   bool
}

func (f *fakeFiller) fill(dir string, headroom int64, inodes bool) (*diskfill.Ballast, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filled = append(f.filled, fillCall{dir, headroom, inodes})
	if f.err != nil {
		return nil, f.err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, diskfill.BallastName), []byte("x"), 0o600); err != nil {
		return nil, err
	}
	return diskfill.Adopt(dir), nil
}

func (f *fakeFiller) calls() []fillCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.filled)
}

func volumeManaged(t *testing.T) (m *managed, server, workers string) {
	t.Helper()
	server, workers = t.TempDir(), t.TempDir()
	m, err := newManaged(ManagedConfig{
		RunDir:       t.TempDir(),
		RunID:        "run1",
		Scenario:     laneScenario(false),
		ServerVolume: server,
		WorkerVolume: workers,
	})
	if err != nil {
		t.Fatalf("newManaged: %v", err)
	}
	return m, server, workers
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestManaged_VolumesPlaceDataAndSessions(t *testing.T) {
	m, server, workers := volumeManaged(t)
	m.farm.id = "f"
	if want := filepath.Join(server, "run1"); m.settings.dataDir() != want {
		t.Errorf("dataDir = %q, want %q", m.settings.dataDir(), want)
	}
	if want := filepath.Join(server, "run1", "sqi.db"); m.settings.sqlitePath() != want {
		t.Errorf("sqlitePath = %q, want %q", m.settings.sqlitePath(), want)
	}
	if got := m.DataDir(); got != m.settings.dataDir() {
		t.Errorf("DataDir = %q, want %q", got, m.settings.dataDir())
	}
	cfg, err := renderServerConfig(m.settings)
	if err != nil {
		t.Fatalf("renderServerConfig: %v", err)
	}
	if !strings.Contains(string(cfg), filepath.Join(server, "run1", "nats")) {
		t.Errorf("server.yaml does not put the NATS data directory under the server volume:\n%s", cfg)
	}
	env, err := m.workerEnv(m.cfg.Scenario.Workers[0], 2, "0.0")
	if err != nil {
		t.Fatalf("workerEnv: %v", err)
	}
	if want := "SQI_WORKER_SESSION_DIR=" + filepath.Join(workers, "run1", "2-sessions"); !slices.Contains(env, want) {
		t.Errorf("worker env lacks %q; has %v", want, env)
	}
	// The worker's own data directory, the logs and the config stay off the
	// volumes, so the harness can still write while one is full.
	for _, kv := range env {
		if strings.HasPrefix(kv, "SQI_WORKER_DATA_DIR=") && strings.Contains(kv, workers) {
			t.Errorf("worker data directory is on the worker volume: %s", kv)
		}
	}
	if !strings.HasPrefix(m.serverLog(), m.settings.RunDir) || strings.Contains(m.serverLog(), server) {
		t.Errorf("server.log = %q, want it under the run directory %q", m.serverLog(), m.settings.RunDir)
	}
	if strings.Contains(m.configPath, server) || strings.Contains(m.workerLog(0), workers) {
		t.Errorf("server.yaml %q or a worker log %q is on a volume", m.configPath, m.workerLog(0))
	}
}

func TestManaged_NoVolumesKeepsTheRunDirectoryLayout(t *testing.T) {
	m := newTestManaged(t, laneScenario(false))
	if want := filepath.Join(m.settings.RunDir, "data"); m.settings.dataDir() != want {
		t.Errorf("dataDir = %q, want %q", m.settings.dataDir(), want)
	}
	m.farm.id = "f"
	env, err := m.workerEnv(m.cfg.Scenario.Workers[0], 1, "0.0")
	if err != nil {
		t.Fatal(err)
	}
	want := "SQI_WORKER_SESSION_DIR=" + filepath.Join(m.settings.RunDir, "workers", "1-sessions")
	if !slices.Contains(env, want) {
		t.Errorf("worker env lacks %q", want)
	}
}

func TestManaged_FillVolumeWithoutVolumeIsAnError(t *testing.T) {
	m := newTestManaged(t, laneScenario(false))
	ff := &fakeFiller{}
	m.fill = ff.fill
	for _, v := range []string{VolumeServer, VolumeWorkers, "nope"} {
		if err := m.FillVolume(v, 1<<20, false); err == nil {
			t.Errorf("FillVolume(%q) succeeded on a target with no volumes", v)
		}
		if err := m.FreeVolume(v); err == nil {
			t.Errorf("FreeVolume(%q) succeeded on a target with no volumes", v)
		}
	}
	if n := len(ff.calls()); n != 0 {
		t.Errorf("the filler was called %d times", n)
	}
}

func TestManaged_FillAndFreeVolume(t *testing.T) {
	m, server, workers := volumeManaged(t)
	ff := &fakeFiller{}
	m.fill = ff.fill
	if err := m.FillVolume(VolumeServer, 5, true); err != nil {
		t.Fatalf("FillVolume(server): %v", err)
	}
	if err := m.FillVolume(VolumeWorkers, 7, false); err != nil {
		t.Fatalf("FillVolume(workers): %v", err)
	}
	want := []fillCall{
		{filepath.Join(server, "run1"), 5, true},
		{filepath.Join(workers, "run1"), 7, false},
	}
	if got := ff.calls(); !slices.Equal(got, want) {
		t.Errorf("fill calls = %v, want %v", got, want)
	}
	if err := m.FillVolume(VolumeServer, 5, true); err != nil {
		t.Errorf("filling a filled volume = %v, want a no-op", err)
	}
	if n := len(ff.calls()); n != 2 {
		t.Errorf("a second FillVolume refilled: %d calls", n)
	}
	serverBallast := filepath.Join(server, "run1", diskfill.BallastName)
	for range 2 {
		if err := m.FreeVolume(VolumeServer); err != nil {
			t.Errorf("FreeVolume(server): %v", err)
		}
	}
	if exists(serverBallast) {
		t.Error("the server volume's ballast is still there after FreeVolume")
	}
	if !exists(filepath.Join(workers, "run1", diskfill.BallastName)) {
		t.Error("freeing the server volume removed the workers' ballast")
	}
	if err := m.FillVolume(VolumeServer, 5, true); err != nil {
		t.Fatalf("filling again after a free: %v", err)
	}
	if !exists(serverBallast) {
		t.Error("the refilled server volume has no ballast")
	}
}

func TestManaged_FillVolumeErrorLeavesNothingApplied(t *testing.T) {
	m, _, _ := volumeManaged(t)
	boom := errors.New("boom")
	m.fill = (&fakeFiller{err: boom}).fill
	if err := m.FillVolume(VolumeServer, 0, false); !errors.Is(err, boom) {
		t.Fatalf("FillVolume = %v, want the filler's error", err)
	}
	m.volMu.Lock()
	n := len(m.ballast)
	m.volMu.Unlock()
	if n != 0 {
		t.Errorf("%d ballasts recorded after a failed fill", n)
	}
}

func TestManaged_FillVolumeAfterCloseIsRefused(t *testing.T) {
	m, _, _ := volumeManaged(t)
	ff := &fakeFiller{}
	m.fill = ff.fill
	if err := m.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.FillVolume(VolumeServer, 0, false); !errors.Is(err, errClosing) {
		t.Errorf("FillVolume after Close = %v, want errClosing", err)
	}
	if n := len(ff.calls()); n != 0 {
		t.Errorf("the filler ran %d times after Close", n)
	}
}

func TestManaged_CloseRemovesBallast(t *testing.T) {
	m, server, workers := volumeManaged(t)
	m.fill = (&fakeFiller{}).fill
	for _, v := range []string{VolumeServer, VolumeWorkers} {
		if err := m.FillVolume(v, 0, false); err != nil {
			t.Fatalf("FillVolume(%s): %v", v, err)
		}
	}
	paths := []string{
		filepath.Join(server, "run1", diskfill.BallastName),
		filepath.Join(workers, "run1", diskfill.BallastName),
	}
	for _, p := range paths {
		if !exists(p) {
			t.Fatalf("no ballast at %s before Close", p)
		}
	}
	if err := m.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, p := range paths {
		if exists(p) {
			t.Errorf("ballast %s survived Close", p)
		}
	}
	if err := m.Close(t.Context()); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestManaged_ServerReady(t *testing.T) {
	m := newTestManaged(t, laneScenario(false))
	if got := m.ServerReady(t.Context()); got == "ready" || got == "" {
		t.Errorf("ServerReady before any server started = %q, want an explanation", got)
	}
	var mu sync.Mutex
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	m.httpURL = srv.URL
	if got := m.ServerReady(t.Context()); got != "ready" {
		t.Errorf("ServerReady = %q, want ready", got)
	}
	mu.Lock()
	status = http.StatusServiceUnavailable
	mu.Unlock()
	if got := m.ServerReady(t.Context()); got == "ready" || !strings.Contains(got, "503") {
		t.Errorf("ServerReady on a 503 = %q, want the probe's error", got)
	}
	srv.Close()
	if got := m.ServerReady(t.Context()); got == "ready" || got == "" {
		t.Errorf("ServerReady with the server gone = %q, want the probe's error", got)
	}
}
