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
	"github.com/uberware/sqi/internal/config"
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

		// The enrollment endpoint is off by default: most callers enroll by
		// seeding the store directly before boot (see seedWorkerCredential)
		// rather than through POST /workers/enroll. A caller that needs the
		// real REST enrollment surface turns it on via mutate — see
		// TestEnrollment_ConnectsToRunningBrokerWithoutRestart.
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
	// Derive the transport from the config this server was actually given, so
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
		t.Fatal("startBrokerAuthServer: HTTP server did not start listening")
	}
	// Readiness must be probed over whatever scheme this server is actually
	// serving: an https:// listener answers a plaintext GET with a 400, which
	// would look like "never became ready" forever. A TLS caller stamps
	// ts.Scheme/ts.Client before this runs (see startTLSServer in tls_test.go).
	if !waitForReadyzClient(t, clientFor(ts), apiURL(ts, "/readyz"), 10*time.Second) {
		t.Fatal("startBrokerAuthServer: server did not become ready")
	}
	return ts
}

// seedWorkerCredential inserts an already-enrolled [store.WorkerCredential]
// row directly into the SQLite database at dbPath, mirroring what
// "sqi-server worker enroll" (the offline CLI enrollment path) writes — a
// direct store.CreateWorkerCredential call from a process holding no broker
// handle, so (unlike POST /workers/enroll, see enrollWorker below) it never
// reaches a running broker's authorized-key set on its own. Used by
// TestRevocation_DisconnectsAndReclaims, which wants both its workers
// connectable from the moment the server boots and has no other reason to
// exercise the REST enrollment surface. Must run before the server opens
// its own connection to the same file — see startBrokerAuthServer.
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

// seedJoinToken inserts a join token row directly into the SQLite database
// at dbPath and returns the raw token, bypassing POST /workers/join-tokens
// (which requires session auth this suite does not otherwise need — minting
// is not what either enrollment or revocation testing is about here). Must
// run before the server opens its own connection to the same file, same
// rule as seedWorkerCredential.
func seedJoinToken(t *testing.T, dbPath, name string) string {
	t.Helper()
	raw, hash, prefix, err := jointoken.Generate()
	if err != nil {
		t.Fatalf("seedJoinToken: jointoken.Generate: %v", err)
	}

	ctx := context.Background()
	st, err := sqlite.Open(ctx, dbPath, sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("seedJoinToken: sqlite.Open: %v", err)
	}
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
// unauthenticated POST /api/v1/workers/enroll wire protocol — the path that
// reloads a RUNNING broker's authorized-key set rather than only taking
// effect at the server's next start.
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
// The per-worker inbox prefix is not optional decoration: an enrolled
// worker is granted "_INBOX_<id>.>" and nothing wider, so a connection left
// on nats.go's process-global "_INBOX" cannot even subscribe to its own
// reply inbox — every JetStream publish then fails waiting for a PubAck it
// is not allowed to hear. internal/worker/natsclient sets the same option
// on every real worker connection.
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

// TestWorkerDeletion_RevokesCredentialAndDisconnects proves the cascade DELETE
// /api/v1/workers/{id} is now expected to perform: removing a worker record
// also revokes its broker credential, through the SAME synchronous
// store-write-then-broker-reload path DELETE /workers/{id}/credential uses.
// Without it, a machine an operator has just decommissioned from the farm
// keeps live broker access — able to connect, lease work and execute job
// code — because WorkersManage (what deleting a worker requires) does not
// imply WorkersEnroll (what revoking a credential directly requires).
//
// worker-a never heartbeats after registering, so the heartbeat sweep marks
// it offline on its own — the ONLY status DELETE /workers/{id} accepts
// without an extra disable step — while its NATS connection stays live
// (nothing here closes it). The test then deletes the worker over REST and
// asserts the still-open connection is closed by the same
// ReloadCredentials call TestRevocation_DisconnectsAndReclaims already
// proves is synchronous for the dedicated credential-revoke endpoint.
func TestWorkerDeletion_RevokesCredentialAndDisconnects(t *testing.T) {
	sqlitePath := t.TempDir() + "/sqi-broker-auth-delete.db"

	seedA, pubA, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed(A): %v", err)
	}
	seedWorkerCredential(t, sqlitePath, "worker-a", pubA)

	ts := startBrokerAuthServer(t, sqlitePath, nil)
	farmID, queueID := seedFarmAndQueue(t, ts)
	natsURL := "nats://" + ts.NATSAddr

	closedA := make(chan struct{})
	workerA := newMockWorkerWithNkey(t, natsURL, "worker-a", farmID, queueID, seedA, pubA,
		nats.ClosedHandler(func(*nats.Conn) { close(closedA) }))
	workerA.register()
	// Deliberately no startHeartbeat: the worker must go offline in the
	// store (the only status DELETE /workers/{id} accepts here) while its
	// NATS connection stays open, so the disconnect this test asserts on
	// can only be explained by the revoke-on-delete cascade, not by the
	// worker's own connection dying of neglect.
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

// TestEnrollment_ConnectsToRunningBrokerWithoutRestart guards against the
// broker's authorized-key set ever again going unreloaded after POST
// /workers/enroll creates a credential. loadBrokerAuthConfig only ever runs
// once, at Start, so without an explicit reload a worker enrolled against a
// RUNNING server could not actually connect: nats-server would refuse it
// with "Authorization Violation", and the real sqi-worker binary exits
// fatally naming that rejection. This enrolls a worker over the real REST
// wire protocol AFTER the server is already up, with no restart in between,
// and asserts it connects and registers successfully.
func TestEnrollment_ConnectsToRunningBrokerWithoutRestart(t *testing.T) {
	sqlitePath := t.TempDir() + "/sqi-broker-auth-enroll.db"

	rawToken := seedJoinToken(t, sqlitePath, "worker-c")

	ts := startBrokerAuthServer(t, sqlitePath, func(cfg *server.Config) {
		cfg.NATSAuthEnrollmentEndpointEnabled = true
	})

	farmID, queueID := seedFarmAndQueue(t, ts)

	seedC, pubC, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed(C): %v", err)
	}

	// Enroll AFTER the server has already booted and become ready — the
	// broker's initial authorized-key set (built once, at Start) could not
	// possibly contain this credential.
	enrollWorker(t, ts, rawToken, "worker-c", pubC)

	natsURL := "nats://" + ts.NATSAddr
	workerC := newMockWorkerWithNkey(t, natsURL, "worker-c", farmID, queueID, seedC, pubC)
	workerC.register()
	workerC.startHeartbeat(200 * time.Millisecond)

	// pollWorkerOnline itself has no generous fixed sleep baked in beyond its
	// own timeout — a rejected connection here would mean register()'s
	// underlying nats.Connect (inside newMockWorkerWithNkey) already failed
	// the test outright with "Authorization Violation", so reaching this
	// point at all already proves the enrolled worker could connect.
	pollWorkerOnline(t, ts, "worker-c", 5*time.Second)
}

