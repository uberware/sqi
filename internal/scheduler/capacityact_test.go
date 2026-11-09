// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/capacity"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── fixtures ────────────────────────────────────────────────────────────────

// controlCall is one control request the scheduler sent.
type controlCall struct {
	workerID string
	msg      protocol.ControlMsg
}

// controlBus is a busClient that records every control request and answers
// it with answer, or accepts it when answer is nil.
type controlBus struct {
	recordBus

	mu     sync.Mutex
	calls  []controlCall
	answer func(workerID string, msg protocol.ControlMsg) ([]byte, error)
}

func (b *controlBus) RequestWorkerControl(_ context.Context, workerID string, data []byte) ([]byte, error) {
	var msg protocol.ControlMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.calls = append(b.calls, controlCall{workerID: workerID, msg: msg})
	answer := b.answer
	b.mu.Unlock()
	if answer == nil {
		return controlReply(true, ""), nil
	}
	return answer(workerID, msg)
}

func (b *controlBus) setAnswer(f func(workerID string, msg protocol.ControlMsg) ([]byte, error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.answer = f
}

func (b *controlBus) sent() []controlCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls)
}

// sentCommand returns the requests of command cmd, in order.
func (b *controlBus) sentCommand(cmd protocol.ControlCommand) []controlCall {
	var out []controlCall
	for _, c := range b.sent() {
		if c.msg.Command == cmd {
			out = append(out, c)
		}
	}
	return out
}

func controlReply(accepted bool, reason string) []byte {
	b, err := json.Marshal(protocol.ControlReplyMsg{Accepted: accepted, Reason: reason})
	if err != nil {
		panic(err)
	}
	return b
}

// packetSend is one direct magic-packet send.
type packetSend struct {
	macs, targets []string
}

// packetRecorder stands in for Config.SendWakePackets. It reports every
// target reached unless fail is set, which returns the targets reached and
// the error instead.
type packetRecorder struct {
	mu    sync.Mutex
	sends []packetSend
	fail  func(targets []string) (int, error)
}

func (r *packetRecorder) send(_ context.Context, macs []net.HardwareAddr, targets []string) (int, error) {
	ps := packetSend{targets: slices.Clone(targets)}
	for _, m := range macs {
		ps.macs = append(ps.macs, m.String())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, ps)
	if r.fail != nil {
		return r.fail(targets)
	}
	return len(targets), nil
}

func (r *packetRecorder) all() []packetSend {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.sends)
}

// actFixture is a scheduler over a fake store holding one farm and queue,
// with a control bus spy and a packet recorder.
type actFixture struct {
	st      *fake.Store
	s       *Scheduler
	bus     *controlBus
	packets *packetRecorder
	logs    *lockedBuffer
}

