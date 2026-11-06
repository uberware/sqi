// SPDX-License-Identifier: AGPL-3.0-or-later

package bus_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/bus/bustest"
)

// serverDurables are the durable consumers a running sqi-server keeps, by
// the stream each reads.
var serverDurables = map[string]string{
	"sqi-task-status-srv": bus.StreamTask,
	"sqi-task-logs-srv":   bus.StreamLogs,
	"sqi-worker-srv":      bus.StreamWorker,
}

// farmState has r's server-side durables created, as a running server does,
// and one message in every stream.
func farmState(t *testing.T, r *bus.Remote) {
	t.Helper()
	client, err := r.NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()
	ctx := t.Context()
	consumers := []func() (jetstream.ConsumeContext, error){
		func() (jetstream.ConsumeContext, error) { return client.ConsumeTaskStatus(ctx, func(jetstream.Msg) {}) },
		func() (jetstream.ConsumeContext, error) { return client.ConsumeTaskLogs(ctx, func(jetstream.Msg) {}) },
		func() (jetstream.ConsumeContext, error) { return client.ConsumeWorker(ctx, func(jetstream.Msg) {}) },
	}
	for _, consume := range consumers {
		cc, err := consume()
		if err != nil {
			t.Fatal(err)
		}
		cc.Stop()
	}
	publishes := []func() error{
		func() error { return client.PublishTaskStatus(ctx, "w1", "j1", []byte("status")) },
		func() error { return client.PublishTaskLog(ctx, "w1", "t1", []byte("log")) },
		func() error { return client.PublishWorkerHeartbeat(ctx, "w1", []byte("heartbeat")) },
		func() error { return client.PublishTaskCancel(ctx, "w1", []byte("cancel")) },
	}
	for _, publish := range publishes {
		if err := publish(); err != nil {
			t.Fatal(err)
		}
	}
}

// purgeForRestore holds r's account for a restore, purges it and gives it
// back, as "sqi-server restore" does around the database steps.
func purgeForRestore(t *testing.T, r *bus.Remote) error {
	t.Helper()
	lock, err := r.LockForRestore(t.Context())
	if err != nil {
		return err
	}
	defer lock.Release()
	return lock.Purge(t.Context())
}

func TestRestoreLock_EmptiesStreamsAndDeletesConsumers(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 1, bustest.Options{})
	logger, _ := bufLogger()
	cfg := remoteConfig(c, bus.DefaultAccount, 1)
	server := startRemote(t, cfg, logger)
	farmState(t, server)
	server.Shutdown()

	if err := purgeForRestore(t, bus.NewRemote(cfg, logger)); err != nil {
		t.Fatalf("lock, Purge, Release: %v", err)
	}

	js := dataJS(t, c)
	for _, name := range allStreams {
		if n := streamInfo(t, js, name).State.Msgs; n != 0 {
			t.Errorf("%s holds %d messages after the purge, want 0", name, n)
		}
	}
	for durable, stream := range serverDurables {
		if _, err := js.Consumer(t.Context(), stream, durable); !errors.Is(err, jetstream.ErrConsumerNotFound) {
			t.Errorf("consumer %s on %s after the purge: %v, want ErrConsumerNotFound", durable, stream, err)
		}
	}
	// The guard was given back: a server starts at once.
	startRemote(t, cfg, logger)
}

func TestRestoreLock_RefusedWhileServerRunning(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 1, bustest.Options{})
	logger, _ := bufLogger()
	cfg := remoteConfig(c, bus.DefaultAccount, 1)
	server := startRemote(t, cfg, logger)
	farmState(t, server)

	err := purgeForRestore(t, bus.NewRemote(cfg, logger))
	host, herr := os.Hostname()
	if herr != nil {
		t.Fatal(herr)
	}
	if !errors.Is(err, bus.ErrGuardHeld) || !strings.Contains(err.Error(), host) {
		t.Fatalf("lock, Purge, Release beside a running server = %v, want ErrGuardHeld naming host %q", err, host)
	}

	js := dataJS(t, c)
	for _, name := range allStreams {
		if n := streamInfo(t, js, name).State.Msgs; n != 1 {
			t.Errorf("%s holds %d messages after a refused purge, want 1", name, n)
		}
	}
	for durable, stream := range serverDurables {
		if _, err := js.Consumer(t.Context(), stream, durable); err != nil {
			t.Errorf("consumer %s on %s after a refused purge: %v", durable, stream, err)
		}
	}
	if err := server.Check(t.Context()); err != nil {
		t.Errorf("the running server's Check after a refused purge = %v, want nil", err)
	}
}

