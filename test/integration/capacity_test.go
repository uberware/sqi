// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// capacity_test.go — capacity groups end to end against the real binaries: the
// scaling signal a sized group exports for queued work, a cloud worker joining
// it as an ephemeral member, a spot preemption handing that worker's tasks
// back and removing it, and, with authentication on, joining a group by
// enrolling with its token.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/server"
	"github.com/uberware/sqi/internal/store"
)

// capacitySweepInterval is the heartbeat-sweep tick these tests run the server
// at; the capacity evaluation runs once per tick.
const capacitySweepInterval = 500 * time.Millisecond

// capacityJobYAML is a six-task job whose tasks each declare one core and need
// a worker tagged special=yes. Each task waits until the file gate exists, so
// a task stays running until the test releases it.
func capacityJobYAML(gate string) string {
	return fmt.Sprintf(`specificationVersion: "jobtemplate-2023-09"
name: Capacity Group Test
steps:
  - name: Render
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "1-6"
    hostRequirements:
      amounts:
        - name: amount.worker.vcpu
          min: 1
      attributes:
        - name: attr.worker.tag.special
          anyOf: ["yes"]
    script:
      actions:
        onRun:
          command: sh
          args:
            - "-c"
            - 'while [ ! -e "$1" ]; do sleep 0.2; done'
            - "sh"
            - %q
`, gate)
}

// ── Wire types ───────────────────────────────────────────────────────────────

// capacityStatusWire mirrors internal/api's capacityStatusResponse.
type capacityStatusWire struct {
	DemandCores     int       `json:"demand_cores"`
	DesiredMachines int       `json:"desired_machines"`
	OnlineMembers   int       `json:"online_members"`
	OfflineMembers  int       `json:"offline_members"`
	EvaluatedAt     time.Time `json:"evaluated_at"`
}

// capacityGroupWire is the subset of internal/api's capacityGroupResponse
// these tests read.
type capacityGroupWire struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	Status *capacityStatusWire `json:"status"`
}

// capacityTask is the subset of a task these tests read.
type capacityTask struct {
	ID                  string `json:"id"`
	Status              string `json:"status"`
	AssignedWorkerID    string `json:"assigned_worker_id"`
	FailureReason       string `json:"failure_reason"`
	FailedAttempts      int    `json:"failed_attempts"`
	UnschedulableReason string `json:"unschedulable_reason"`
}

// ── REST helpers ─────────────────────────────────────────────────────────────

// createCapacityGroup creates a capacity group from body and returns it.
func createCapacityGroup(t *testing.T, ts *testServer, body map[string]any) capacityGroupWire {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal capacity group: %v", err)
	}
	var g capacityGroupWire
	mustDoJSONClient(t, clientFor(ts), http.MethodPost, apiURL(ts, "/api/v1/capacity-groups"),
		raw, "application/json", http.StatusCreated, &g)
	if g.ID == "" {
		t.Fatal("POST /capacity-groups returned no id")
	}
	return g
}

// sizedCloudGroup is the body of the sized group "cloud" these tests create:
// four-CPU machines tagged special=yes, serving queueID, up to four machines
// added two at a time.
func sizedCloudGroup(farmID, queueID string) map[string]any {
	return map[string]any{
		"farm_id":      farmID,
		"name":         "cloud",
		"kind":         "sized",
		"provider":     "external",
		"queue_ids":    []string{queueID},
		"min_machines": 0,
		"max_machines": 4,
		"max_step_up":  2,
		"machine_profile": map[string]any{
			"cpu_count": 4,
			"tags":      map[string]string{"special": "yes"},
		},
	}
}

// getCapacityGroup reads a capacity group with its status.
func getCapacityGroup(t *testing.T, ts *testServer, id string) capacityGroupWire {
	t.Helper()
	var g capacityGroupWire
	mustDoJSONClient(t, clientFor(ts), http.MethodGet, apiURL(ts, "/api/v1/capacity-groups/"+id),
		nil, "", http.StatusOK, &g)
	return g
}

