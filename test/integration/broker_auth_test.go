// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// TestRevocation_DisconnectsAndReclaims proves revocation end to end: DELETE
// /api/v1/workers/{id}/credential revokes in the store, then reloads the
// running broker's authorized-key set, which disconnects the revoked
// worker's live NATS connection SYNCHRONOUSLY — inside the reload call,
// because nats-server's reloadAuthorization re-runs isClientAuthorized over
// every connected client and calls authViolation() on any that no longer
// pass. The disconnected worker's in-flight task is then returned to ready
// by the EXISTING heartbeat-sweep/reclaim path (internal/scheduler), not by
// anything this test reimplements, and a second, unrevoked
// worker is left completely unaffected and able to lease the reclaimed
// task.
//
// This is deliberately distinct from "sqi-server worker revoke" (the CLI),
// which writes the same store row from a separate process holding no broker
// handle and only takes effect at the running server's next start — that
// path is covered by cmd/sqi-server's own tests, not here.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/server"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// ── Server boot (broker auth on, auth off) ──────────────────────────────────

// startBrokerAuthServer boots a full sqi-server with broker authentication
// enabled and a short heartbeat-sweep timing so a revoked worker's task is
// reclaimed within seconds rather than this suite's default 30s. Auth
// (session/API-key) is left off: DELETE /api/v1/workers/{id}/credential is
// mounted unconditionally regardless of AuthEnabled (the anonymous
// superuser principal is granted it, same as most permission-gated routes),
// so nothing about the revocation path this test exercises needs a login
// flow.
//
// sqlitePath is the caller's choice, not a generated temp path, so a caller
// can open its own store.Store on the same file and seed rows (e.g. an
// enrolled worker credential) BEFORE calling this — see
// seedWorkerCredential. The store's write pool is a single connection;
// seeding must complete and close its handle before this server opens its
// own, not run concurrently with it.
func startBrokerAuthServer(t *testing.T, sqlitePath string, mutate func(*server.Config)) *testServer {
	t.Helper()

	httpAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	natsAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	tmpDir := t.TempDir()

	cfg := server.Config{
		HTTPAddr:           httpAddr,
		CORSOrigins:        []string{"*"},
		NATSAddr:           natsAddr,
		NATSDataDir:        tmpDir + "/nats",
		NATSMaxStoreMB:     64,
		SQLitePath:         sqlitePath,
		CheckpointInterval: time.Minute,
		Scheduler: scheduler.Config{
			AssignInterval:  100 * time.Millisecond,
			AssignBatchSize: 10,
			AssignWorkers:   2,
			// Short enough that the heartbeat-sweep reclaim this test
			// verifies against completes in a few seconds, not this
			// suite's usual 30s/15s (tuned for tests that never exercise
			// offline detection at all).
			WorkerTimeout:          2 * time.Second,
			HeartbeatSweepInterval: 300 * time.Millisecond,
		},
		DiscoveryEnabled: false,

		// The enrollment endpoint is deliberately left unmounted: this
		// suite enrolls by seeding the store directly before boot (see
		// seedWorkerCredential) rather than through POST /workers/enroll,
		// so nothing here needs it.
		NATSAuthEnabled: true,
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
		t.Fatalf("startBrokerAuthServer: server exited during startup: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	ts := &testServer{HTTPAddr: httpAddr, NATSAddr: natsAddr, cancel: cancel, done: done}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Logf("warning: server did not stop within 15s after context cancel")
		}
	})

	if !waitForTCP(t, httpAddr, 10*time.Second) {
		t.Fatal("startBrokerAuthServer: HTTP server did not start listening")
	}
	if !waitForReadyz(t, httpAddr, 10*time.Second) {
		t.Fatal("startBrokerAuthServer: server did not become ready")
	}
	return ts
}

// seedWorkerCredential inserts an already-enrolled [store.WorkerCredential]
// row directly into the SQLite database at dbPath, mirroring what
// "sqi-server worker enroll" (the offline CLI enrollment path) writes — a
// direct store.CreateWorkerCredential call from a process holding no broker
// handle. This is deliberate, not a shortcut around POST /workers/enroll:
// enrollment through either the CLI or the REST endpoint only takes effect
// in a running broker's authorized-key set at the broker's NEXT START
// (loadBrokerAuthConfig reads the active credential set once, at Start) —
// unlike revocation, nothing reloads a live broker's authorized-key set on
// a new enrollment. So a worker this test wants connectable from the moment
// the server boots must be enrolled in the store BEFORE startBrokerAuthServer
// runs, exactly as an operator running the CLI before first start would do.
// Must run before the server opens its own connection to the same file —
// see startBrokerAuthServer.
func seedWorkerCredential(t *testing.T, dbPath, workerID, publicKey string) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, dbPath, sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("seedWorkerCredential: sqlite.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	if _, err := st.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID:         workerID + "-cred",
		WorkerID:   workerID,
		PublicKey:  publicKey,
		EnrolledAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seedWorkerCredential: CreateWorkerCredential: %v", err)
	}
}

// ── REST helper for the synchronous revoke path under test ─────────────────

// revokeWorkerCredential calls the synchronous revocation endpoint under
// test: DELETE /api/v1/workers/{id}/credential.
func revokeWorkerCredential(t *testing.T, ts *testServer, workerID string) {
	t.Helper()
	mustDoJSON(t, http.MethodDelete, apiURL(ts, "/api/v1/workers/"+workerID+"/credential"), nil, "", http.StatusNoContent, nil)
}

