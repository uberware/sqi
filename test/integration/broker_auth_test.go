// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// TestRevocation_DisconnectsAndReclaims tests revocation end to end: DELETE
// /api/v1/workers/{id}/credential revokes in the store, then asks the broker
// to disconnect the worker. The broker's Revoke call closes the revoked
// worker's live NATS connection synchronously, returning only after its final
// pass has disconnected any connection left. The worker's reconnect goes back
// through the auth callout, which refuses the revoked key. The disconnected
// worker's in-flight task is then returned to ready by the scheduler's
// heartbeat-sweep reclaim path (internal/scheduler), and a second, unrevoked
// worker is unaffected and leases the reclaimed task.
//
// This is distinct from "sqi-server worker revoke" (the CLI), which writes the
// same store row from a separate process holding no broker handle and takes
// effect at the running server's next start; cmd/sqi-server's tests cover that
// path.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/auth/jointoken"
	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/server"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/backend"
)

// ── Server boot (broker auth on, auth off) ──────────────────────────────────

// startBrokerAuthServer boots a full sqi-server with broker authentication
// enabled and a short heartbeat-sweep timing so a revoked worker's task is
// reclaimed within seconds rather than this suite's default 30s. Auth
// (session/API-key) is left off: DELETE /api/v1/workers/{id}/credential is
// mounted whatever AuthEnabled says (the anonymous superuser principal is
// granted it, as for most permission-gated routes), so the revocation path
// needs no login.
//
// so is chosen by the caller (from storeFor) rather than generated, so a
// caller can open its own handle on the same database and seed rows (e.g. an
// enrolled worker credential) before calling this; see seedWorkerCredential.
// On SQLite the store's write pool is a single connection, so seeding must
// finish and close its handle before this server opens its own. The same
// order is kept on PostgreSQL, though a second handle would do no harm there.
func startBrokerAuthServer(t *testing.T, so backend.Options, mutate func(*server.Config)) *testServer {
	t.Helper()

	httpAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	natsAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	tmpDir := t.TempDir()

	cfg := server.Config{
		HTTPAddr:           httpAddr,
		CORSOrigins:        []string{"*"},
		CheckpointInterval: time.Minute,
		Scheduler: scheduler.Config{
			AssignInterval:  100 * time.Millisecond,
			AssignBatchSize: 10,
			AssignWorkers:   2,
			// Short enough that the heartbeat-sweep reclaim these tests
			// wait for completes in a few seconds, not this suite's usual
			// 30s/15s (sized for tests that never exercise offline
			// detection).
			WorkerTimeout:          2 * time.Second,
			HeartbeatSweepInterval: 300 * time.Millisecond,
			// The server's default grace (~3 minutes, sized for a worker's
			// own redial backoff) would hold the sweep off for longer than
			// these tests run. Their workers never lose the broker, so the
			// grace only has to cover WorkerTimeout after boot.
			BusReconnectGrace: 2 * time.Second,
		},
		DiscoveryEnabled: false,

		// The enrollment endpoint is off by default: most callers enroll by
		// seeding the store directly before boot (see seedWorkerCredential)
		// rather than through POST /workers/enroll. A caller that needs the
		// real REST enrollment surface turns it on via mutate — see
		// TestEnrollment_ConnectsToRunningBrokerWithoutRestart.
		NATS: config.NATSConfig{
			Embedded: config.NATSEmbeddedConfig{
				Addr:       natsAddr,
				DataDir:    tmpDir + "/nats",
				MaxStoreMB: 64,
			},
			// A hand-built Config skips config defaults, and the auth
			// callout needs a positive user JWT lifetime.
			Auth: config.NATSAuthConfig{Enabled: true, UserTTL: time.Hour},
		},
	}
	applyStore(&cfg, so)
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

	ts := &testServer{HTTPAddr: httpAddr, NATSAddr: natsAddr, Store: so, DBPath: so.SQLitePath, cancel: cancel, done: done}
	// Derive the transport from the config this server was given, so
	// every shared helper (apiURL, seedFarmAndQueue, pollForOnlineWorker, the
	// readiness probe below) speaks the right scheme with no TLS-specific
	// variants. `sqi-server tls init` writes ca.crt beside the server cert,
	// which is what the TLS callers here use.
	if cfg.HTTPTLS.Enabled {
		ts.Scheme = "https"
		ts.Client = tlsClient(t, filepath.Dir(cfg.HTTPTLS.CertFile))
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Logf("warning: server did not stop within 15s after context cancel")
		}
	})

	if !waitForTCP(t, httpAddr, 10*time.Second) {
		t.Fatalf("startBrokerAuthServer: HTTP server did not start listening on %s (NATS %s)", httpAddr, natsAddr)
	}
	// Readiness must be probed over the scheme this server serves: an https://
	// listener answers a plaintext GET with a 400, which would read as never
	// becoming ready. A TLS caller stamps
	// ts.Scheme/ts.Client before this runs (see startTLSServer in tls_test.go).
	if !waitForReadyzClient(t, clientFor(ts), apiURL(ts, "/readyz"), 10*time.Second) {
		t.Fatalf("startBrokerAuthServer: server did not become ready (HTTP %s, NATS %s)", httpAddr, natsAddr)
	}
	return ts
}