// A server started while a restore holds the account is refused, and the
// refusal says a restore holds it.
func TestLockForRestore_RefusesAServerNamingTheRestore(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 1, bustest.Options{})
	logger, _ := bufLogger()
	cfg := remoteConfig(c, bus.DefaultAccount, 1)

	lock, err := bus.NewRemote(cfg, logger).LockForRestore(t.Context())
	if err != nil {
		t.Fatalf("LockForRestore: %v", err)
	}
	server := bus.NewRemote(cfg, logger)
	err = server.Start(t.Context())
	server.Shutdown()
	if !errors.Is(err, bus.ErrGuardHeld) || !strings.Contains(err.Error(), "restore") {
		t.Fatalf("Start during a restore = %v, want ErrGuardHeld naming the restore", err)
	}

	lock.Release()
	startRemote(t, cfg, logger)
}

// A purge on an account no server has started on yet finds no streams and
// no consumers, which is not an error.
func TestRestoreLock_FreshAccount(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 1, bustest.Options{})
	logger, _ := bufLogger()
	if err := purgeForRestore(t, bus.NewRemote(remoteConfig(c, bus.DefaultAccount, 1), logger)); err != nil {
		t.Fatalf("lock, Purge, Release on a fresh account: %v", err)
	}
	js := dataJS(t, c)
	for _, name := range allStreams {
		if _, err := js.Stream(t.Context(), name); !errors.Is(err, jetstream.ErrStreamNotFound) {
			t.Errorf("stream %s after purging a fresh account: %v, want ErrStreamNotFound", name, err)
		}
	}
}

// A restore configured with a stream_replicas other than the farm's changes
// only the messages: rescaling is the server's job.
func TestRestoreLock_LeavesReplicasAlone(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	logger, _ := bufLogger()
	server := startRemote(t, remoteConfig(c, bus.DefaultAccount, 3), logger)
	server.Shutdown()

	if err := purgeForRestore(t, bus.NewRemote(remoteConfig(c, bus.DefaultAccount, 1), logger)); err != nil {
		t.Fatalf("lock, Purge, Release: %v", err)
	}
	js := dataJS(t, c)
	for _, name := range append(allStreams, "KV_"+bus.GuardBucket) {
		if got := streamInfo(t, js, name).Config.Replicas; got != 3 {
			t.Errorf("%s: replicas = %d after a purge configured for 1, want 3", name, got)
		}
	}
}

func TestRestoreLock_UnreachableFails(t *testing.T) {
	t.Parallel()
	logger, _ := bufLogger()
	err := purgeForRestore(t, bus.NewRemote(bus.RemoteConfig{
		URLs: closedPorts(t, 1), Account: bus.DefaultAccount, StreamReplicas: 1, ConnectTimeout: 500 * time.Millisecond,
	}, logger))
	if err == nil {
		t.Fatal("lock, Purge, Release with no broker listening = nil, want an error")
	}
}

// RestoreTargets names what a purge empties, for the operator who must do it
// by hand.
func TestRestoreTargets(t *testing.T) {
	t.Parallel()
	streams, consumers := bus.RestoreTargets()
	if strings.Join(streams, ",") != strings.Join(allStreams, ",") {
		t.Errorf("streams = %v, want %v", streams, allStreams)
	}
	if len(consumers) != len(serverDurables) {
		t.Fatalf("consumers = %v, want the %d server durables", consumers, len(serverDurables))
	}
	for _, c := range consumers {
		if serverDurables[c.Name] != c.Stream {
			t.Errorf("consumer %s on %s, want it on %s", c.Name, c.Stream, serverDurables[c.Name])
		}
	}
}

// A restore whose guard expired while it was frozen, and that a server then
// took, refuses to purge, and its log says the restore lost the account, not
// that two servers share it.
func TestRestoreLock_LostGuardRefusesPurge(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	logger, logs := bufLogger()
	lock, err := bus.NewRemote(remoteConfig(c, bus.DefaultAccount, 3), logger).LockForRestore(t.Context())
	if err != nil {
		t.Fatalf("LockForRestore: %v", err)
	}
	t.Cleanup(lock.Release)
	resume := bus.PauseGuardRefresh(bus.RestoreLockGuard(lock))
	js := dataJS(t, c)
	taken := takeOverExpired(t, js, guardHolder("render-b", 2))
	t.Cleanup(func() { release(t, taken) })
	resume()

	eventually(t, 10*time.Second, "the restore learns it lost the account", func() error {
		if !strings.Contains(logs.String(), "the restore no longer holds the broker account") {
			return errors.New("no log line yet")
		}
		return nil
	})
	if err := lock.Purge(t.Context()); !errors.Is(err, bus.ErrGuardHeld) {
		t.Fatalf("Purge after the guard was lost = %v, want ErrGuardHeld", err)
	}
	if strings.Contains(logs.String(), "two servers") {
		t.Errorf("a lost restore logged as two servers:\n%s", logs)
	}
}