// ── The default-config regression ───────────────────────────────────────────

// listActiveWorkerCredentials opens a second, independent connection to the
// SQLite database a running server was started against and calls
// ListActiveWorkerCredentials. This is safe to run WHILE that server is
// still up: the store opens every database in WAL mode, which allows a
// reader to run alongside the server's own single-connection writer.
// AutoMigrate is left off — the server that owns dbPath has already applied
// every migration, and re-running goose's version check here would be pure
// overhead against a live file for no benefit.
func listActiveWorkerCredentials(t *testing.T, dbPath string) []store.WorkerCredential {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, dbPath, sqlite.Options{AutoMigrate: false})
	if err != nil {
		t.Fatalf("listActiveWorkerCredentials: sqlite.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	creds, err := st.ListActiveWorkerCredentials(ctx)
	if err != nil {
		t.Fatalf("listActiveWorkerCredentials: %v", err)
	}
	return creds
}

// TestDefaultConfig_NoBrokerAuth is the load-bearing regression for the
// whole component: a server and worker started with NO broker-auth
// configuration must behave exactly as they did before broker
// authentication existed. Every other test in this file proves the new
// capability works; this one proves it costs nothing when unused.
//
// If this test fails, the default path has regressed. Fix the cause — never
// adjust the test to accommodate the regression. Every operator who has
// never heard of broker authentication meets this exact path on day one.
//
// This is one half of a two-part proof. The other half — that
// internal/config's own zero-configuration default carries nats.auth.enabled
// = false through to the value the server actually runs with — is pinned at
// the unit level by TestServerConfig_DefaultsAreTheConfigDefaults and
// TestServerConfig_CarriesTheBrokerAuthSettings in cmd/sqi-server, which
// exercise the exact config.Config -> server.Config mapping function the
// serve subcommand uses. This test starts from that already-proven
// conclusion (asserted below as a guard, not re-derived) and proves the
// RUNTIME behavior it implies: a real sqi-worker subprocess with no
// credential file and no join token registers, is leased a task, runs it to
// completion, and the worker-credential table -- which only broker auth
// ever writes to -- stays empty throughout.
func TestDefaultConfig_NoBrokerAuth(t *testing.T) {
	// Guard the claim the rest of this test depends on: an operator's own
	// zero-configuration default leaves broker authentication off.
	if config.DefaultConfig().NATS.Auth.Enabled {
		t.Fatal("config.DefaultConfig().NATS.Auth.Enabled = true, want false -- " +
			"broker authentication must default to off")
	}

	// startServer boots server.Config with every NATSAuth* field left at its
	// zero value: the same "nobody has ever configured this" state an
	// operator gets with no nats.auth section in their config file, no
	// SQI_NATS_AUTH_* environment variable, and no --nats-auth-* flag --
	// matching the config default asserted above.
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	// A real sqi-worker subprocess. startRealWorker sets no join-token,
	// credential-file, or server-url environment variable at all -- exactly
	// as every worker invocation looked before broker auth existed -- and
	// blocks until the worker is visible online, which is this test's proof
	// of criterion 1: the worker registers with no credential.
	startRealWorker(t, ts, farmID, queueID)

	// Criterion 2: it is leased a task and runs it to completion.
	jobID := submitJob(t, ts, farmID, queueID)
	if got := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, 30*time.Second); got != "completed" {
		t.Fatalf("job on the default no-broker-auth path ended %q, want completed", got)
	}

	// Criterion 3: nothing enrolled anything. Broker auth was never engaged
	// on this path, so no row should ever have been written to the
	// worker-credential table.
	if creds := listActiveWorkerCredentials(t, ts.DBPath); len(creds) != 0 {
		t.Errorf("default path created %d worker credentials, want 0: %+v", len(creds), creds)
	}
}