// seedWorkerCredential inserts an already-enrolled [store.WorkerCredential]
// row directly into the database o names, as "sqi-server worker enroll" (the
// offline CLI enrollment path) does: a direct store.CreateWorkerCredential
// call from a process holding no broker handle, making no broker call. The
// broker's auth callout reads the row from the store when the worker
// connects. Tests use it when they want workers connectable from the moment
// the server boots and do not test the REST enrollment surface (see
// enrollWorker for that). Must run before the server opens its own
// connection to the same database; see startBrokerAuthServer.
func seedWorkerCredential(t *testing.T, o backend.Options, workerID, publicKey string) {
	t.Helper()
	ctx := context.Background()
	st := openTestStore(t, o)
	// Closed here, not left to the test's cleanup: on SQLite this handle must
	// be gone before the server opens its own. Closing twice is harmless.
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

// seedJoinToken inserts a join token row directly into the database o names
// and returns the raw token, bypassing POST /workers/join-tokens, which
// requires session auth these tests do not otherwise need. Must run before
// the server opens its own connection to the same database, as with
// seedWorkerCredential.
func seedJoinToken(t *testing.T, o backend.Options, name string) string {
	t.Helper()
	raw, hash, prefix, err := jointoken.Generate()
	if err != nil {
		t.Fatalf("seedJoinToken: jointoken.Generate: %v", err)
	}

	ctx := context.Background()
	st := openTestStore(t, o)
	// Closed here for the reason given in seedWorkerCredential.
	defer func() { _ = st.Close() }()

	now := time.Now().UTC()
	if _, err := st.CreateWorkerJoinToken(ctx, store.WorkerJoinToken{
		ID:        name + "-token",
		TokenHash: hash,
		Prefix:    prefix,
		Name:      name,
		ExpiresAt: now.Add(time.Hour),
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("seedJoinToken: CreateWorkerJoinToken: %v", err)
	}
	return raw
}

// ── REST helpers for the synchronous enroll and revoke paths under test ────

// workerCredentialWireResp is the subset of POST /workers/enroll's response
// this suite needs (mirrors internal/api/workerenroll.go's
// workerCredentialResponse).
type workerCredentialWireResp struct {
	ID       string `json:"id"`
	WorkerID string `json:"worker_id"`
}

// enrollWorker exchanges joinToken for a broker credential over the real,
// unauthenticated POST /api/v1/workers/enroll wire protocol, which makes the
// credential connectable on a running broker with no further step.
func enrollWorker(t *testing.T, ts *testServer, joinToken, workerID, publicKey string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"join_token": joinToken,
		"worker_id":  workerID,
		"public_key": publicKey,
	})
	if err != nil {
		t.Fatalf("enrollWorker: marshal: %v", err)
	}
	var resp workerCredentialWireResp
	mustDoJSON(t, http.MethodPost, apiURL(ts, "/api/v1/workers/enroll"), body, "application/json", http.StatusCreated, &resp)
	if resp.WorkerID != workerID {
		t.Fatalf("enrollWorker: response worker_id = %q, want %q", resp.WorkerID, workerID)
	}
}

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
//
// The per-worker inbox prefix is required: an enrolled worker is granted
// "_INBOX_<id>.>" and nothing wider, so a connection on nats.go's default
// "_INBOX" cannot subscribe to its own reply inbox, and every JetStream
// publish fails waiting for a PubAck it is not allowed to receive.
// internal/worker/natsclient sets the same option on every real worker
// connection.
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
		nats.CustomInboxPrefix(brokerauth.InboxPrefix(workerID)),
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
	so := storeFor(t, t.TempDir()+"/sqi-broker-auth.db")

	seedA, pubA, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed(A): %v", err)
	}
	seedB, pubB, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed(B): %v", err)
	}

	// Both workers are enrolled before the server starts, so both are
	// connectable from the moment it boots. See seedWorkerCredential's doc
	// comment for why this test does not use POST /workers/enroll.
	seedWorkerCredential(t, so, "worker-a", pubA)
	seedWorkerCredential(t, so, "worker-b", pubB)

	ts := startBrokerAuthServer(t, so, nil)

	farmID, queueID := seedFarmAndQueue(t, ts)

	natsURL := "nats://" + ts.NATSAddr

	closedA := make(chan struct{})
	workerA := newMockWorkerWithNkey(t, natsURL, "worker-a", farmID, queueID, seedA, pubA,
		nats.ClosedHandler(func(*nats.Conn) { close(closedA) }))
	workerB := newMockWorkerWithNkey(t, natsURL, "worker-b", farmID, queueID, seedB, pubB)

	workerA.register()
	workerB.register()
	// Heartbeat well under the 2s WorkerTimeout configured above. Once
	// revocation closes worker A's NATS connection its heartbeat loop keeps
	// ticking, but every publish fails on the closed connection, so the
	// server hears nothing more from it; the test's cleanup ends the loop.
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

	// 1. A's NATS connection closes. Revocation is synchronous (the broker
	// disconnects the worker inside Revoke, which DELETE
	// /workers/{id}/credential's handler waits on), so the timeout below only
	// allows for scheduling jitter in observing an event that already
	// happened.
	select {
	case <-closedA:
	case <-time.After(2 * time.Second):
		t.Fatal("worker A's NATS connection was not closed by revocation")
	}

	// 2. A's task returns to ready (running -> ready), moved by the
	// heartbeat-sweep reclaim path once A's missed heartbeats exceed
	// WorkerTimeout, not by this test or by RevokeWorker.
	pollTaskStatus(t, ts, taskID, []string{"ready"}, 8*time.Second)

	// 3. B is unaffected and can still lease: it leases the reclaimed task,
	// which also confirms the reclaimed task can be assigned again.
	assignB := workerB.pullAssignment(10 * time.Second)
	if assignB.TaskID != taskID {
		t.Fatalf("worker B's assignment: got task %q, want the reclaimed task %q", assignB.TaskID, taskID)
	}
	if !workerB.nc.IsConnected() {
		t.Fatal("worker B's connection was disturbed by A's revocation")
	}
}