// pollTaskStatus polls GET /api/v1/tasks/{id} (via getTaskDetail, defined in
// retry_test.go) until its status is one of targets, or timeout elapses.
func pollTaskStatus(t *testing.T, ts *testServer, taskID string, targets []string, timeout time.Duration) string {
	t.Helper()
	targetSet := make(map[string]bool, len(targets))
	for _, s := range targets {
		targetSet[s] = true
	}
	var last string
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		last = getTaskDetail(t, ts, taskID).Status
		if targetSet[last] {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("pollTaskStatus: task %s stuck at %q; wanted one of %v after %s", taskID, last, targets, timeout)
	return ""
}

// ── nkey-authenticated mock worker ──────────────────────────────────────────

// newMockWorkerWithNkey connects to natsURL as the given enrolled nkey
// credential and returns a *mockWorker built around that connection.
// extraOpts is appended after the nkey and NoReconnect options — NoReconnect
// so a revoked worker's disconnect is observed directly rather than masked
// by nats.go retrying (and getting the same auth error) for some time first,
// mirroring internal/bus's own revocation tests.
func newMockWorkerWithNkey(t *testing.T, natsURL, workerID, farmID, queueID string, seed []byte, pub string, extraOpts ...nats.Option) *mockWorker {
	t.Helper()

	opts := append([]nats.Option{
		nats.Nkey(pub, func(nonce []byte) ([]byte, error) {
			kp, err := nkeys.FromSeed(seed)
			if err != nil {
				return nil, err
			}
			return kp.Sign(nonce)
		}),
		nats.NoReconnect(),
	}, extraOpts...)

	nc, err := nats.Connect(natsURL, opts...)
	if err != nil {
		t.Fatalf("newMockWorkerWithNkey(%s): Connect: %v", workerID, err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		t.Fatalf("newMockWorkerWithNkey(%s): jetstream.New: %v", workerID, err)
	}

	t.Cleanup(func() {
		if !nc.IsClosed() {
			nc.Close()
		}
	})

	return &mockWorker{t: t, id: workerID, farmID: farmID, queueID: queueID, nc: nc, js: js}
}

// ── The test ─────────────────────────────────────────────────────────────────

func TestRevocation_DisconnectsAndReclaims(t *testing.T) {
	sqlitePath := t.TempDir() + "/sqi-broker-auth.db"

	seedA, pubA, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed(A): %v", err)
	}
	seedB, pubB, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed(B): %v", err)
	}

	// Both workers are enrolled BEFORE the server starts, so the broker's
	// initial authorized-key set (built once, at Start, from
	// ListActiveWorkerCredentials) includes both — see seedWorkerCredential's
	// doc comment for why this test does not use POST /workers/enroll.
	seedWorkerCredential(t, sqlitePath, "worker-a", pubA)
	seedWorkerCredential(t, sqlitePath, "worker-b", pubB)

	ts := startBrokerAuthServer(t, sqlitePath, nil)

	farmID, queueID := seedFarmAndQueue(t, ts)

	natsURL := "nats://" + ts.NATSAddr

	closedA := make(chan struct{})
	workerA := newMockWorkerWithNkey(t, natsURL, "worker-a", farmID, queueID, seedA, pubA,
		nats.ClosedHandler(func(*nats.Conn) { close(closedA) }))
	workerB := newMockWorkerWithNkey(t, natsURL, "worker-b", farmID, queueID, seedB, pubB)

	workerA.register()
	workerB.register()
	// Heartbeat well under the 2s WorkerTimeout configured above. Worker A's
	// heartbeat loop stops on its own the moment revocation closes its NATS
	// connection (the publish fails and the loop returns) — nothing here
	// needs to stop it explicitly.
	workerA.startHeartbeat(200 * time.Millisecond)
	workerB.startHeartbeat(200 * time.Millisecond)

	pollWorkerOnline(t, ts, "worker-a", 5*time.Second)
	pollWorkerOnline(t, ts, "worker-b", 5*time.Second)

	jobID := submitJob(t, ts, farmID, queueID)
	assign := workerA.pullAssignment(15 * time.Second)
	if assign.JobID != jobID {
		t.Fatalf("assignment job ID: got %q, want %q", assign.JobID, jobID)
	}
	taskID := assign.TaskID

	workerA.publishStatus(assign, "running", nil)
	pollTaskStatus(t, ts, taskID, []string{"running"}, 5*time.Second)

	// ── Revoke worker A: the synchronous path under test ────────────────────
	revokeWorkerCredential(t, ts, "worker-a")

	// 1. A's NATS connection closes. Revocation is synchronous — nats-server
	// re-authorizes every connected client inside the broker's
	// ReloadOptions call, which DELETE /workers/{id}/credential's handler
	// waits on — so this is not a poll for a generous timeout, only
	// tolerance for scheduling jitter in observing an event that already
	// happened.
	select {
	case <-closedA:
	case <-time.After(2 * time.Second):
		t.Fatal("worker A's NATS connection was not closed by revocation")
	}

	// 2. A's task returns to ready via a legal store.ValidateTaskTransition
	// arrow (running -> ready), applied by the EXISTING heartbeat-sweep and
	// reclaim path once A's missed heartbeats exceed WorkerTimeout — not by
	// anything this test or RevokeWorker itself does directly.
	pollTaskStatus(t, ts, taskID, []string{"ready"}, 8*time.Second)

	// 3. B is unaffected and can still lease — proven by actually leasing
	// the reclaimed task, which simultaneously confirms the reclaim resulted
	// in a real reassignment rather than a status stuck at "ready".
	assignB := workerB.pullAssignment(10 * time.Second)
	if assignB.TaskID != taskID {
		t.Fatalf("worker B's assignment: got task %q, want the reclaimed task %q", assignB.TaskID, taskID)
	}
	if !workerB.nc.IsConnected() {
		t.Fatal("worker B's connection was disturbed by A's revocation")
	}
}
