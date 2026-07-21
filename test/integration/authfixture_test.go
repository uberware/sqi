// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// authfixture_test.go — the pieces ldap_test.go and oidc_test.go share.
//
// Both suites boot a container holding a real identity backend and an
// sqi-server wired to it, and they were doing so with two verbatim copies of
// the same code. The copies are here instead. Note that the "the shared harness
// is untagged, this file is not" argument that justifies keeping these variants
// out of harness_test.go does NOT apply between the two suites: both are
// //go:build integration in this same package, so a helper tagged the same way
// is theirs to share.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/server"
)

// ── Docker ────────────────────────────────────────────────────────────────────

var (
	dockerProbeOnce sync.Once
	// dockerProbeSkip is empty when docker is usable and otherwise says why it
	// is not. Probed once per test binary: `docker info` costs a few hundred
	// milliseconds and the answer cannot change mid-run.
	dockerProbeSkip string
)

// requireDocker skips the calling test unless a responding docker daemon is
// available. what names the suite ("LDAP", "OIDC") and envHint completes the
// sentence "install Docker/colima/podman, or <envHint>".
func requireDocker(t *testing.T, what, envHint string) {
	t.Helper()
	dockerProbeOnce.Do(func() {
		if _, err := exec.LookPath("docker"); err != nil {
			dockerProbeSkip = "docker not found on PATH"
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "docker", "info").CombinedOutput(); err != nil {
			dockerProbeSkip = fmt.Sprintf("docker daemon not responding: %v\n%s", err, out)
		}
	})
	if dockerProbeSkip != "" {
		t.Skipf("skipping %s integration test: %s (install Docker/colima/podman, or %s)",
			what, dockerProbeSkip, envHint)
	}
}

// removeContainer force-removes a container, best effort. Containers are
// started with --rm so an aborted run (SIGINT, panic) does not leak one; this
// is the ordinary path, and it is deliberately not allowed to fail a test —
// there is nothing useful a test can do about a teardown that did not take.
func removeContainer(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run() //nolint:errcheck // best-effort teardown
}

// removeContainerOnCleanup removes name when the test finishes.
func removeContainerOnCleanup(t *testing.T, name string) {
	t.Helper()
	t.Cleanup(func() { removeContainer(name) })
}

// ── Server ────────────────────────────────────────────────────────────────────

// startAuthServer boots a full auth-enabled sqi-server on httpAddr, with the
// SQLite file named dbName inside a per-test temp dir, and mutate applied to
// the configuration — which is where the caller wires its own auth backend.
//
// Self-contained rather than an option on startServer: that harness is untagged
// and every non-integration test depends on it, so the auth-enabled variant
// stays behind the build tag with its callers.
func startAuthServer(t *testing.T, httpAddr, dbName string, mutate func(*server.Config)) *testServer {
	t.Helper()

	natsAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	tmpDir := t.TempDir()

	cfg := server.Config{
		HTTPAddr:           httpAddr,
		CORSOrigins:        []string{"*"},
		NATSAddr:           natsAddr,
		NATSDataDir:        tmpDir + "/nats",
		NATSMaxStoreMB:     64,
		SQLitePath:         tmpDir + "/" + dbName,
		CheckpointInterval: time.Minute,
		Scheduler: scheduler.Config{
			AssignInterval:         100 * time.Millisecond,
			AssignBatchSize:        10,
			AssignWorkers:          2,
			WorkerTimeout:          30 * time.Second,
			HeartbeatSweepInterval: 15 * time.Second,
		},
		DiscoveryEnabled: false,

		AuthEnabled:      true,
		AuthSessionTTL:   time.Hour,
		AuthCookieName:   "sqi_session",
		AuthCookieSecure: "false",
	}
	if mutate != nil {
		mutate(&cfg)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	srv := server.New(cfg, logger, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	select {
	case err := <-done:
		cancel()
		t.Fatalf("server exited during startup: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	ts := &testServer{HTTPAddr: httpAddr, NATSAddr: natsAddr, cancel: cancel, done: done}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	})

	if !waitForTCP(t, httpAddr, 20*time.Second) {
		t.Fatal("HTTP server did not start listening")
	}
	if !waitForReadyz(t, httpAddr, 30*time.Second) {
		t.Fatal("server did not become ready")
	}
	return ts
}