// pollWorkerOffline polls GET /api/v1/workers until workerID is visible with
// status "offline", or timeout elapses. Used to wait for the heartbeat-sweep
// to notice a worker that has stopped heartbeating, without depending on the
// sweep's exact timing.
func pollWorkerOffline(t *testing.T, ts *testServer, workerID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var resp workerListResp
		mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/workers"), nil, "", http.StatusOK, &resp)
		for _, w := range resp.Items {
			if w.ID == workerID && w.Status == "offline" {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("pollWorkerOffline: worker %s did not go offline within %s", workerID, timeout)
}

// TestWorkerDeletion_RevokesCredentialAndDisconnects tests the cascade DELETE
// /api/v1/workers/{id} performs: removing a worker record also revokes its
// broker credential, through the same synchronous
// store-write-then-broker-disconnect path DELETE /workers/{id}/credential uses.
// Without it, a machine an operator has decommissioned would keep broker
// access (able to connect, lease work and execute job code), because
// WorkersManage (what deleting a worker requires) does not imply
// WorkersEnroll (what revoking a credential directly requires).
//
// worker-a never heartbeats after registering, so the heartbeat sweep marks
// it offline, the only status DELETE /workers/{id} accepts without a disable
// step, while its NATS connection stays open. The test then deletes the
// worker over REST and asserts the open connection is closed by the same
// Broker.Revoke call TestRevocation_DisconnectsAndReclaims tests for the
// credential-revoke endpoint.
func TestWorkerDeletion_RevokesCredentialAndDisconnects(t *testing.T) {
	so := storeFor(t, t.TempDir()+"/sqi-broker-auth-delete.db")

	seedA, pubA, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed(A): %v", err)
	}
	seedWorkerCredential(t, so, "worker-a", pubA)

	ts := startBrokerAuthServer(t, so, nil)
	farmID, queueID := seedFarmAndQueue(t, ts)
	natsURL := "nats://" + ts.NATSAddr

	closedA := make(chan struct{})
	workerA := newMockWorkerWithNkey(t, natsURL, "worker-a", farmID, queueID, seedA, pubA,
		nats.ClosedHandler(func(*nats.Conn) { close(closedA) }))
	workerA.register()
	// No startHeartbeat: the worker must go offline in the store (the only
	// status DELETE /workers/{id} accepts here) while its NATS connection
	// stays open, so the disconnect asserted below can only come from the
	// revoke-on-delete cascade.
	pollWorkerOffline(t, ts, "worker-a", 5*time.Second)

	mustDoJSON(t, http.MethodDelete, apiURL(ts, "/api/v1/workers/worker-a"), nil, "", http.StatusNoContent, nil)

	// The credential revoke inside the delete handler must have disconnected
	// worker A's still-open NATS connection, synchronously.
	select {
	case <-closedA:
	case <-time.After(2 * time.Second):
		t.Fatal("worker A's NATS connection was not closed by deleting its worker record")
	}

	// The worker row itself is gone.
	mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/workers/worker-a"), nil, "", http.StatusNotFound, nil)
}

// TestRevocation_ParkedLeaseGetsNoWork revokes a worker while it has a lease
// request parked (it asked for work when there was none), then submits a job.
// The revoke disconnected the worker, so the parked request must not be
// served: a task leased to it would go to an inbox no one reads and wait for
// the heartbeat sweep. WorkerTimeout is raised so the sweep cannot take the
// worker offline during the test, which would refuse the request for another
// reason.
func TestRevocation_ParkedLeaseGetsNoWork(t *testing.T) {
	so := storeFor(t, t.TempDir()+"/sqi-broker-auth-parked.db")
	seedA, pubA, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed(A): %v", err)
	}
	seedWorkerCredential(t, so, "worker-a", pubA)

	ts := startBrokerAuthServer(t, so, func(cfg *server.Config) {
		cfg.Scheduler.WorkerTimeout = 30 * time.Second
	})
	farmID, queueID := seedFarmAndQueue(t, ts)

	closedA := make(chan struct{})
	workerA := newMockWorkerWithNkey(t, "nats://"+ts.NATSAddr, "worker-a", farmID, queueID, seedA, pubA,
		nats.ClosedHandler(func(*nats.Conn) { close(closedA) }))
	workerA.register()
	workerA.startHeartbeat(200 * time.Millisecond)
	pollWorkerOnline(t, ts, "worker-a", 5*time.Second)

	// A lease request with no work to lease parks on the server. The request
	// itself ends when the revoke closes the connection.
	req, err := json.Marshal(map[string]string{"worker_id": "worker-a"})
	if err != nil {
		t.Fatalf("marshal lease request: %v", err)
	}
	requested := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, reqErr := workerA.nc.RequestWithContext(ctx, bus.WorkLeaseSubject("worker-a", queueID), req)
		requested <- reqErr
	}()
	// The server parks a request that finds no work at once; this leaves it
	// time to arrive. A request that arrived after the revoke instead would be
	// held by the check at the front of the lease handler, so the test would
	// still pass without exercising the parked path, which
	// TestParkedLeaseOfARevokedWorkerGetsNoWork in internal/scheduler pins.
	time.Sleep(500 * time.Millisecond)

	revokeWorkerCredential(t, ts, "worker-a")
	select {
	case <-closedA:
	case <-time.After(2 * time.Second):
		t.Fatal("worker A's NATS connection was not closed by revocation")
	}
	if reqErr := <-requested; reqErr == nil {
		t.Fatal("the parked lease request was answered; want it to end with the closed connection")
	}

	jobID := submitJob(t, ts, farmID, queueID)
	ids := jobTaskIDs(t, ts, jobID)
	if len(ids) != 1 {
		t.Fatalf("job has %d tasks, want 1", len(ids))
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if got := getTaskDetail(t, ts, ids[0]).Status; got != "ready" {
			t.Fatalf("task status %q after the revoke, want ready: the revoked worker's parked request was served", got)
		}
	}
	if attempts := getTaskAttempts(t, ts, ids[0]).Items; len(attempts) != 0 {
		t.Fatalf("task has %d attempts, want 0 (nothing leased to the revoked worker)", len(attempts))
	}
}