// waitCapacityStatus polls the group until an evaluation made after since
// satisfies want, and returns it. It fails the test, naming what and the last
// status seen, when none does within timeout.
func waitCapacityStatus(t *testing.T, ts *testServer, id string, since time.Time, timeout time.Duration,
	what string, want func(capacityStatusWire) bool,
) capacityStatusWire {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *capacityStatusWire
	for {
		g := getCapacityGroup(t, ts, id)
		if s := g.Status; s != nil {
			last = s
			if s.EvaluatedAt.After(since) && want(*s) {
				return *s
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("capacity group %s: no evaluation after %s with %s within %s; last status: %+v",
				id, since.Format(time.RFC3339Nano), what, timeout, last)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// capacityMemberIDs returns the IDs of the group's members.
func capacityMemberIDs(t *testing.T, ts *testServer, id string) []string {
	t.Helper()
	var members []struct {
		ID string `json:"id"`
	}
	mustDoJSONClient(t, clientFor(ts), http.MethodGet, apiURL(ts, "/api/v1/capacity-groups/"+id+"/members"),
		nil, "", http.StatusOK, &members)
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.ID
	}
	return ids
}

// waitCapacityMember polls the group's members until workerID is one of them.
func waitCapacityMember(t *testing.T, ts *testServer, groupID, workerID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ids := capacityMemberIDs(t, ts, groupID)
		if slices.Contains(ids, workerID) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker %s did not become a member of capacity group %s within %s; members: %v",
				workerID, groupID, timeout, ids)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// capacityJobTasks returns every task of jobID.
func capacityJobTasks(t *testing.T, ts *testServer, jobID string) []capacityTask {
	t.Helper()
	var resp struct {
		Items []capacityTask `json:"items"`
	}
	mustDoJSONClient(t, clientFor(ts), http.MethodGet, apiURL(ts, "/api/v1/jobs/"+jobID+"/tasks?limit=1000"),
		nil, "", http.StatusOK, &resp)
	return resp.Items
}

// workerStatusCode returns the HTTP status of GET /workers/{id}.
func workerStatusCode(t *testing.T, ts *testServer, workerID string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, apiURL(ts, "/api/v1/workers/"+workerID), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := clientFor(ts).Do(req)
	if err != nil {
		t.Fatalf("GET /workers/%s: %v", workerID, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// findOtherOnlineWorker returns the ID of an online worker in farmID other
// than exclude, or fails the test after timeout.
func findOtherOnlineWorker(t *testing.T, ts *testServer, farmID, exclude string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var resp struct {
			Items []struct {
				ID     string `json:"id"`
				FarmID string `json:"farm_id"`
				Status string `json:"status"`
			} `json:"items"`
		}
		mustDoJSONClient(t, clientFor(ts), http.MethodGet, apiURL(ts, "/api/v1/workers"), nil, "", http.StatusOK, &resp)
		for _, w := range resp.Items {
			if w.FarmID == farmID && w.Status == "online" && w.ID != exclude {
				return w.ID
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no online worker other than %q in farm %s within %s", exclude, farmID, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// preemptionDiagnostics describes a preemption that did not settle: the
// member's row, every attempt of each task not back in ready, the server's
// INFO-and-above log, so the order of the reclaim, the deregister and any
// lease after it can be read, and the stacks of the server's goroutines. A
// member process still running is sent SIGQUIT, which makes the Go runtime
// print its goroutines to the test's stderr, so a worker stuck in its
// shutdown shows where.
func preemptionDiagnostics(
	t *testing.T, ts *testServer, db store.Store, member *exec.Cmd, workerID string, tasks []capacityTask,
) string {
	t.Helper()
	var b strings.Builder
	// The process may have exited and not been reaped yet (the cleanup owns
	// Wait), which a signal cannot tell from a running one.
	if err := member.Process.Signal(syscall.Signal(3)); err == nil {
		fmt.Fprintf(&b, "\n--- sent SIGQUIT to worker process %d: if it was still running, its stacks are on stderr",
			member.Process.Pid)
	}
	if w, err := db.GetWorker(t.Context(), workerID); err != nil {
		fmt.Fprintf(&b, "\n--- worker %s: %v", workerID, err)
	} else {
		fmt.Fprintf(&b, "\n--- worker %s: status %s, ephemeral %v, group %q, last heartbeat %v, updated %s",
			w.ID, w.Status, w.Ephemeral, w.CapacityGroupID, w.LastHeartbeatAt, w.UpdatedAt.Format(time.RFC3339Nano))
	}
	for _, task := range tasks {
		if task.Status == "ready" {
			continue
		}
		var raw json.RawMessage
		mustDoJSONClient(t, clientFor(ts), http.MethodGet, apiURL(ts, "/api/v1/tasks/"+task.ID+"/attempts"),
			nil, "", http.StatusOK, &raw)
		fmt.Fprintf(&b, "\n--- task %s (%s) attempts: %s", task.ID, task.Status, raw)
	}
	if ts.Logs != nil {
		fmt.Fprintf(&b, "\n--- server log (offsets from %s) ---\n%s",
			ts.Logs.start.UTC().Format(time.RFC3339Nano), ts.Logs.trailText())
	}
	fmt.Fprintf(&b, "\n--- server goroutines ---\n%s", sqiGoroutines())
	return b.String()
}

// ── Fake instance metadata service ───────────────────────────────────────────

// fakeIMDS answers the two IMDSv2 requests the worker's aws preemption watcher
// makes: a session token, and the spot instance-action document, which is
// absent (404) until notice is set.
type fakeIMDS struct {
	srv    *httptest.Server
	notice atomic.Bool
	polls  atomic.Int64
}

func newFakeIMDS(t *testing.T) *fakeIMDS {
	t.Helper()
	const token = "capacity-test-token"
	f := &fakeIMDS{}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /latest/api/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, token) //nolint:errcheck // a failed write fails the worker's poll, which retries
	})
	mux.HandleFunc("GET /latest/meta-data/spot/instance-action", func(w http.ResponseWriter, r *http.Request) {
		f.polls.Add(1)
		if r.Header.Get("X-aws-ec2-metadata-token") != token {
			http.Error(w, "missing session token", http.StatusUnauthorized)
			return
		}
		if !f.notice.Load() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"action":"terminate","time":"2026-11-08T00:00:00Z"}`) //nolint:errcheck // as above
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// ── The signal, an ephemeral member, a preemption ────────────────────────────

// TestCapacityGroup_SignalAndEphemeralLifecycle runs a sized group through its
// life with authentication off:
//
//  1. Six one-core tasks queued for the group's four-CPU profile, with nothing
//     online, make it ask for two machines, over REST and on /metrics.
//  2. A worker naming the group in its configuration joins it as an ephemeral
//     member and runs the tasks.
//  3. A spot preemption notice hands the worker's tasks back as a reclaim, not
//     a failure, and removes the worker for good.
//  4. Once another worker drains the job, the group's demand is zero.
func TestCapacityGroup_SignalAndEphemeralLifecycle(t *testing.T) {
	ts := startServerWith(t, func(c *server.Config) {
		c.Scheduler.HeartbeatSweepInterval = capacitySweepInterval
		// The server's default grace would hold the liveness sweeps off for
		// longer than this test runs.
		c.Scheduler.BusReconnectGrace = 2 * time.Second
	})
	farmID, queueID := seedFarmAndQueue(t, ts)
	group := createCapacityGroup(t, ts, sizedCloudGroup(farmID, queueID))

	// ── The signal, with nothing online ──────────────────────────────────────
	gate := filepath.Join(t.TempDir(), "release")
	submitted := time.Now().UTC()
	jobID := submitJobWithParams(t, ts, farmID, queueID, capacityJobYAML(gate), nil)

	st := waitCapacityStatus(t, ts, group.ID, submitted, 2*capacitySweepInterval+5*time.Second,
		"demand_cores 6 and desired_machines 2", func(s capacityStatusWire) bool {
			return s.DemandCores == 6 && s.DesiredMachines == 2
		})
	t.Logf("signal after %s: %+v", time.Since(submitted).Round(time.Millisecond), st)
	if st.OnlineMembers != 0 || st.OfflineMembers != 0 {
		t.Errorf("members before any worker: online %d, offline %d; want 0 and 0", st.OnlineMembers, st.OfflineMembers)
	}
	// The gauges are set before the status the API reads is stored.
	if v := metricValue(t, ts, `sqi_capacity_desired_machines{group="cloud"}`); v != 2 {
		t.Errorf(`sqi_capacity_desired_machines{group="cloud"} = %v, want 2`, v)
	}
	if v := metricValue(t, ts, `sqi_capacity_demand_cores{group="cloud"}`); v != 6 {
		t.Errorf(`sqi_capacity_demand_cores{group="cloud"} = %v, want 6`, v)
	}

	// ── A cloud worker joins as an ephemeral member ──────────────────────────
	imds := newFakeIMDS(t)
	member := startRealWorkerNoWait(t, ts, farmID, queueID, nil, []string{
		"SQI_WORKER_CAPACITY_GROUP=cloud",
		"SQI_WORKER_CAPABILITY_TAGS=special=yes",
		"SQI_WORKER_PREEMPTION_WATCH=aws",
		"SQI_WORKER_PREEMPTION_POLL_INTERVAL=1s",
		"SQI_WORKER_PREEMPTION_ENDPOINT=" + imds.srv.URL,
	}, false)
	memberID := pollForOnlineWorker(t, ts, farmID, 30*time.Second)
	waitCapacityMember(t, ts, group.ID, memberID, 10*time.Second)

	// The REST worker shape carries no membership fields, so the ephemeral
	// flag is read from the store.
	db := openTestStore(t, ts.Store)
	w, err := db.GetWorker(t.Context(), memberID)
	if err != nil {
		t.Fatalf("read worker %s from the store: %v", memberID, err)
	}
	if !w.Ephemeral || w.CapacityGroupID != group.ID {
		t.Fatalf("worker %s: ephemeral %v, capacity group %q; want true and %q", memberID, w.Ephemeral, w.CapacityGroupID, group.ID)
	}

	// The member runs the work.
	var held []string
	deadline := time.Now().Add(30 * time.Second)
	for len(held) == 0 {
		for _, task := range capacityJobTasks(t, ts, jobID) {
			if task.Status == "running" && task.AssignedWorkerID == memberID {
				held = append(held, task.ID)
			}
		}
		if len(held) == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("no task of job %s started running on member %s within 30s", jobID, memberID)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	t.Logf("member %s is running %d task(s)", memberID, len(held))

	// The watcher is polling, so the notice below is what it acts on.
	deadline = time.Now().Add(10 * time.Second)
	for imds.polls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the worker's preemption watcher never polled the fake metadata service")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// ── Preemption: the tasks come back, the worker goes ─────────────────────
	imds.notice.Store(true)
	noticed := time.Now()
	deadline = noticed.Add(10 * time.Second)
	for {
		tasks := capacityJobTasks(t, ts, jobID)
		allReady := len(tasks) == 6
		for _, task := range tasks {
			allReady = allReady && task.Status == "ready"
		}
		gone := workerStatusCode(t, ts, memberID) == http.StatusNotFound
		if allReady && gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("10s after the preemption notice: every task ready = %v (tasks: %+v), worker %s gone = %v%s",
				allReady, tasks, memberID, gone, preemptionDiagnostics(t, ts, db, member, memberID, tasks))
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("tasks back and worker removed %s after the notice", time.Since(noticed).Round(time.Millisecond))

	// A reclaim, not a failure: nothing counted against the tasks or the job.
	reclaimMessages := []string{store.FailureReasonWorkerShutdown, store.FailureReasonWorkerOffline}
	for _, task := range capacityJobTasks(t, ts, jobID) {
		if task.FailedAttempts != 0 || task.FailureReason != "" {
			t.Errorf("task %s after preemption: failed_attempts %d, failure_reason %q; want 0 and none",
				task.ID, task.FailedAttempts, task.FailureReason)
		}
	}
	for _, id := range held {
		attempts := taskAttempts(t, ts, id)
		if len(attempts) == 0 {
			t.Errorf("task %s ran on the member but has no attempt", id)
		}
		for _, a := range attempts {
			if !slices.Contains(reclaimMessages, a.Message) {
				t.Errorf("task %s attempt %d closed with %q (status %q); want a reclaim, one of %q",
					id, a.AttemptNumber, a.Message, a.Status, reclaimMessages)
			}
		}
	}
	if job := getJobDetailPark(t, ts, jobID); job.FailedAttempts != 0 || job.Status == "failed" {
		t.Errorf("job %s after preemption: status %q, failed_attempts %d; want not failed and 0",
			jobID, job.Status, job.FailedAttempts)
	}
	if ids := capacityMemberIDs(t, ts, group.ID); len(ids) != 0 {
		t.Errorf("capacity group members after the preemption: %v, want none", ids)
	}

	// ── Another worker drains the job; the demand goes to zero ───────────────
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatalf("release the tasks: %v", err)
	}
	plain := startRealWorkerNoWait(t, ts, farmID, queueID, nil, []string{
		"SQI_WORKER_CAPABILITY_TAGS=special=yes",
	}, false)
	plainID := pollForOnlineWorker(t, ts, farmID, 30*time.Second)
	if final := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, 60*time.Second); final != "completed" {
		t.Fatalf("job %s ended %q, want completed", jobID, final)
	}

	// With the job drained and nothing online to absorb demand, the
	// evaluation reads an empty queue.
	if err := plain.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("stop the second worker: %v", err)
	}
	pollWorkerOffline(t, ts, plainID, 30*time.Second)
	drained := time.Now().UTC()
	st = waitCapacityStatus(t, ts, group.ID, drained, 2*capacitySweepInterval+5*time.Second,
		"demand_cores 0", func(s capacityStatusWire) bool { return s.DemandCores == 0 })
	t.Logf("after the drain: %+v", st)
	if v := metricValue(t, ts, `sqi_capacity_demand_cores{group="cloud"}`); v != 0 {
		t.Errorf(`sqi_capacity_demand_cores{group="cloud"} = %v after the drain, want 0`, v)
	}
}

// ── Joining by group token, with authentication on ───────────────────────────

// sessionTransport sends every request with a session cookie and a
// same-origin Origin header, which the CSRF guard requires of a
// cookie-authenticated mutation.
type sessionTransport struct {
	base   http.RoundTripper
	cookie *http.Cookie
	origin string
}

func (s sessionTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.AddCookie(s.cookie)
	r.Header.Set("Origin", s.origin)
	return s.base.RoundTrip(r)
}

// sessionClient logs in as username and returns a client that sends the
// session with every request.
func sessionClient(t *testing.T, ts *testServer, cookieName, username, password string) *http.Client {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, apiURL(ts, "/api/v1/auth/login"), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST /auth/login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login as %s: HTTP %d, want 200", username, resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			return &http.Client{
				Timeout:   10 * time.Second,
				Transport: sessionTransport{base: http.DefaultTransport, cookie: c, origin: "http://" + ts.HTTPAddr},
			}
		}
	}
	t.Fatalf("login as %s set no %s cookie", username, cookieName)
	return nil
}

// enrollStatus posts an enrollment and returns its HTTP status.
func enrollStatus(t *testing.T, ts *testServer, joinToken, workerID, publicKey string) int {
	t.Helper()
	body, err := json.Marshal(map[string]string{"join_token": joinToken, "worker_id": workerID, "public_key": publicKey})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, apiURL(ts, "/api/v1/workers/enroll"), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST /workers/enroll: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// newPublicKey returns a fresh worker public key.
func newPublicKey(t *testing.T) string {
	t.Helper()
	_, pub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	return pub
}

// TestCapacityGroup_GroupTokenEnrollment checks joining a sized group by its
// token with REST and broker authentication both on: a real worker enrolled
// with a group token registers as an ephemeral member; the token is reusable
// until revoked and refused after; and a worker enrolled with an ordinary join
// token does not join a group by naming it in its configuration.
func TestCapacityGroup_GroupTokenEnrollment(t *testing.T) {
	const (
		cookieName    = "sqi_session"
		adminUser     = "admin"
		adminPassword = "capacity-test-admin-password"
	)
	httpAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	ts := startAuthServer(t, httpAddr, "sqi-capacity-auth.db", func(c *server.Config) {
		c.AuthCookieName = cookieName
		c.AuthBootstrapUsername = adminUser
		c.AuthBootstrapPassword = adminPassword
		// A hand-built Config skips config defaults: the auth callout needs a
		// positive user JWT lifetime and a minted join token a positive TTL.
		c.NATS.Auth = config.NATSAuthConfig{
			Enabled:                   true,
			UserTTL:                   time.Hour,
			JoinTokenTTL:              time.Hour,
			JoinTokenSingleUse:        true,
			EnrollmentEndpointEnabled: true,
		}
		c.Scheduler.HeartbeatSweepInterval = capacitySweepInterval
		c.Scheduler.BusReconnectGrace = 2 * time.Second
	})
	ts.Client = sessionClient(t, ts, cookieName, adminUser, adminPassword)

	farmID, queueID := seedFarmAndQueue(t, ts)
	group := createCapacityGroup(t, ts, sizedCloudGroup(farmID, queueID))

	var minted struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	mustDoJSONClient(t, clientFor(ts), http.MethodPost, apiURL(ts, "/api/v1/capacity-groups/"+group.ID+"/tokens"),
		[]byte(`{"name":"capacity-test"}`), "application/json", http.StatusCreated, &minted)
	if minted.ID == "" || minted.Token == "" {
		t.Fatalf("minted group token has no id or token: %+v", minted)
	}

	// ── A real worker enrolls with the group token and joins ─────────────────
	startRealWorkerNoWait(t, ts, farmID, queueID, nil, []string{
		"SQI_WORKER_NATS_SERVER_URL=http://" + ts.HTTPAddr,
		"SQI_WORKER_NATS_JOIN_TOKEN=" + minted.Token,
		"SQI_WORKER_CAPABILITY_TAGS=special=yes",
	}, false)
	memberID := findOnlineWorker(t, ts, farmID, 60*time.Second)
	if memberID == "" {
		t.Fatal("the worker enrolled with the group token never came online")
	}
	waitCapacityMember(t, ts, group.ID, memberID, 10*time.Second)

	db := openTestStore(t, ts.Store)
	w, err := db.GetWorker(t.Context(), memberID)
	if err != nil {
		t.Fatalf("read worker %s from the store: %v", memberID, err)
	}
	if !w.Ephemeral || w.CapacityGroupID != group.ID {
		t.Fatalf("worker %s: ephemeral %v, capacity group %q; want true and %q", memberID, w.Ephemeral, w.CapacityGroupID, group.ID)
	}
	st := waitCapacityStatus(t, ts, group.ID, time.Now().UTC(), 2*capacitySweepInterval+5*time.Second,
		"one online member", func(s capacityStatusWire) bool { return s.OnlineMembers == 1 })
	t.Logf("group with its member: %+v", st)

	// ── Reusable until revoked, refused after ────────────────────────────────
	if code := enrollStatus(t, ts, minted.Token, "capacity-reuse", newPublicKey(t)); code != http.StatusCreated {
		t.Fatalf("second enrollment with the live group token: HTTP %d, want 201 (group tokens are reusable)", code)
	}
	mustDoJSONClient(t, clientFor(ts), http.MethodDelete,
		apiURL(ts, "/api/v1/capacity-groups/"+group.ID+"/tokens/"+minted.ID), nil, "", http.StatusNoContent, nil)
	if code := enrollStatus(t, ts, minted.Token, "capacity-after-revoke", newPublicKey(t)); code != http.StatusUnauthorized {
		t.Fatalf("enrollment with the revoked group token: HTTP %d, want 401", code)
	}

	// ── An ordinary join token cannot join a group by configuration ──────────
	var plainToken struct {
		Token string `json:"token"`
	}
	mustDoJSONClient(t, clientFor(ts), http.MethodPost, apiURL(ts, "/api/v1/workers/join-tokens"),
		[]byte(`{"name":"capacity-plain"}`), "application/json", http.StatusCreated, &plainToken)
	startRealWorkerNoWait(t, ts, farmID, queueID, nil, []string{
		"SQI_WORKER_NATS_SERVER_URL=http://" + ts.HTTPAddr,
		"SQI_WORKER_NATS_JOIN_TOKEN=" + plainToken.Token,
		"SQI_WORKER_CAPACITY_GROUP=cloud",
	}, false)
	plainID := findOtherOnlineWorker(t, ts, farmID, memberID, 60*time.Second)
	// Membership is written right after the registration; give it several
	// heartbeats to appear before concluding it never will.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ids := capacityMemberIDs(t, ts, group.ID); slices.Contains(ids, plainID) {
			t.Fatalf("worker %s, enrolled with an ordinary join token, joined capacity group %s by naming it: members %v",
				plainID, group.ID, ids)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if pw, err := db.GetWorker(t.Context(), plainID); err != nil {
		t.Fatalf("read worker %s from the store: %v", plainID, err)
	} else if pw.CapacityGroupID != "" || pw.Ephemeral {
		t.Fatalf("worker %s: capacity group %q, ephemeral %v; want none and false", plainID, pw.CapacityGroupID, pw.Ephemeral)
	}
}