// newActFixture builds an actFixture, its scheduler over wrap(st) when wrap
// is given.
func newActFixture(t *testing.T, wrap func(*fake.Store) store.Store) *actFixture {
	t.Helper()
	st := newCheckedFake(t)
	if _, err := st.CreateFarm(t.Context(), store.Farm{ID: capFarm, Name: "farm"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(t.Context(), store.Queue{ID: capQueue, FarmID: capFarm, Name: "queue"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	var used store.Store = st
	if wrap != nil {
		used = wrap(st)
	}
	f := &actFixture{st: st, bus: &controlBus{}, packets: &packetRecorder{}, logs: &lockedBuffer{}}
	f.s = newMetricsScheduler(used, f.bus, "")
	f.s.logger = slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.s.cfg.SendWakePackets = f.packets.send
	f.s.cfg.WakeOnLANSendTo = []string{"10.0.0.255:9"}
	return f
}

// sweep runs one capacity sweep at now under ctx and waits for the provider
// calls it started.
func (f *actFixture) sweepCtx(ctx context.Context, now time.Time) {
	f.s.sweepCapacityAt(ctx, now)
	f.s.capState.act.calls.Wait()
}

func (f *actFixture) sweep(t *testing.T, now time.Time) {
	t.Helper()
	f.sweepCtx(t.Context(), now)
}

func (f *actFixture) worker(t *testing.T, id string) store.Worker {
	t.Helper()
	w, err := f.st.GetWorker(t.Context(), id)
	if err != nil {
		t.Fatalf("GetWorker %s: %v", id, err)
	}
	return w
}

// wolGroup is a valid, enabled, named wake-on-LAN group retiring with sleep,
// changed by mut.
func wolGroup(name string, mut func(*store.CapacityGroup)) store.CapacityGroup {
	return sizedGroup(name, func(g *store.CapacityGroup) {
		g.Kind, g.Provider, g.Profile = store.CapacityGroupNamed, store.CapacityProviderWakeOnLAN, store.MachineProfile{}
		g.RetireAction = store.CapacityRetireSleep
		if mut != nil {
			mut(g)
		}
	})
}

// lanWorker is a 4-CPU linux worker of capFarm with one interface, mac, at
// ip on a /24.
func lanWorker(id, ip, mac string) store.Worker {
	return store.Worker{
		ID: id, FarmID: capFarm, CPUCount: 4, OS: "linux", Arch: "amd64", IPAddress: ip,
		Interfaces: []store.NetInterface{{Name: "eth0", MAC: mac, Addrs: []string{ip + "/24"}}},
	}
}

// addNamedMember registers w, makes it a (not ephemeral) member of groupID
// and, unless online, takes it offline.
func addNamedMember(t *testing.T, st store.Store, groupID string, w store.Worker, online bool) store.Worker {
	t.Helper()
	if w.FarmID == "" {
		w.FarmID = capFarm
	}
	w.Status = store.WorkerStatusOnline
	if _, _, err := st.RegisterWorker(t.Context(), w); err != nil {
		t.Fatalf("RegisterWorker %s: %v", w.ID, err)
	}
	out, err := st.SetWorkerCapacityGroup(t.Context(), w.ID, groupID, false)
	if err != nil {
		t.Fatalf("SetWorkerCapacityGroup %s: %v", w.ID, err)
	}
	if online {
		return out
	}
	if _, _, err := st.OfflineWorker(t.Context(), w.ID, "", time.Now().UTC()); err != nil {
		t.Fatalf("OfflineWorker %s: %v", w.ID, err)
	}
	got, err := st.GetWorker(t.Context(), w.ID)
	if err != nil {
		t.Fatalf("GetWorker %s: %v", w.ID, err)
	}
	return got
}

// registerOnline registers w online, in no group.
func registerOnline(t *testing.T, st store.Store, w store.Worker) {
	t.Helper()
	w.Status = store.WorkerStatusOnline
	if _, _, err := st.RegisterWorker(t.Context(), w); err != nil {
		t.Fatalf("RegisterWorker %s: %v", w.ID, err)
	}
}

// metricValue reads one series' value, gauge or counter.
func metricValue(t *testing.T, m prometheus.Metric) float64 {
	t.Helper()
	var d dto.Metric
	if err := m.Write(&d); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	if d.Counter != nil {
		return d.GetCounter().GetValue()
	}
	return d.GetGauge().GetValue()
}

func actionsTotal(t *testing.T, f *actFixture, group, action, result string) float64 {
	t.Helper()
	return metricValue(t, f.s.metrics.CapacityActionsTotal.WithLabelValues(group, action, result))
}

func bootTimeouts(t *testing.T, f *actFixture, group string) float64 {
	t.Helper()
	return metricValue(t, f.s.metrics.CapacityBootTimeoutsTotal.WithLabelValues(group))
}

func lastAction(t *testing.T, s *Scheduler, groupID string) capacity.ActionRecord {
	t.Helper()
	st, ok := s.CapacityStatus(groupID)
	if !ok {
		t.Fatalf("CapacityStatus(%s): no status", groupID)
	}
	if st.LastAction == nil {
		t.Fatalf("CapacityStatus(%s).LastAction = nil", groupID)
	}
	return *st.LastAction
}

// deregisterAs handles a deregistration of workerID of kind.
func deregisterAs(t *testing.T, s *Scheduler, workerID string, kind protocol.DeregisterKind) {
	t.Helper()
	msg := &fakeJSMsg{
		subject: bus.WorkerDeregisterSubject(workerID),
		data: workerMsgJSON(t, map[string]string{
			"worker_id": workerID, "reason": "capacity group", "kind": string(kind),
		}),
	}
	s.handleWorkerMessage(msg)
	if !msg.acked {
		t.Fatalf("deregister of %s not acked", workerID)
	}
}

// ── the actuator ──────────────────────────────────────────────────────────────

// A retire candidate of a named wake-on-LAN group is moved to retiring and
// sent the group's retire action; its deregistration then puts it to sleep
// and offline.
func TestActuator_RetireSendsRetireActionAndFinishesOnDeregister(t *testing.T) {
	f := newActFixture(t, nil)
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	t0 := time.Now().UTC()

	f.sweep(t, t0)
	if n := len(f.bus.sent()); n != 0 {
		t.Fatalf("control requests before the idle timeout = %d, want 0", n)
	}
	f.sweep(t, t0.Add(2*time.Minute))

	calls := f.bus.sent()
	if len(calls) != 1 || calls[0].workerID != "m1" || calls[0].msg.Command != protocol.ControlSleep ||
		calls[0].msg.Version != protocol.ProtocolVersion || calls[0].msg.Type != protocol.TypeControl {
		t.Fatalf("control requests = %+v, want one sleep to m1", calls)
	}
	if w := f.worker(t, "m1"); w.PowerState != store.PowerRetiring || w.Status != store.WorkerStatusOnline {
		t.Fatalf("m1 = %s/%s, want retiring and online until it deregisters", w.PowerState, w.Status)
	}
	if got := actionsTotal(t, f, "lan", "retire", "ok"); got != 1 {
		t.Errorf("actions_total{retire,ok} = %v, want 1", got)
	}
	if la := lastAction(t, f.s, g.ID); la.Action != "retire" || la.Result != "ok" {
		t.Errorf("LastAction = %+v, want retire ok", la)
	}

	deregisterAs(t, f.s, "m1", protocol.DeregisterRetired)
	w := f.worker(t, "m1")
	if w.PowerState != store.PowerAsleep || w.Status != store.WorkerStatusOffline {
		t.Fatalf("m1 after deregistering = %s/%s, want asleep and offline", w.PowerState, w.Status)
	}
	if !strings.Contains(f.logs.String(), "capacity group member retired: sleep") {
		t.Errorf("log lacks the retirement:\n%s", f.logs.String())
	}

	f.sweep(t, t0.Add(3*time.Minute))
	if n := len(f.bus.sent()); n != 1 {
		t.Errorf("control requests after it slept = %d, want still 1", n)
	}
}

// A refusal puts the member back to awake at once, with the worker's reason,
// and it is not asked again until it has been idle for another idle timeout.
func TestActuator_RetireRefusedAbortsToAwake(t *testing.T) {
	f := newActFixture(t, nil)
	f.bus.setAnswer(func(string, protocol.ControlMsg) ([]byte, error) {
		return controlReply(false, protocol.ControlRefusalNotAllowed), nil
	})
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	t0 := time.Now().UTC()

	f.sweep(t, t0)
	f.sweep(t, t0.Add(2*time.Minute))

	w := f.worker(t, "m1")
	if w.PowerState != store.PowerAwake {
		t.Fatalf("m1 = %s, want awake after the refusal", w.PowerState)
	}
	if want := "worker refused: worker.power.allow_remote_sleep is false"; w.PowerReason != want {
		t.Errorf("PowerReason = %q, want %q", w.PowerReason, want)
	}
	if got := actionsTotal(t, f, "lan", "retire", "refused"); got != 1 {
		t.Errorf("actions_total{retire,refused} = %v, want 1", got)
	}
	if la := lastAction(t, f.s, g.ID); la.Action != "retire" || la.Result != "refused" ||
		!strings.Contains(la.Message, "allow_remote_sleep") {
		t.Errorf("LastAction = %+v, want retire refused naming the reason", la)
	}

	// The next ticks inside one idle timeout ask nothing more.
	f.sweep(t, t0.Add(2*time.Minute+15*time.Second))
	f.sweep(t, t0.Add(2*time.Minute+45*time.Second))
	if n := len(f.bus.sent()); n != 1 {
		t.Errorf("sleep requests within an idle timeout of the refusal = %d, want 1", n)
	}
}

// No reply within the control timeout aborts the retirement.
func TestActuator_RetireNoReplyAborts(t *testing.T) {
	f := newActFixture(t, nil)
	f.bus.setAnswer(func(string, protocol.ControlMsg) ([]byte, error) {
		return nil, context.DeadlineExceeded
	})
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	t0 := time.Now().UTC()

	f.sweep(t, t0)
	f.sweep(t, t0.Add(2*time.Minute))

	w := f.worker(t, "m1")
	if w.PowerState != store.PowerAwake {
		t.Fatalf("m1 = %s, want awake", w.PowerState)
	}
	if want := "no reply to the sleep request within 10s"; w.PowerReason != want {
		t.Errorf("PowerReason = %q, want %q", w.PowerReason, want)
	}
	if got := actionsTotal(t, f, "lan", "retire", "timeout"); got != 1 {
		t.Errorf("actions_total{retire,timeout} = %v, want 1", got)
	}
}

// A retirement this process began whose sleep reaches no one (the worker is
// online but not listening on its control subject) aborts at once: no command
// was delivered, and waiting would hold the member out of work for a boot
// timeout on every idle timeout.
func TestActuator_RetireNoRespondersOnFirstSendAborts(t *testing.T) {
	f := newActFixture(t, nil)
	f.bus.setAnswer(func(string, protocol.ControlMsg) ([]byte, error) { return nil, nats.ErrNoResponders })
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	t0 := time.Now().UTC()

	f.sweep(t, t0)
	f.sweep(t, t0.Add(2*time.Minute))

	if n := len(f.bus.sentCommand(protocol.ControlSleep)); n != 1 {
		t.Fatalf("sleep requests = %d, want 1", n)
	}
	w := f.worker(t, "m1")
	if w.PowerState != store.PowerAwake {
		t.Fatalf("m1 = %s, want awake", w.PowerState)
	}
	if want := "worker is not listening on its control subject"; w.PowerReason != want {
		t.Errorf("PowerReason = %q, want %q", w.PowerReason, want)
	}
	if got := actionsTotal(t, f, "lan", "retire", "refused"); got != 1 {
		t.Errorf("actions_total{retire,refused} = %v, want 1", got)
	}
}

// A retirement recorded before this process started (re-sent after a restart
// or a failover) keeps waiting on no responders: the worker may have carried
// out the first request and left already, and its deregistration, or the
// boot timeout, settles it.
func TestActuator_RetireNoRespondersOnReissueStaysRetiring(t *testing.T) {
	f := newActFixture(t, nil)
	f.bus.setAnswer(func(string, protocol.ControlMsg) ([]byte, error) { return nil, nats.ErrNoResponders })
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	t0 := time.Now().UTC()
	if _, ok, err := f.st.BeginRetirement(t.Context(), g.ID, "m1", t0); err != nil || !ok {
		t.Fatalf("BeginRetirement = %v, %v", ok, err)
	}

	f.sweep(t, t0.Add(15*time.Second))

	if n := len(f.bus.sentCommand(protocol.ControlSleep)); n != 1 {
		t.Fatalf("sleep requests = %d, want the recorded one re-sent once", n)
	}
	if got := f.worker(t, "m1").PowerState; got != store.PowerRetiring {
		t.Errorf("m1 = %s, want still retiring", got)
	}
	if got := actionsTotal(t, f, "lan", "retire", "failed"); got != 1 {
		t.Errorf("actions_total{retire,failed} = %v, want 1", got)
	}
}

// parkLease parks a lease request of workerID through the disabled-worker
// hold, as a retiring worker's request is, and returns the task IDs it is
// eventually answered with.
func parkLease(t *testing.T, s *Scheduler, workerID string) <-chan []string {
	t.Helper()
	done := make(chan []string, 1)
	go func() {
		var ids []string
		for _, raw := range s.holdDisabledLease(s.ctx, workerID, "") {
			var m protocol.AssignMsg
			if err := json.Unmarshal(raw, &m); err == nil {
				ids = append(ids, m.TaskID)
			}
		}
		done <- ids
	}()
	waitParkedDisabled(t, s, workerID)
	return done
}

// A retirement that ends with the member awake again, aborted on a refusal or
// expired after the boot timeout, releases the lease request the member parked
// while it was retiring at once, instead of leaving it to wait out the hold.
func TestActuator_SettledRetirementReleasesParkedLease(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, f *actFixture, groupID string, t0 time.Time) <-chan []string
	}{
		{"aborted", func(t *testing.T, f *actFixture, _ string, t0 time.Time) <-chan []string {
			asked, release := make(chan struct{}, 1), make(chan struct{})
			f.bus.setAnswer(func(string, protocol.ControlMsg) ([]byte, error) {
				asked <- struct{}{}
				<-release
				return controlReply(false, protocol.ControlRefusalBusy), nil
			})
			f.sweep(t, t0)
			f.s.sweepCapacityAt(t.Context(), t0.Add(2*time.Minute))
			select {
			case <-asked:
			case <-time.After(5 * time.Second):
				t.Fatal("no sleep was requested")
			}
			done := parkLease(t, f.s, "m1")
			close(release)
			f.s.capState.act.calls.Wait()
			return done
		}},
		{"expired", func(t *testing.T, f *actFixture, groupID string, t0 time.Time) <-chan []string {
			if _, ok, err := f.st.BeginRetirement(t.Context(), groupID, "m1", t0); err != nil || !ok {
				t.Fatalf("BeginRetirement = %v, %v", ok, err)
			}
			// The recorded intent is this process's own, so nothing is re-sent.
			f.s.capState.act.dispatched = map[intentKey]struct{}{memberIntent(f.worker(t, "m1")): {}}
			done := parkLease(t, f.s, "m1")
			f.sweep(t, t0.Add(5*time.Minute+time.Second))
			return done
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newActFixture(t, func(st *fake.Store) store.Store { return demandBlind{st} })
			f.s.ctx = t.Context()
			f.s.leaseHoldTimeout = 30 * time.Second // only a wake answers within the test
			g := createGroup(t, f.st, wolGroup("lan", nil))
			addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
			tasks := submitReady(t, f.st, 1, store.Step{})
			t0 := time.Now().UTC()

			done := tc.run(t, f, g.ID, t0)

			if got := f.worker(t, "m1").PowerState; got != store.PowerAwake {
				t.Fatalf("m1 = %s, want awake", got)
			}
			select {
			case ids := <-done:
				if len(ids) != 1 || ids[0] != tasks[0].ID {
					t.Errorf("released lease = %v, want [%s]", ids, tasks[0].ID)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the parked lease was not released when the retirement settled")
			}
		})
	}
}

// pickLeaseStore leases a task to the worker BeginRetirement is called for
// first, as a lease landing between the policy's pick and the retirement does.
type pickLeaseStore struct {
	store.Store

	t      *testing.T
	taskID string
	once   sync.Once
}

func (s *pickLeaseStore) BeginRetirement(ctx context.Context, groupID, workerID string, now time.Time) (store.Worker, bool, error) {
	s.once.Do(func() { storetest.Lease(s.t, s.Store, store.LeaseRequest{TaskID: s.taskID, WorkerID: workerID}) })
	return s.Store.BeginRetirement(ctx, groupID, workerID, now)
}

// A lease landing after the policy picked a member but before its retirement
// makes the retirement compare-and-set refuse: the member stays awake with
// its task and is sent nothing.
func TestActuator_LeaseBetweenPickAndRetireKeepsMemberAwake(t *testing.T) {
	race := &pickLeaseStore{t: t}
	f := newActFixture(t, func(st *fake.Store) store.Store {
		race.Store = st
		// The evaluation must not see the task as demand: it stands for work
		// that arrived after the pick.
		return demandBlind{race}
	})
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	tasks := submitReady(t, f.st, 1, store.Step{})
	race.taskID = tasks[0].ID
	t0 := time.Now().UTC()

	f.sweep(t, t0)
	f.sweep(t, t0.Add(2*time.Minute))

	if n := len(f.bus.sent()); n != 0 {
		t.Fatalf("control requests = %+v, want none", f.bus.sent())
	}
	w := f.worker(t, "m1")
	if w.PowerState != store.PowerAwake || w.Status != store.WorkerStatusOnline {
		t.Fatalf("m1 = %s/%s, want awake and online", w.PowerState, w.Status)
	}
	if task := mustTaskOf(t, f.st, tasks[0].ID); task.Status != store.TaskStatusAssigned || task.AssignedWorkerID != "m1" {
		t.Fatalf("task = %s on %q, want assigned to m1", task.Status, task.AssignedWorkerID)
	}
}

// demandBlind reports no ready work, so a test can place a task the
// evaluation does not see.
type demandBlind struct{ store.Store }

func (demandBlind) ListReadyStepDemand(context.Context, string, time.Time) ([]store.StepDemand, error) {
	return nil, nil
}

// The disable flag is the maintenance hold: a disabled member is never woken,
// retired or sent anything, even one left waking from before it was disabled.
func TestActuator_DisabledMemberNeverTouched(t *testing.T) {
	f := newActFixture(t, nil)
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("idle", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	addNamedMember(t, f.st, g.ID, lanWorker("off", "192.168.1.22", "aa:bb:cc:dd:ee:02"), false)
	addNamedMember(t, f.st, g.ID, lanWorker("waking", "192.168.1.23", "aa:bb:cc:dd:ee:03"), false)
	t0 := time.Now().UTC()
	if moved, err := f.st.MarkWaking(t.Context(), g.ID, []string{"waking"}, t0); err != nil || len(moved) != 1 {
		t.Fatalf("MarkWaking = %v, %v", moved, err)
	}
	for _, id := range []string{"idle", "off", "waking"} {
		if _, err := f.st.SetWorkerDisabled(t.Context(), id, true); err != nil {
			t.Fatalf("SetWorkerDisabled %s: %v", id, err)
		}
	}
	submitReady(t, f.st, 3, store.Step{})

	for i := range 4 {
		f.sweep(t, t0.Add(time.Duration(i)*time.Minute))
	}

	if calls := f.bus.sent(); len(calls) != 0 {
		t.Errorf("control requests = %+v, want none", calls)
	}
	if sends := f.packets.all(); len(sends) != 0 {
		t.Errorf("packets = %+v, want none", sends)
	}
	for id, want := range map[string]store.PowerState{"idle": store.PowerAwake, "off": store.PowerAwake, "waking": store.PowerWaking} {
		if got := f.worker(t, id).PowerState; got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
}

// Members left waking or retiring expire whatever the group's provider and
// whether it is enabled, so a switch to external, or a disable, mid-transition
// never leaves a member stuck.
func TestActuator_ExpiryRunsForAnyProvider(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*store.CapacityGroup)
	}{
		{"switched to external", func(g *store.CapacityGroup) { g.Provider = store.CapacityProviderExternal }},
		{"disabled", func(g *store.CapacityGroup) { g.Enabled = false }},
		{"unconfigured script", func(g *store.CapacityGroup) { g.Provider = "script:missing" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newActFixture(t, nil)
			g := createGroup(t, f.st, wolGroup("lan", nil))
			addNamedMember(t, f.st, g.ID, lanWorker("waking", "192.168.1.21", "aa:bb:cc:dd:ee:01"), false)
			addNamedMember(t, f.st, g.ID, lanWorker("retiring", "192.168.1.22", "aa:bb:cc:dd:ee:02"), true)
			t0 := time.Now().UTC()
			if moved, err := f.st.MarkWaking(t.Context(), g.ID, []string{"waking"}, t0); err != nil || len(moved) != 1 {
				t.Fatalf("MarkWaking = %v, %v", moved, err)
			}
			if _, ok, err := f.st.BeginRetirement(t.Context(), g.ID, "retiring", t0); err != nil || !ok {
				t.Fatalf("BeginRetirement = %v, %v", ok, err)
			}
			tc.mut(&g)
			if _, err := f.st.UpdateCapacityGroup(t.Context(), g); err != nil {
				t.Fatalf("UpdateCapacityGroup: %v", err)
			}

			f.sweep(t, t0.Add(g.BootTimeout-time.Second))
			if got := f.worker(t, "waking").PowerState; got != store.PowerWaking {
				t.Fatalf("waking before its boot timeout = %s, want waking", got)
			}
			f.sweep(t, t0.Add(g.BootTimeout+time.Second))

			w := f.worker(t, "waking")
			if w.PowerState != store.PowerWakeFailed ||
				w.PowerReason != "did not register within boot_timeout (5m0s) after a wake request" {
				t.Errorf("waking = %s %q, want wake_failed with the boot-timeout reason", w.PowerState, w.PowerReason)
			}
			r := f.worker(t, "retiring")
			if r.PowerState != store.PowerAwake || r.PowerReason != "retirement did not complete within boot_timeout (5m0s)" {
				t.Errorf("retiring = %s %q, want awake with the retirement reason", r.PowerState, r.PowerReason)
			}
			if got := bootTimeouts(t, f, "lan"); got != 1 {
				t.Errorf("boot_timeouts_total = %v, want 1", got)
			}
			if n := strings.Count(f.logs.String(), "worker_id=waking"); n == 0 {
				t.Errorf("the failed wake is not logged:\n%s", f.logs.String())
			}
			if calls := f.bus.sent(); len(calls) != 0 {
				t.Errorf("control requests = %+v, want none", calls)
			}
			if sends := f.packets.all(); len(sends) != 0 {
				t.Errorf("packets = %+v, want none", sends)
			}
		})
	}
}

// A member found waking with no dispatch made by this process (the previous
// leader recorded it) is sent its wake once, and only once.
func TestActuator_ReissuesRecordedIntentOncePerProcess(t *testing.T) {
	f := newActFixture(t, nil)
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), false)
	t0 := time.Now().UTC()
	if moved, err := f.st.MarkWaking(t.Context(), g.ID, []string{"m1"}, t0); err != nil || len(moved) != 1 {
		t.Fatalf("MarkWaking = %v, %v", moved, err)
	}

	for i := 1; i <= 4; i++ {
		f.sweep(t, t0.Add(time.Duration(i)*15*time.Second))
	}

	sends := f.packets.all()
	if len(sends) != 1 || !slices.Equal(sends[0].macs, []string{"aa:bb:cc:dd:ee:01"}) {
		t.Errorf("packet sends = %+v, want one for m1", sends)
	}
	if got := f.worker(t, "m1").PowerState; got != store.PowerWaking {
		t.Errorf("m1 = %s, want still waking", got)
	}
}

// After a failover the new leader repeats the absolute request the old one
// recorded, once, and sends nothing that conflicts with it.
func TestActuator_FailoverRepeatsAbsoluteRequestOnly(t *testing.T) {
	st := newCheckedFake(t)
	if _, err := st.CreateFarm(t.Context(), store.Farm{ID: capFarm, Name: "farm"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	g := createGroup(t, st, wolGroup("lan", nil))
	addNamedMember(t, st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	addNamedMember(t, st, g.ID, lanWorker("m2", "192.168.1.22", "aa:bb:cc:dd:ee:02"), false)
	ctxA, depose := leaderCtx(t, st)

	busA, busB := &controlBus{}, &controlBus{}
	a := &actFixture{st: st, s: newMetricsScheduler(st, busA, ""), bus: busA, packets: &packetRecorder{}}
	b := &actFixture{st: st, s: newMetricsScheduler(st, busB, ""), bus: busB, packets: &packetRecorder{}}
	a.s.cfg.SendWakePackets, b.s.cfg.SendWakePackets = a.packets.send, b.packets.send
	t0 := time.Now().UTC()

	a.sweepCtx(ctxA, t0)
	a.sweepCtx(ctxA, t0.Add(2*time.Minute))
	if w := a.worker(t, "m1"); w.PowerState != store.PowerRetiring {
		t.Fatalf("m1 = %s, want retiring under the first leader", w.PowerState)
	}
	if n := len(busA.sentCommand(protocol.ControlSleep)); n != 1 {
		t.Fatalf("leader A sent %d sleeps, want 1", n)
	}

	depose()
	ctxB := store.WithLeaderEpoch(t.Context(), 2)
	for i := range 3 {
		b.sweepCtx(ctxB, t0.Add(2*time.Minute+time.Duration(i+1)*15*time.Second))
	}

	calls := busB.sent()
	if len(calls) != 1 || calls[0].workerID != "m1" || calls[0].msg.Command != protocol.ControlSleep {
		t.Errorf("leader B sent %+v, want exactly one sleep to m1", calls)
	}
	if sends := b.packets.all(); len(sends) != 0 {
		t.Errorf("leader B sent packets %+v, want none", sends)
	}
	if w := b.worker(t, "m1"); w.PowerState != store.PowerRetiring {
		t.Errorf("m1 = %s, want still retiring until it deregisters", w.PowerState)
	}

	// The deposed leader, still sweeping, acts on nothing: past the boot
	// timeout it would otherwise expire m1's retirement and retire it again,
	// and with work waiting it would wake m2.
	submitReady(t, st, 1, store.Step{})
	for i := range 2 {
		a.sweepCtx(ctxA, t0.Add(2*time.Minute+g.BootTimeout+time.Duration(i+1)*15*time.Second))
	}
	if calls := busA.sent(); len(calls) != 1 {
		t.Errorf("deposed leader A sent %+v, want only its first sleep", calls)
	}
	if sends := a.packets.all(); len(sends) != 0 {
		t.Errorf("deposed leader A sent packets %+v, want none", sends)
	}
	if w := a.worker(t, "m1"); w.PowerState != store.PowerRetiring {
		t.Errorf("m1 = %s, want still retiring: the deposed leader must not expire it", w.PowerState)
	}
}

// retireConflictStore refuses every FinishRetirement as the store refuses one
// for a retiring member that still holds a task.
type retireConflictStore struct{ store.Store }

func (retireConflictStore) FinishRetirement(_ context.Context, workerID string, _ time.Time) (store.Worker, bool, bool, error) {
	return store.Worker{}, false, false, fmt.Errorf("store: finish retirement of worker %s, which holds a task: %w",
		workerID, store.ErrConflict)
}

// A deregistration of a retiring member whose retirement the store refuses to
// finish (it still holds a task) is acknowledged, leaves the member retiring
// for the boot timeout to settle, and is logged.
func TestDeregister_RetiringMemberHoldingATaskStaysRetiring(t *testing.T) {
	f := newActFixture(t, func(st *fake.Store) store.Store { return retireConflictStore{st} })
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	if _, ok, err := f.st.BeginRetirement(t.Context(), g.ID, "m1", time.Now().UTC()); err != nil || !ok {
		t.Fatalf("BeginRetirement = %v, %v", ok, err)
	}

	deregisterAs(t, f.s, "m1", protocol.DeregisterRetired)

	if w := f.worker(t, "m1"); w.PowerState != store.PowerRetiring {
		t.Errorf("m1 = %s, want still retiring", w.PowerState)
	}
	if out := f.logs.String(); !strings.Contains(out, "finish capacity retirement on deregister failed") {
		t.Errorf("the refused finish is not logged:\n%s", out)
	}
}

// A deposed leader applies nothing: a refusal arriving after it lost the
// lease is not written, so the member stays as the new leader sees it.
func TestActuator_DeposedLeaderLateResultWritesNothing(t *testing.T) {
	f := newActFixture(t, nil)
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	ctx, depose := leaderCtx(t, f.st)
	release := make(chan struct{})
	asked := make(chan struct{}, 1)
	f.bus.setAnswer(func(string, protocol.ControlMsg) ([]byte, error) {
		asked <- struct{}{}
		<-release
		return controlReply(false, protocol.ControlRefusalBusy), nil
	})
	t0 := time.Now().UTC()

	f.s.sweepCapacityAt(ctx, t0)
	f.s.sweepCapacityAt(ctx, t0.Add(2*time.Minute))
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("no sleep was requested")
	}
	depose()
	close(release)
	f.s.capState.act.calls.Wait()

	if got := f.worker(t, "m1").PowerState; got != store.PowerRetiring {
		t.Errorf("m1 = %s, want retiring: a deposed leader's late result must not write", got)
	}
}

// With a lease parked through the disabled-worker hold, a registration that
// turns a waking, asleep or wake-failed member awake logs the wake and
// releases the parked request, which is then leased the ready task.
func TestRegister_WakingMemberBecomesAwakeAndReleasesLease(t *testing.T) {
	tests := []struct {
		name  string
		prior func(t *testing.T, f *actFixture, groupID string, at time.Time)
	}{
		{"waking", func(t *testing.T, f *actFixture, groupID string, at time.Time) {
			if _, err := f.st.MarkWaking(t.Context(), groupID, []string{"m1"}, at); err != nil {
				t.Fatalf("MarkWaking: %v", err)
			}
		}},
		{"asleep", func(t *testing.T, f *actFixture, groupID string, at time.Time) {
			if _, _, err := f.st.RegisterWorker(t.Context(), store.Worker{ID: "m1", FarmID: capFarm, Status: store.WorkerStatusOnline, CPUCount: 4}); err != nil {
				t.Fatalf("RegisterWorker: %v", err)
			}
			if _, ok, err := f.st.BeginRetirement(t.Context(), groupID, "m1", at); err != nil || !ok {
				t.Fatalf("BeginRetirement = %v, %v", ok, err)
			}
			if _, _, ok, err := f.st.FinishRetirement(t.Context(), "m1", at); err != nil || !ok {
				t.Fatalf("FinishRetirement = %v, %v", ok, err)
			}
		}},
		{"wake failed", func(t *testing.T, f *actFixture, groupID string, at time.Time) {
			if _, err := f.st.MarkWaking(t.Context(), groupID, []string{"m1"}, at); err != nil {
				t.Fatalf("MarkWaking: %v", err)
			}
			if _, err := f.st.FailWakes(t.Context(), groupID, at.Add(time.Second), at, "x"); err != nil {
				t.Fatalf("FailWakes: %v", err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newActFixture(t, nil)
			f.s.ctx = t.Context()
			f.s.leaseHoldTimeout = 30 * time.Second // only a wake answers within the test
			g := createGroup(t, f.st, wolGroup("lan", nil))
			addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), false)
			f.sweep(t, time.Now().UTC()) // the scheduler learns the farm manages power
			tc.prior(t, f, g.ID, time.Now().UTC().Add(-time.Minute))
			tasks := submitReady(t, f.st, 1, store.Step{})

			done := make(chan []string, 1)
			go func() {
				var ids []string
				for _, raw := range f.s.holdDisabledLease(f.s.ctx, "m1", "") {
					var m protocol.AssignMsg
					if err := json.Unmarshal(raw, &m); err == nil {
						ids = append(ids, m.TaskID)
					}
				}
				done <- ids
			}()
			waitParkedDisabled(t, f.s, "m1")

			msg := &fakeJSMsg{
				subject: bus.WorkerRegisterSubject("m1"),
				data: workerMsgJSON(t, protocol.RegisterMsg{
					Version: protocol.ProtocolVersion, Type: protocol.TypeRegister,
					WorkerID: "m1", FarmID: capFarm, Hostname: "m1", OS: "linux", Arch: "amd64", CPUCount: 4,
				}),
			}
			f.s.handleWorkerMessage(msg)
			if !msg.acked {
				t.Fatal("registration not acked")
			}
			if w := f.worker(t, "m1"); w.PowerState != store.PowerAwake || w.Status != store.WorkerStatusOnline {
				t.Fatalf("m1 = %s/%s, want awake and online", w.PowerState, w.Status)
			}
			if out := f.logs.String(); !strings.Contains(out, "capacity group member woke") || !strings.Contains(out, "worker_id=m1") {
				t.Errorf("log lacks the wake:\n%s", out)
			}
			select {
			case ids := <-done:
				if len(ids) != 1 || ids[0] != tasks[0].ID {
					t.Errorf("released lease = %v, want [%s]", ids, tasks[0].ID)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the parked lease was not released by the registration")
			}
		})
	}
}

// waitParkedDisabled waits until workerID has a lease request parked through
// the disabled-worker hold.
func waitParkedDisabled(t *testing.T, s *Scheduler, workerID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.disabledWaiters.mu.Lock()
		parked := len(s.disabledWaiters.waiters[workerID])
		s.disabledWaiters.mu.Unlock()
		if parked > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no lease request of %s parked through the disabled-worker hold", workerID)
		}
		time.Sleep(time.Millisecond)
	}
}

// A retiring or asleep worker may not take new work: its lease request is
// parked through the disabled-worker hold and answered with nothing.
func TestLease_RetiringAndAsleepParked(t *testing.T) {
	tests := []struct {
		name  string
		state func(t *testing.T, st *fake.Store, groupID string)
	}{
		{"retiring", func(t *testing.T, st *fake.Store, groupID string) {
			if _, ok, err := st.BeginRetirement(t.Context(), groupID, "w1", time.Now().UTC()); err != nil || !ok {
				t.Fatalf("BeginRetirement = %v, %v", ok, err)
			}
		}},
		{"asleep", func(t *testing.T, st *fake.Store, groupID string) {
			if _, ok, err := st.BeginRetirement(t.Context(), groupID, "w1", time.Now().UTC()); err != nil || !ok {
				t.Fatalf("BeginRetirement = %v, %v", ok, err)
			}
			if _, _, ok, err := st.FinishRetirement(t.Context(), "w1", time.Now().UTC()); err != nil || !ok {
				t.Fatalf("FinishRetirement = %v, %v", ok, err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newCheckedFake(t)
			s := newMetricsScheduler(st, &recordBus{}, "f1")
			s.ctx = t.Context()
			s.leaseHoldTimeout = 30 * time.Second
			one := 1
			w, ids := seedLeaseFixture(t, st, []*int{&one})
			g := createGroup(t, st, wolGroup("lan", func(g *store.CapacityGroup) { g.FarmID = w.FarmID }))
			if _, err := st.SetWorkerCapacityGroup(t.Context(), w.ID, g.ID, false); err != nil {
				t.Fatalf("SetWorkerCapacityGroup: %v", err)
			}
			tc.state(t, st, g.ID)

			req, err := json.Marshal(leaseRequest{WorkerID: w.ID})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			done := make(chan []byte, 1)
			go func() { done <- s.handleLeaseRequest(w.ID, "q1", req) }()
			waitParkedDisabled(t, s, w.ID)
			s.WakeWorker(w.ID)

			select {
			case raw := <-done:
				var rep leaseReply
				if err := json.Unmarshal(raw, &rep); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if len(rep.Assignments) != 0 {
					t.Fatalf("assignments = %d, want none for a %s worker", len(rep.Assignments), tc.name)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the woken request was not answered")
			}
			if task := mustTaskOf(t, st, ids[0]); task.Status != store.TaskStatusReady {
				t.Fatalf("task = %s, want still ready", task.Status)
			}
		})
	}
}

// ── no action without an acting group ────────────────────────────────────────

// External groups are evaluated and exported as before and never acted on:
// no power transition, no control request, no packet and no new series, even
// with wake and retire candidates.
func TestCapacitySweep_ExternalGroupsNeverAct(t *testing.T) {
	var spy *spyStore
	f := newActFixture(t, func(st *fake.Store) store.Store {
		spy = &spyStore{Store: st, calls: map[string]int{}}
		return spy
	})
	named := createGroup(t, f.st, wolGroup("onprem", func(g *store.CapacityGroup) {
		g.Provider = store.CapacityProviderExternal
		g.Priority = 10
	}))
	sized := createGroup(t, f.st, sizedGroup("cloud", nil))
	addNamedMember(t, f.st, named.ID, lanWorker("off", "192.168.1.21", "aa:bb:cc:dd:ee:01"), false)
	addNamedMember(t, f.st, named.ID, lanWorker("idle", "192.168.1.22", "aa:bb:cc:dd:ee:02"), true)
	addMember(t, f.st, sized.ID, store.Worker{ID: "cloud-idle", CPUCount: 4, OS: "linux", Arch: "amd64"})
	t0 := time.Now().UTC()

	// Idle members past their idle timeout: retire candidates.
	f.sweep(t, t0)
	f.sweep(t, t0.Add(2*time.Minute))
	if st, ok := f.s.CapacityStatus(named.ID); !ok || len(st.RetireCandidates) == 0 {
		t.Fatalf("named status = %+v, want retire candidates (the signal is still computed)", st)
	}
	// More work than the idle members absorb: a wake candidate.
	submitReady(t, f.st, 3, store.Step{})
	f.sweep(t, t0.Add(3*time.Minute))
	if st, ok := f.s.CapacityStatus(named.ID); !ok || !slices.Equal(st.WakeCandidates, []string{"off"}) {
		t.Fatalf("named status = %+v, want wake candidate off", st)
	}
	spy.mu.Lock()
	calls := maps.Clone(spy.calls)
	spy.mu.Unlock()
	for _, m := range powerMethods {
		if calls[m] != 0 {
			t.Errorf("%s called %d times, want 0", m, calls[m])
		}
	}
	if c := f.bus.sent(); len(c) != 0 {
		t.Errorf("control requests = %+v, want none", c)
	}
	if p := f.packets.all(); len(p) != 0 {
		t.Errorf("packets = %+v, want none", p)
	}
	for key := range capacitySeries(t, f.s.metrics) {
		if !strings.HasPrefix(key, "sqi_capacity_demand_cores") && !strings.HasPrefix(key, "sqi_capacity_desired_machines") &&
			!strings.HasPrefix(key, "sqi_capacity_members") {
			t.Errorf("series %s appeared for external groups", key)
		}
	}
	if out := f.logs.String(); out != "" {
		t.Errorf("log output = %q, want none", out)
	}
	for _, id := range []string{"off", "idle", "cloud-idle"} {
		if got := f.worker(t, id).PowerState; got != store.PowerAwake {
			t.Errorf("%s = %s, want awake", id, got)
		}
	}
}

// powerMethods are the store's power transitions, which only an acting group
// may cause.
var powerMethods = []string{
	"MarkWaking", "FailWakes", "BeginRetirement", "FinishRetirement", "AbortRetirement", "ExpireRetirements",
}

func (s *spyStore) MarkWaking(ctx context.Context, groupID string, ids []string, now time.Time) ([]string, error) {
	s.count("MarkWaking")
	return s.Store.MarkWaking(ctx, groupID, ids, now)
}

func (s *spyStore) FailWakes(ctx context.Context, groupID string, cutoff, now time.Time, reason string) ([]string, error) {
	s.count("FailWakes")
	return s.Store.FailWakes(ctx, groupID, cutoff, now, reason)
}

func (s *spyStore) BeginRetirement(ctx context.Context, groupID, workerID string, now time.Time) (store.Worker, bool, error) {
	s.count("BeginRetirement")
	return s.Store.BeginRetirement(ctx, groupID, workerID, now)
}

func (s *spyStore) FinishRetirement(ctx context.Context, workerID string, now time.Time) (store.Worker, bool, bool, error) {
	s.count("FinishRetirement")
	return s.Store.FinishRetirement(ctx, workerID, now)
}

func (s *spyStore) AbortRetirement(ctx context.Context, workerID string, now time.Time, reason string) (bool, error) {
	s.count("AbortRetirement")
	return s.Store.AbortRetirement(ctx, workerID, now, reason)
}

func (s *spyStore) ExpireRetirements(ctx context.Context, groupID string, cutoff, now time.Time, reason string) ([]string, error) {
	s.count("ExpireRetirements")
	return s.Store.ExpireRetirements(ctx, groupID, cutoff, now, reason)
}

// A heartbeat from an asleep member's still-running process (a sleep that has
// not taken effect yet) records its liveness and nothing else: the member
// stays asleep and offline, and its lease request is parked.
func TestHeartbeat_DoesNotReviveAsleepMember(t *testing.T) {
	f := newActFixture(t, nil)
	f.s.ctx = t.Context()
	f.s.leaseHoldTimeout = 30 * time.Second
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	now := time.Now().UTC()
	if _, ok, err := f.st.BeginRetirement(t.Context(), g.ID, "m1", now); err != nil || !ok {
		t.Fatalf("BeginRetirement = %v, %v", ok, err)
	}
	if _, _, ok, err := f.st.FinishRetirement(t.Context(), "m1", now); err != nil || !ok {
		t.Fatalf("FinishRetirement = %v, %v", ok, err)
	}
	submitReady(t, f.st, 1, store.Step{})

	msg := &fakeJSMsg{
		subject: bus.WorkerHeartbeatSubject("m1"),
		data:    workerMsgJSON(t, protocol.HeartbeatMsg{Version: protocol.ProtocolVersion, WorkerID: "m1", At: now}),
	}
	f.s.handleWorkerMessage(msg)
	if !msg.acked {
		t.Fatal("heartbeat not acked")
	}
	w := f.worker(t, "m1")
	if w.PowerState != store.PowerAsleep || w.Status != store.WorkerStatusOffline {
		t.Fatalf("m1 after a heartbeat = %s/%s, want asleep and offline", w.PowerState, w.Status)
	}

	req, err := json.Marshal(leaseRequest{WorkerID: "m1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	done := make(chan []byte, 1)
	go func() { done <- f.s.handleLeaseRequest("m1", capQueue, req) }()
	waitParkedDisabled(t, f.s, "m1")
	f.s.WakeWorker("m1")
	select {
	case raw := <-done:
		var rep leaseReply
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(rep.Assignments) != 0 {
			t.Errorf("an asleep member was leased %d tasks", len(rep.Assignments))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the woken request was not answered")
	}
}

// A group naming a script provider the server does not configure does not
// act: it records why as its last action, and logs it once.
func TestActuator_UnconfiguredScriptRecordsConfigureFailure(t *testing.T) {
	f := newActFixture(t, nil)
	g := createGroup(t, f.st, wolGroup("lan", func(g *store.CapacityGroup) { g.Provider = "script:missing" }))
	addNamedMember(t, f.st, g.ID, lanWorker("idle", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	addNamedMember(t, f.st, g.ID, lanWorker("off", "192.168.1.22", "aa:bb:cc:dd:ee:02"), false)
	t0 := time.Now().UTC()

	for i := range 4 {
		f.sweep(t, t0.Add(time.Duration(i)*time.Minute))
	}

	la := lastAction(t, f.s, g.ID)
	if la.Action != "configure" || la.Result != "failed" || la.Message != "provider script:missing is not configured on this server" {
		t.Errorf("LastAction = %+v, want configure failed naming the provider", la)
	}
	if n := strings.Count(f.logs.String(), "does not configure"); n != 1 {
		t.Errorf("configuration warning logged %d times over four sweeps, want 1:\n%s", n, f.logs.String())
	}
	if c := f.bus.sent(); len(c) != 0 {
		t.Errorf("control requests = %+v, want none", c)
	}
	if got := f.worker(t, "idle").PowerState; got != store.PowerAwake {
		t.Errorf("idle = %s, want awake", got)
	}
}