// TestEnrollment_ConnectsToRunningBrokerWithoutRestart checks that enrollment
// needs no broker call: the broker's auth callout reads the credential from
// the store when the worker connects, so a worker enrolled over POST
// /workers/enroll against a running server connects without a restart or
// reload. It enrolls a worker over REST after the server is up and asserts it
// connects and registers.
func TestEnrollment_ConnectsToRunningBrokerWithoutRestart(t *testing.T) {
	so := storeFor(t, t.TempDir()+"/sqi-broker-auth-enroll.db")

	rawToken := seedJoinToken(t, so, "worker-c")

	ts := startBrokerAuthServer(t, so, func(cfg *server.Config) {
		cfg.NATS.Auth.EnrollmentEndpointEnabled = true
	})

	farmID, queueID := seedFarmAndQueue(t, ts)

	seedC, pubC, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed(C): %v", err)
	}

	// Enroll after the server has booted and become ready, so the credential
	// did not exist when the broker started; the auth callout must find it in
	// the store at connect time.
	enrollWorker(t, ts, rawToken, "worker-c", pubC)

	natsURL := "nats://" + ts.NATSAddr
	workerC := newMockWorkerWithNkey(t, natsURL, "worker-c", farmID, queueID, seedC, pubC)
	workerC.register()
	workerC.startHeartbeat(200 * time.Millisecond)

	// A rejected connection would already have failed the test in
	// newMockWorkerWithNkey's nats.Connect with "Authorization Violation", so
	// reaching this point shows the enrolled worker connected; this checks it
	// also registered.
	pollWorkerOnline(t, ts, "worker-c", 5*time.Second)
}

