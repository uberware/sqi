// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// heartbeat delivers one heartbeat from workerID through the worker consumer's
// handler and reports whether it was acked.
func heartbeat(t *testing.T, s *Scheduler, workerID string) bool {
	t.Helper()
	msg := &fakeJSMsg{
		subject: bus.WorkerHeartbeatSubject(workerID),
		data: workerMsgJSON(t, protocol.HeartbeatMsg{
			Version: protocol.ProtocolVersion, WorkerID: workerID, At: time.Now().UTC(),
		}),
	}
	s.handleWorkerMessage(msg)
	return msg.acked
}

// offlineWorker registers id online in capFarm and then takes it offline, as
// the heartbeat sweep does.
func offlineWorker(t *testing.T, st store.Store, id string) {
	t.Helper()
	registerOnline(t, st, store.Worker{ID: id, FarmID: capFarm, CPUCount: 4})
	if _, _, err := st.OfflineWorker(t.Context(), id, "", time.Now().UTC()); err != nil {
		t.Fatalf("OfflineWorker %s: %v", id, err)
	}
}

// reregisterRequests scrapes sqi_scheduler_worker_reregister_requests_total
// for outcome.
func reregisterRequests(t *testing.T, s *Scheduler, outcome string) float64 {
	t.Helper()
	return scrapedValue(t, s, `sqi_scheduler_worker_reregister_requests_total{outcome="`+outcome+`"}`)
}

// A heartbeat recorded for a worker the store holds offline asks the worker,
// once, to register again, and the heartbeat is acked as usual.
func TestHeartbeat_OfflineWorkerAskedToReregister(t *testing.T) {
	f := newActFixture(t, nil)
	offlineWorker(t, f.st, "w1")

	if !heartbeat(t, f.s, "w1") {
		t.Fatal("heartbeat not acked")
	}
	f.s.reregister.calls.Wait()

	calls := f.bus.sent()
	if len(calls) != 1 || calls[0].workerID != "w1" || calls[0].msg.Command != protocol.ControlReregister ||
		calls[0].msg.Version != protocol.ProtocolVersion || calls[0].msg.Type != protocol.TypeControl {
		t.Fatalf("control requests = %+v, want one reregister to w1", calls)
	}
	if got := reregisterRequests(t, f.s, "accepted"); got != 1 {
		t.Errorf("reregister_requests_total{accepted} = %v, want 1", got)
	}
	if !strings.Contains(f.logs.String(), "scheduler: asked an offline worker to register again") ||
		!strings.Contains(f.logs.String(), "worker_id=w1") {
		t.Errorf("log lacks the request:\n%s", f.logs.String())
	}
	if w := f.worker(t, "w1"); w.Status != store.WorkerStatusOffline {
		t.Errorf("w1 status = %s, want still offline: only its registration brings it back", w.Status)
	}
}

// An online worker's heartbeat sends nothing.
func TestHeartbeat_OnlineWorkerNotAsked(t *testing.T) {
	f := newActFixture(t, nil)
	registerOnline(t, f.st, store.Worker{ID: "w1", FarmID: capFarm, CPUCount: 4})

	if !heartbeat(t, f.s, "w1") {
		t.Fatal("heartbeat not acked")
	}
	f.s.reregister.calls.Wait()
	if calls := f.bus.sent(); len(calls) != 0 {
		t.Fatalf("control requests = %+v, want none", calls)
	}
}

// A capacity-group member that is retiring or asleep is offline on purpose:
// it is never asked to register again.
func TestHeartbeat_RetiringOrAsleepNotAsked(t *testing.T) {
	tests := []struct {
		name  string
		state store.PowerState
	}{
		{"retiring", store.PowerRetiring},
		{"asleep", store.PowerAsleep},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newActFixture(t, nil)
			g := createGroup(t, f.st, wolGroup("lan", nil))
			addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
			now := time.Now().UTC()
			if _, ok, err := f.st.BeginRetirement(t.Context(), g.ID, "m1", now); err != nil || !ok {
				t.Fatalf("BeginRetirement = %v, %v", ok, err)
			}
			if tt.state == store.PowerAsleep {
				if _, _, ok, err := f.st.FinishRetirement(t.Context(), "m1", now); err != nil || !ok {
					t.Fatalf("FinishRetirement = %v, %v", ok, err)
				}
			} else if _, _, err := f.st.OfflineWorker(t.Context(), "m1", "", now); err != nil {
				t.Fatalf("OfflineWorker: %v", err)
			}
			if w := f.worker(t, "m1"); w.Status != store.WorkerStatusOffline || w.PowerState != tt.state {
				t.Fatalf("m1 = %s/%s, want offline/%s", w.Status, w.PowerState, tt.state)
			}

			if !heartbeat(t, f.s, "m1") {
				t.Fatal("heartbeat not acked")
			}
			f.s.reregister.calls.Wait()
			if calls := f.bus.sent(); len(calls) != 0 {
				t.Fatalf("control requests = %+v, want none", calls)
			}
		})
	}
}

