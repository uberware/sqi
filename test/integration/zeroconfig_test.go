// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// The zero-config farm is the one deployment this work holds fixed: a server
// started with no config file and a worker started with no nats.url must
// still find each other and run a task, whatever changes underneath them in
// the broker. This is its regression test.
//
// The mDNS part binds the test broker to every interface (the advertisement
// carries the machine's hostname, so a loopback-bound broker is unreachable at
// it), so it runs only when discovery testing was asked for explicitly —
// `make test-discovery` — and skips otherwise, exactly as
// TestDiscovery_RealBinaryFindsItsServerOverMDNS does. The rest of the test
// runs everywhere; when the mDNS part is skipped the worker joins by an
// explicit URL instead, so the task still runs.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/server"
)

// zeroConfigJobYAML is the task the farm runs: one echo, as the default
// worker would be asked to.
const zeroConfigJobYAML = `specificationVersion: "jobtemplate-2023-09"
name: Zero Config Farm

steps:
  - name: Run
    script:
      actions:
        onRun:
          command: echo
          args:
            - "hello from the zero-config farm"
`

// TestZeroConfigFarm boots a server from the configuration an operator gets
// with no config file at all and checks the farm the new broker layout leaves
// them: an embedded broker with worker authentication off, single-replica
// streams, no guard bucket, and a worker that finds the server and runs a
// task.
func TestZeroConfigFarm(t *testing.T) {
	// Exactly what the server loads with no config file: built-in defaults
	// only. The per-test addresses and directories below are the only things
	// changed, and they are placement, not behavior.
	loaded, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("config.Load with no file: %v", err)
	}
	if loaded.NATS.Mode != "embedded" {
		t.Fatalf("default nats.mode = %q, want embedded", loaded.NATS.Mode)
	}
	if loaded.NATS.Auth.Enabled {
		t.Fatal("default nats.auth.enabled = true, want false: authentication is opt-in")
	}
	if loaded.NATS.Embedded.TLS.Enabled {
		t.Fatal("default nats.embedded.tls.enabled = true, want false")
	}

	// Whether the mDNS part can run is decided before the server boots,
	// because it changes what the server binds and advertises, and because
	// the foreign-server check must see the network before this test's own
	// server is on it.
	mdnsOK := false
	if multicastRequired() {
		t.Run("mdns preflight", func(t *testing.T) {
			requireMulticast(t)
			noForeignServerOrFail(t)
			mdnsOK = true
		})
		// Under `make test-discovery` the mDNS half is the point; a preflight
		// that ended any other way must not let the parent pass without it.
		if !mdnsOK {
			t.Fatal("SQI_TEST_REQUIRE_MULTICAST is set but the mDNS preflight did not pass")
		}
	}

	instance := ""
	if mdnsOK {
		instance = instanceName(t, "sqi-zero")
	}
	natsPort := freePort(t)
	ts := startServerWith(t, func(cfg *server.Config) {
		embedded := loaded.NATS.Embedded
		embedded.DataDir = cfg.NATS.Embedded.DataDir
		embedded.MaxStoreMB = cfg.NATS.Embedded.MaxStoreMB
		embedded.Addr = fmt.Sprintf("127.0.0.1:%d", natsPort)
		if mdnsOK {
			// Reachable at the advertised hostname.
			embedded.Addr = fmt.Sprintf("0.0.0.0:%d", natsPort)
			cfg.DiscoveryEnabled = true
			cfg.DiscoveryInstanceName = instance
			cfg.DiscoveryInterfaces = loopbackIfaces()
		}
		cfg.NATS = loaded.NATS
		cfg.NATS.Embedded = embedded
	})
	clientURL := fmt.Sprintf("nats://127.0.0.1:%d", natsPort)

	t.Run("embedded broker with authentication off", func(t *testing.T) {
		// An anonymous connection, with no credential of any kind, is
		// accepted: that is what authentication being off means.
		nc, err := nats.Connect(clientURL, nats.Timeout(5*time.Second))
		if err != nil {
			t.Fatalf("anonymous connect to the zero-config broker: %v", err)
		}
		defer nc.Close()
		if _, err := jetstream.New(nc); err != nil {
			t.Fatalf("jetstream on the anonymous connection: %v", err)
		}
	})

	t.Run("streams are single-replica and there is no guard bucket", func(t *testing.T) {
		nc, err := nats.Connect(clientURL, nats.Timeout(5*time.Second))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer nc.Close()
		js, err := jetstream.New(nc)
		if err != nil {
			t.Fatalf("jetstream: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Finding all four streams through an anonymous connection shows
		// they are reachable by unauthenticated workers. That they live in
		// the SQI account is pinned by internal/bus's
		// TestEmbedded_StreamsAreSingleReplicaInSQI, not here: a client
		// cannot read its account's name.
		want := map[string]bool{
			bus.StreamTask: false, bus.StreamLogs: false,
			bus.StreamWorker: false, bus.StreamCancel: false,
		}
		lister := js.ListStreams(ctx)
		for info := range lister.Info() {
			if _, ok := want[info.Config.Name]; ok {
				want[info.Config.Name] = true
			}
			if info.Config.Replicas != 1 {
				t.Errorf("stream %s has %d replicas, want 1", info.Config.Name, info.Config.Replicas)
			}
			if strings.HasPrefix(info.Config.Name, "KV_") {
				t.Errorf("stream %s is a KV bucket; the embedded broker keeps none", info.Config.Name)
			}
		}
		if err := lister.Err(); err != nil {
			t.Fatalf("list streams: %v", err)
		}
		for name, found := range want {
			if !found {
				t.Errorf("stream %s is not provisioned", name)
			}
		}

		// The active-server guard exists only against an external broker.
		if _, err := js.KeyValue(ctx, "SQI_SERVER"); !errors.Is(err, jetstream.ErrBucketNotFound) {
			t.Errorf("KeyValue(SQI_SERVER) error = %v, want ErrBucketNotFound: an embedded broker has no guard bucket", err)
		}
	})

	farmID, queueID := seedFarmAndQueue(t, ts)

	// The worker belongs to the whole test, not to the subtest that starts it:
	// the process is stopped by the cleanup of the t it is started with, and
	// the task subtest below needs it still running.
	farm := t
	var workerID string
	t.Run("worker finds the server over mDNS", func(t *testing.T) {
		if !mdnsOK {
			t.Skip("binds the test broker to all interfaces; run `make test-discovery` to include it")
		}
		// No nats.url at all: the harness default is blanked, so mDNS is the
		// only way this worker can find its broker.
		startRealWorkerNoWait(farm, ts, farmID, queueID, nil, []string{
			"SQI_WORKER_NATS_URL=",
			"SQI_WORKER_DISCOVERY_ENABLE_MDNS=true",
			"SQI_WORKER_DISCOVERY_MDNS_TIMEOUT=15s",
		}, false)
		workerID = findOnlineWorker(t, ts, farmID, 60*time.Second)
		if workerID == "" {
			t.Fatal("worker with no nats.url never came online: it did not find the zero-config server over mDNS")
		}
	})

	t.Run("worker runs a script task to succeeded", func(t *testing.T) {
		if workerID == "" {
			if multicastRequired() {
				t.Fatal("SQI_TEST_REQUIRE_MULTICAST is set but the worker did not join over mDNS; " +
					"the explicit-URL fallback exists only for the default run")
			}
			// The mDNS part was skipped; join by URL so the farm still runs
			// the task.
			t.Log("joining by explicit URL: the mDNS part did not run")
			workerID = startRealWorker(farm, ts, farmID, queueID)
		}

		url := fmt.Sprintf("%s?farm_id=%s&queue_id=%s&owner=zero-config", apiURL(ts, "/api/v1/jobs"), farmID, queueID)
		var jobResp struct {
			ID string `json:"id"`
		}
		mustDoJSON(t, http.MethodPost, url, []byte(zeroConfigJobYAML), "application/x-yaml", http.StatusCreated, &jobResp)
		if jobResp.ID == "" {
			t.Fatal("server returned an empty job ID")
		}
		if got := pollJobStatus(t, ts, jobResp.ID, []string{"completed", "failed", "canceled"}, 60*time.Second); got != "completed" {
			t.Fatalf("job final status = %q, want completed", got)
		}

		taskID := firstTaskID(t, ts, jobResp.ID)
		pollTaskLogs(t, ts, taskID, "hello from the zero-config farm", 15*time.Second)

		var task struct {
			Status           string `json:"status"`
			AssignedWorkerID string `json:"assigned_worker_id"`
		}
		mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/tasks/"+taskID), nil, "", http.StatusOK, &task)
		if task.Status != "succeeded" {
			t.Errorf("task status = %q, want succeeded", task.Status)
		}
		if task.AssignedWorkerID != workerID {
			t.Errorf("task ran on %q, want the farm's worker %q", task.AssignedWorkerID, workerID)
		}
	})
}