// ── The default configuration ───────────────────────────────────────────────

// listActiveWorkerCredentials opens a second, independent handle on the
// database a running server was started against and calls
// ListActiveWorkerCredentials. This is safe while that server is running: on
// SQLite the store opens every database in WAL mode, which
// allows a reader to run alongside the server's own single-connection
// writer, and on PostgreSQL a second handle takes no single-server lock
// (openTestStore leaves ServerLock off). Its migration pass finds the schema
// already current, because the server that owns o has applied every
// migration.
func listActiveWorkerCredentials(t *testing.T, o backend.Options) []store.WorkerCredential {
	t.Helper()
	ctx := context.Background()
	st := openTestStore(t, o)
	defer func() { _ = st.Close() }()

	creds, err := st.ListActiveWorkerCredentials(ctx)
	if err != nil {
		t.Fatalf("listActiveWorkerCredentials: %v", err)
	}
	return creds
}

// TestDefaultConfig_NoBrokerAuth guards the default path: a server and worker
// started with no broker-auth configuration must behave as if broker
// authentication did not exist. The other tests in this file check the
// feature works; this one checks it costs nothing when unused.
//
// A failure here means the default path is broken, which every operator who
// has not configured broker authentication runs: fix the cause, not the test.
//
// TestServerConfig_DefaultsAreTheConfigDefaults and
// TestServerConfig_CarriesTheBrokerAuthSettings in cmd/sqi-server check that
// internal/config's zero-configuration default carries nats.auth.enabled =
// false through the config.Config -> server.Config mapping the serve
// subcommand uses. This test asserts that default as a guard and checks the
// runtime behavior it implies: a real sqi-worker subprocess with no
// credential file and no join token registers, is leased a task, runs it to
// completion, and the worker-credential table, which only broker auth writes
// to, stays empty.
func TestDefaultConfig_NoBrokerAuth(t *testing.T) {
	// Guard the claim the rest of this test depends on: an operator's own
	// zero-configuration default leaves broker authentication off.
	if config.DefaultConfig().NATS.Auth.Enabled {
		t.Fatal("config.DefaultConfig().NATS.Auth.Enabled = true, want false -- " +
			"broker authentication must default to off")
	}

	// startServer boots server.Config with every NATS.Auth field left at its
	// zero value: the state an operator gets with no nats.auth section in
	// their config file, no SQI_NATS_AUTH_* environment variable and no
	// --nats-auth-* flag, matching the config default asserted above.
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	// A real sqi-worker subprocess. startRealWorker sets no join-token,
	// credential-file or server-url environment variable, and blocks until the
	// worker is online: the worker registers with no credential.
	startRealWorker(t, ts, farmID, queueID)

	// It is leased a task and runs it to completion.
	jobID := submitJob(t, ts, farmID, queueID)
	if got := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, 30*time.Second); got != "completed" {
		t.Fatalf("job on the default no-broker-auth path ended %q, want completed", got)
	}

	// Nothing was enrolled: with broker auth off, no row is written to the
	// worker-credential table.
	if creds := listActiveWorkerCredentials(t, ts.Store); len(creds) != 0 {
		t.Errorf("default path created %d worker credentials, want 0: %+v", len(creds), creds)
	}
}