// Two heartbeats inside WorkerTimeout send one request.
func TestHeartbeat_ReregisterRateLimited(t *testing.T) {
	f := newActFixture(t, nil)
	offlineWorker(t, f.st, "w1")

	for range 2 {
		if !heartbeat(t, f.s, "w1") {
			t.Fatal("heartbeat not acked")
		}
		f.s.reregister.calls.Wait()
	}
	if n := len(f.bus.sentCommand(protocol.ControlReregister)); n != 1 {
		t.Fatalf("reregister requests = %d, want 1", n)
	}
}

// A refusal and a request that fails are counted, and neither stops a later
// heartbeat, past WorkerTimeout, from asking again.
func TestHeartbeat_ReregisterOutcomesCounted(t *testing.T) {
	f := newActFixture(t, nil)
	offlineWorker(t, f.st, "w1")
	f.s.cfg.WorkerTimeout = time.Millisecond

	f.bus.setAnswer(func(string, protocol.ControlMsg) ([]byte, error) {
		return controlReply(false, protocol.ControlRefusalAlreadyRetiring), nil
	})
	heartbeat(t, f.s, "w1")
	f.s.reregister.calls.Wait()

	time.Sleep(5 * time.Millisecond)
	f.bus.setAnswer(func(string, protocol.ControlMsg) ([]byte, error) {
		return []byte("not json"), nil
	})
	heartbeat(t, f.s, "w1")
	f.s.reregister.calls.Wait()

	if n := len(f.bus.sentCommand(protocol.ControlReregister)); n != 2 {
		t.Fatalf("reregister requests = %d, want 2", n)
	}
	if got := reregisterRequests(t, f.s, "refused"); got != 1 {
		t.Errorf("reregister_requests_total{refused} = %v, want 1", got)
	}
	if got := reregisterRequests(t, f.s, "error"); got != 1 {
		t.Errorf("reregister_requests_total{error} = %v, want 1", got)
	}
}

// The request runs off the consumer goroutine: a worker that does not answer
// does not hold up the heartbeat's ack, and at most reregisterMaxInFlight
// requests wait at once, the rest skipped until a later heartbeat.
func TestHeartbeat_ReregisterDoesNotBlockConsumer(t *testing.T) {
	f := newActFixture(t, nil)
	release := make(chan struct{})
	f.bus.setAnswer(func(string, protocol.ControlMsg) ([]byte, error) {
		<-release
		return controlReply(true, ""), nil
	})
	ids := make([]string, reregisterMaxInFlight+1)
	for i := range ids {
		ids[i] = "w" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		offlineWorker(t, f.st, ids[i])
	}

	for _, id := range ids {
		done := make(chan bool, 1)
		go func() { done <- heartbeat(t, f.s, id) }()
		select {
		case acked := <-done:
			if !acked {
				t.Fatalf("heartbeat from %s not acked", id)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("heartbeat from %s blocked on the reregister request", id)
		}
	}
	waitFor(t, func() bool { return len(f.bus.sent()) == reregisterMaxInFlight })
	close(release)
	f.s.reregister.calls.Wait()

	if n := len(f.bus.sentCommand(protocol.ControlReregister)); n != reregisterMaxInFlight {
		t.Fatalf("reregister requests = %d, want %d: the one past the bound is skipped", n, reregisterMaxInFlight)
	}
	// The skipped worker was not rate-limited by the ask it never got.
	last := ids[len(ids)-1]
	heartbeat(t, f.s, last)
	f.s.reregister.calls.Wait()
	calls := f.bus.sentCommand(protocol.ControlReregister)
	if n := len(calls); n != reregisterMaxInFlight+1 || calls[n-1].workerID != last {
		t.Fatalf("reregister requests = %d, want %d ending with %s", n, reregisterMaxInFlight+1, last)
	}
}

// The rate-limit entries are dropped once a worker is seen online, and pruned
// once old, so the map does not grow with every worker ever asked.
func TestReregister_EntriesForgottenAndPruned(t *testing.T) {
	f := newActFixture(t, nil)
	offlineWorker(t, f.st, "w1")
	offlineWorker(t, f.st, "w2")
	heartbeat(t, f.s, "w1")
	heartbeat(t, f.s, "w2")
	f.s.reregister.calls.Wait()
	if n := f.s.reregister.size(); n != 2 {
		t.Fatalf("entries = %d, want 2", n)
	}

	registerOnline(t, f.st, store.Worker{ID: "w1", FarmID: capFarm, CPUCount: 4})
	heartbeat(t, f.s, "w1")
	f.s.reregister.calls.Wait()
	if n := f.s.reregister.size(); n != 1 {
		t.Fatalf("entries after w1 came back online = %d, want 1", n)
	}

	f.s.pruneReregisterAsks(t.Context())
	if n := f.s.reregister.size(); n != 1 {
		t.Fatalf("entries after the sweep's prune = %d, want 1: w2 was asked just now", n)
	}
	f.s.reregister.prune(time.Now().Add(time.Second))
	if n := f.s.reregister.size(); n != 0 {
		t.Fatalf("entries after pruning = %d, want 0", n)
	}
}

// waitFor polls cond until it holds, failing the test after five seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(time.Millisecond)
	}
}
