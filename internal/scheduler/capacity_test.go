// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
)

const (
	capFarm  = "farm-1"
	capQueue = "queue-1"
)

// newCapacityFixture returns a fake store holding one farm and queue, a
// scheduler over it (through wrap, when given) and the scheduler's log output.
func newCapacityFixture(t *testing.T, wrap func(*fake.Store) store.Store) (*fake.Store, *Scheduler, *bytes.Buffer) {
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
	s := newMetricsScheduler(used, &recordBus{}, "")
	logs := &bytes.Buffer{}
	s.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return st, s, logs
}

// sizedGroup is a valid, enabled sized group of 4-CPU machines serving every
// queue, changed by mut.
func sizedGroup(name string, mut func(*store.CapacityGroup)) store.CapacityGroup {
	g := store.CapacityGroup{
		ID: uuid.NewString(), FarmID: capFarm, Name: name,
		Kind: store.CapacityGroupSized, Provider: store.CapacityProviderExternal, Enabled: true,
		MinMachines: 0, MaxMachines: 10, MaxStepUp: 10,
		IdleTimeout: time.Minute, BootTimeout: 5 * time.Minute,
		RetireAction: store.CapacityRetireShutdown,
		Profile:      store.MachineProfile{CPUCount: 4, OS: "linux", Arch: "amd64"},
	}
	if mut != nil {
		mut(&g)
	}
	return g
}

func createGroup(t *testing.T, st store.Store, g store.CapacityGroup) store.CapacityGroup {
	t.Helper()
	out, err := st.CreateCapacityGroup(t.Context(), g)
	if err != nil {
		t.Fatalf("CreateCapacityGroup %s: %v", g.Name, err)
	}
	return out
}

// addMember registers w online and makes it an ephemeral member of groupID.
func addMember(t *testing.T, st store.Store, groupID string, w store.Worker) store.Worker {
	t.Helper()
	if w.FarmID == "" {
		w.FarmID = capFarm
	}
	w.Status = store.WorkerStatusOnline
	if _, _, err := st.RegisterWorker(t.Context(), w); err != nil {
		t.Fatalf("RegisterWorker %s: %v", w.ID, err)
	}
	out, err := st.SetWorkerCapacityGroup(t.Context(), w.ID, groupID, true)
	if err != nil {
		t.Fatalf("SetWorkerCapacityGroup %s: %v", w.ID, err)
	}
	return out
}

// submitReady submits a running job in capQueue whose only step (built from
// step) holds n ready tasks declaring no cores.
func submitReady(t *testing.T, st store.Store, n int, step store.Step) []store.Task {
	t.Helper()
	now := time.Now().UTC()
	job := store.Job{
		ID: uuid.NewString(), FarmID: capFarm, QueueID: capQueue, Name: "job",
		Status: store.JobStatusRunning, RawTemplate: minimalJobJSON, TemplateFormat: store.TemplateFormatJSON,
		CreatedAt: now, UpdatedAt: now,
	}
	step.ID, step.JobID, step.Name, step.Status = uuid.NewString(), job.ID, "Render", store.StepStatusRunning
	tasks := make([]store.Task, n)
	for i := range tasks {
		tasks[i] = store.Task{
			ID: uuid.NewString(), JobID: job.ID, StepID: step.ID, Name: "t",
			Status: store.TaskStatusReady, CreatedAt: now, UpdatedAt: now,
		}
	}
	storetest.Submit(t, st, store.JobSubmission{Job: job, Steps: []store.Step{step}, Tasks: tasks})
	return tasks
}

// houdiniStep needs a worker tagged houdini=20.5.
func houdiniStep() store.Step {
	return store.Step{HostRequirements: &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{{Name: "attr.worker.tag.houdini", AnyOf: []string{"20.5"}}},
	}}
}

// capacitySeries reads every sqi_capacity_* series without creating any,
// keyed name{label=value,...} with labels sorted.
func capacitySeries(t *testing.T, m *metrics.Metrics) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for name, c := range map[string]prometheus.Collector{
		"sqi_capacity_demand_cores":        m.CapacityDemandCores,
		"sqi_capacity_desired_machines":    m.CapacityDesiredMachines,
		"sqi_capacity_members":             m.CapacityMembers,
		"sqi_capacity_machines":            m.CapacityMachines,
		"sqi_capacity_actions_total":       m.CapacityActionsTotal,
		"sqi_capacity_boot_timeouts_total": m.CapacityBootTimeoutsTotal,
		"sqi_capacity_group_degraded":      m.CapacityGroupDegraded,
	} {
		ch := make(chan prometheus.Metric)
		go func() { c.Collect(ch); close(ch) }()
		for pm := range ch {
			var d dto.Metric
			if err := pm.Write(&d); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
			var labels []string
			for _, l := range d.GetLabel() {
				labels = append(labels, l.GetName()+"="+l.GetValue())
			}
			sort.Strings(labels)
			v := d.GetGauge().GetValue()
			if d.Counter != nil {
				v = d.GetCounter().GetValue()
			}
			out[name+"{"+strings.Join(labels, ",")+"}"] = v
		}
	}
	return out
}

func statusFor(t *testing.T, s *Scheduler, groupID string) capacityStatusView {
	t.Helper()
	st, ok := s.CapacityStatus(groupID)
	if !ok {
		t.Fatalf("CapacityStatus(%s): no status", groupID)
	}
	return capacityStatusView{st.DemandCores, st.DesiredMachines, st.RetireCandidates}
}

type capacityStatusView struct {
	demand, desired int
	retire          []string
}

// spyStore counts the calls the capacity sweep could make.
type spyStore struct {
	store.Store

	mu    sync.Mutex
	calls map[string]int
}

func (s *spyStore) count(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[name]++
}

func (s *spyStore) ListCapacityGroups(ctx context.Context) ([]store.CapacityGroup, error) {
	s.count("ListCapacityGroups")
	return s.Store.ListCapacityGroups(ctx)
}

func (s *spyStore) ListCapacityGroupMembers(ctx context.Context, id string) ([]store.Worker, error) {
	s.count("ListCapacityGroupMembers")
	return s.Store.ListCapacityGroupMembers(ctx, id)
}

func (s *spyStore) ListReadyStepDemand(ctx context.Context, farmID string, now time.Time) ([]store.StepDemand, error) {
	s.count("ListReadyStepDemand")
	return s.Store.ListReadyStepDemand(ctx, farmID, now)
}

func (s *spyStore) ListWorkerCommitments(ctx context.Context, farmID string) (map[string]store.WorkerCommitment, error) {
	s.count("ListWorkerCommitments")
	return s.Store.ListWorkerCommitments(ctx, farmID)
}

func (s *spyStore) ListWorkers(ctx context.Context, opts store.ListWorkersOptions) (store.Page[store.Worker], error) {
	s.count("ListWorkers")
	return s.Store.ListWorkers(ctx, opts)
}

func (s *spyStore) GetJob(ctx context.Context, id string) (store.Job, error) {
	s.count("GetJob")
	return s.Store.GetJob(ctx, id)
}

func (s *spyStore) GetStep(ctx context.Context, id string) (store.Step, error) {
	s.count("GetStep")
	return s.Store.GetStep(ctx, id)
}

func (s *spyStore) ListUsagePools(ctx context.Context) ([]store.UsagePool, error) {
	s.count("ListUsagePools")
	return s.Store.ListUsagePools(ctx)
}

// With no capacity group the sweep makes one query and changes nothing: no
// demand query, no log line, no series, no power transition, no control
// request and no packet. With only a disabled group it also reads that group's
// members, whose vanished ephemeral machines it must still remove, and nothing
// else.
func TestCapacitySweep_NoGroupsDoesNothing(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, st *fake.Store)
		want  map[string]int
	}{
		{"no groups", func(*testing.T, *fake.Store) {}, map[string]int{"ListCapacityGroups": 2}},
		{"only a disabled group", func(t *testing.T, st *fake.Store) {
			createGroup(t, st, sizedGroup("off", func(g *store.CapacityGroup) { g.Enabled = false }))
		}, map[string]int{"ListCapacityGroups": 2, "ListCapacityGroupMembers": 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var spy *spyStore
			st, s, logs := newCapacityFixture(t, func(st *fake.Store) store.Store {
				spy = &spyStore{Store: st, calls: map[string]int{}}
				return spy
			})
			ctl, packets := &controlBus{}, &packetRecorder{}
			s.bus = ctl
			s.cfg.SendWakePackets = packets.send
			tc.setup(t, st)
			// Work and an idle worker exist, so any demand query would find some.
			submitReady(t, st, 3, store.Step{})
			if _, _, err := st.RegisterWorker(t.Context(), store.Worker{
				ID: "w1", FarmID: capFarm, CPUCount: 4, Status: store.WorkerStatusOnline,
			}); err != nil {
				t.Fatalf("RegisterWorker: %v", err)
			}

			s.sweepCapacity(t.Context())
			s.sweepCapacity(t.Context())

			spy.mu.Lock()
			got := maps.Clone(spy.calls)
			spy.mu.Unlock()
			if !maps.Equal(got, tc.want) {
				t.Errorf("store calls = %v, want %v", got, tc.want)
			}
			if n := len(capacitySeries(t, s.metrics)); n != 0 {
				t.Errorf("sqi_capacity_* series = %d, want 0: %v", n, capacitySeries(t, s.metrics))
			}
			if logs.Len() != 0 {
				t.Errorf("log output = %q, want none", logs.String())
			}
			if sts := s.capacityStatuses(); len(sts) != 0 {
				t.Errorf("CapacityStatuses = %v, want none", sts)
			}
			if c := ctl.sent(); len(c) != 0 {
				t.Errorf("control requests = %+v, want none", c)
			}
			if p := packets.all(); len(p) != 0 {
				t.Errorf("packets = %+v, want none", p)
			}
		})
	}
}

// Only demand a group's machine could run counts toward it: a higher-priority
// group without the tag the work needs sees none of it.
func TestCapacitySweep_ExportsDemandForEligibleWork(t *testing.T) {
	st, s, _ := newCapacityFixture(t, nil)
	without := createGroup(t, st, sizedGroup("plain", func(g *store.CapacityGroup) { g.Priority = 10 }))
	with := createGroup(t, st, sizedGroup("houdini", func(g *store.CapacityGroup) {
		g.Profile.Tags = map[string]string{"houdini": "20.5"}
	}))
	submitReady(t, st, 2, houdiniStep())

	s.sweepCapacity(t.Context())

	series := capacitySeries(t, s.metrics)
	for key, want := range map[string]float64{
		"sqi_capacity_demand_cores{group=houdini}":          8,
		"sqi_capacity_desired_machines{group=houdini}":      2,
		"sqi_capacity_demand_cores{group=plain}":            0,
		"sqi_capacity_desired_machines{group=plain}":        0,
		"sqi_capacity_members{group=houdini,state=online}":  0,
		"sqi_capacity_members{group=houdini,state=offline}": 0,
		"sqi_capacity_members{group=plain,state=online}":    0,
		"sqi_capacity_members{group=plain,state=offline}":   0,
	} {
		got, ok := series[key]
		if !ok || got != want {
			t.Errorf("%s = %v (present %v), want %v", key, got, ok, want)
		}
	}
	if len(series) != 8 {
		t.Errorf("series = %v, want exactly 8", series)
	}
	if v := statusFor(t, s, with.ID); v.demand != 8 || v.desired != 2 {
		t.Errorf("houdini status = %+v, want demand 8, desired 2", v)
	}
	if v := statusFor(t, s, without.ID); v.demand != 0 || v.desired != 0 {
		t.Errorf("plain status = %+v, want demand 0, desired 0", v)
	}
	all := s.capacityStatuses()
	if len(all) != 2 || all[0].GroupID != without.ID || all[1].GroupID != with.ID {
		t.Errorf("CapacityStatuses = %+v, want plain then houdini", all)
	}
}

// Once the work is taken, the signal falls to zero rather than keeping its
// last value.
func TestCapacitySweep_DemandFallsToZero(t *testing.T) {
	st, s, _ := newCapacityFixture(t, nil)
	g := createGroup(t, st, sizedGroup("burst", nil))
	tasks := submitReady(t, st, 2, store.Step{})

	s.sweepCapacity(t.Context())
	if v := statusFor(t, s, g.ID); v.demand != 8 {
		t.Fatalf("demand before = %d, want 8", v.demand)
	}

	// A worker outside the group takes both tasks.
	if _, _, err := st.RegisterWorker(t.Context(), store.Worker{
		ID: "outsider", FarmID: capFarm, CPUCount: 4, Status: store.WorkerStatusOnline,
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	for _, task := range tasks {
		storetest.Lease(t, st, store.LeaseRequest{TaskID: task.ID, WorkerID: "outsider"})
	}
	s.sweepCapacity(t.Context())

	series := capacitySeries(t, s.metrics)
	for _, key := range []string{"sqi_capacity_demand_cores{group=burst}", "sqi_capacity_desired_machines{group=burst}"} {
		if got, ok := series[key]; !ok || got != 0 {
			t.Errorf("%s = %v (present %v), want 0", key, got, ok)
		}
	}
	if v := statusFor(t, s, g.ID); v.demand != 0 || v.desired != 0 {
		t.Errorf("status after = %+v, want demand 0, desired 0", v)
	}
}

// A group that is deleted, disabled or renamed leaves no series behind.
func TestCapacitySweep_DeletedGroupSeriesRemoved(t *testing.T) {
	st, s, _ := newCapacityFixture(t, nil)
	keep := createGroup(t, st, sizedGroup("keep", nil))
	renamed := createGroup(t, st, sizedGroup("old-name", nil))
	deleted := createGroup(t, st, sizedGroup("deleted", nil))
	disabled := createGroup(t, st, sizedGroup("disabled", nil))
	submitReady(t, st, 1, store.Step{})

	s.sweepCapacity(t.Context())
	if n := len(capacitySeries(t, s.metrics)); n != 16 {
		t.Fatalf("series before = %d, want 16 (4 groups x 4)", n)
	}

	if err := st.DeleteCapacityGroup(t.Context(), deleted.ID); err != nil {
		t.Fatalf("DeleteCapacityGroup: %v", err)
	}
	renamed.Name = "new-name"
	if _, err := st.UpdateCapacityGroup(t.Context(), renamed); err != nil {
		t.Fatalf("rename: %v", err)
	}
	disabled.Enabled = false
	if _, err := st.UpdateCapacityGroup(t.Context(), disabled); err != nil {
		t.Fatalf("disable: %v", err)
	}
	s.sweepCapacity(t.Context())

	var groups []string
	for key := range capacitySeries(t, s.metrics) {
		_, rest, _ := strings.Cut(key, "{group=")
		name, _, _ := strings.Cut(rest, ",")
		name = strings.TrimSuffix(name, "}")
		if !slices.Contains(groups, name) {
			groups = append(groups, name)
		}
	}
	slices.Sort(groups)
	if want := []string{"keep", "new-name"}; !slices.Equal(groups, want) {
		t.Errorf("groups with series = %v, want %v", groups, want)
	}
	if n := len(capacitySeries(t, s.metrics)); n != 8 {
		t.Errorf("series after = %d, want 8", n)
	}
	for _, id := range []string{deleted.ID, disabled.ID} {
		if _, ok := s.CapacityStatus(id); ok {
			t.Errorf("CapacityStatus(%s) still present", id)
		}
	}
	if _, ok := s.CapacityStatus(keep.ID); !ok {
		t.Error("CapacityStatus(keep) missing")
	}

	// The last group going clears everything.
	for _, g := range []store.CapacityGroup{keep, renamed} {
		g.Enabled = false
		if _, err := st.UpdateCapacityGroup(t.Context(), g); err != nil {
			t.Fatalf("disable %s: %v", g.Name, err)
		}
	}
	s.sweepCapacity(t.Context())
	if n := len(capacitySeries(t, s.metrics)); n != 0 {
		t.Errorf("series with no enabled group = %d, want 0", n)
	}
	if sts := s.capacityStatuses(); len(sts) != 0 {
		t.Errorf("CapacityStatuses with no enabled group = %v, want none", sts)
	}
}

// A member's idle time starts over when it is seen busy, so a member that
// worked a moment ago is not retired on the strength of an earlier idle spell.
func TestCapacitySweep_IdleSinceResetsWhenBusy(t *testing.T) {
	st, s, _ := newCapacityFixture(t, nil)
	g := createGroup(t, st, sizedGroup("burst", nil))
	addMember(t, st, g.ID, store.Worker{ID: "m1", CPUCount: 4, OS: "linux", Arch: "amd64"})
	t0 := time.Now().UTC()

	s.sweepCapacityAt(t.Context(), t0)
	s.sweepCapacityAt(t.Context(), t0.Add(2*time.Minute))
	if v := statusFor(t, s, g.ID); !slices.Equal(v.retire, []string{"m1"}) {
		t.Fatalf("retire after 2m idle = %v, want [m1]", v.retire)
	}

	tasks := submitReady(t, st, 1, store.Step{})
	attempt := storetest.Lease(t, st, store.LeaseRequest{TaskID: tasks[0].ID, WorkerID: "m1"})
	s.sweepCapacityAt(t.Context(), t0.Add(3*time.Minute))
	if v := statusFor(t, s, g.ID); len(v.retire) != 0 {
		t.Fatalf("retire while busy = %v, want none", v.retire)
	}

	storetest.Complete(t, st, attempt, store.TaskStatusSucceeded)
	s.sweepCapacityAt(t.Context(), t0.Add(3*time.Minute+30*time.Second))
	if v := statusFor(t, s, g.ID); len(v.retire) != 0 {
		t.Errorf("retire 30s after the task ended = %v, want none (idle time must restart)", v.retire)
	}
	s.sweepCapacityAt(t.Context(), t0.Add(4*time.Minute+30*time.Second))
	if v := statusFor(t, s, g.ID); !slices.Equal(v.retire, []string{"m1"}) {
		t.Errorf("retire a minute after the task ended = %v, want [m1]", v.retire)
	}
}

// A sized group's online members are checked against its profile; each one
// that falls short is reported once with a reason and logged once.
func TestCapacitySweep_ReportsProfileMismatch(t *testing.T) {
	st, s, logs := newCapacityFixture(t, nil)
	g := createGroup(t, st, sizedGroup("burst", func(g *store.CapacityGroup) {
		g.Profile.Tags = map[string]string{"houdini": "20.5"}
	}))
	tags := map[string]string{"houdini": "20.5"}
	addMember(t, st, g.ID, store.Worker{ID: "good", CPUCount: 4, OS: "linux", Arch: "amd64", Tags: tags})
	addMember(t, st, g.ID, store.Worker{ID: "small", CPUCount: 2, OS: "linux", Arch: "amd64", Tags: tags})
	addMember(t, st, g.ID, store.Worker{ID: "untagged", CPUCount: 4, OS: "linux", Arch: "amd64"})

	s.sweepCapacity(t.Context())
	s.sweepCapacity(t.Context())

	st0, ok := s.CapacityStatus(g.ID)
	if !ok {
		t.Fatal("no status")
	}
	got := map[string]string{}
	for _, m := range st0.Mismatches {
		if _, dup := got[m.WorkerID]; dup {
			t.Errorf("worker %s reported twice", m.WorkerID)
		}
		got[m.WorkerID] = m.Reason
	}
	if len(got) != 2 {
		t.Fatalf("mismatches = %+v, want small and untagged", st0.Mismatches)
	}
	if r := got["small"]; !strings.Contains(r, "cpu") {
		t.Errorf("small's reason = %q, want it to name the CPU count", r)
	}
	if r := got["untagged"]; !strings.Contains(r, "houdini") {
		t.Errorf("untagged's reason = %q, want it to name the tag", r)
	}
	for _, id := range []string{"small", "untagged"} {
		if n := strings.Count(logs.String(), "worker_id="+id); n != 1 {
			t.Errorf("%s logged %d times over two sweeps, want 1:\n%s", id, n, logs.String())
		}
	}
	if strings.Contains(logs.String(), "worker_id=good") {
		t.Errorf("matching member logged:\n%s", logs.String())
	}
	// A mismatch never removes the member.
	if st0.OnlineMembers != 3 {
		t.Errorf("OnlineMembers = %d, want 3", st0.OnlineMembers)
	}
}

// TestOnlineWorkers_ReadsPastOnePage pins that the online-worker read the
// capacity and unschedulable sweeps share sees a farm larger than one page:
// a worker past the first [store.MaxLimit] would otherwise never absorb
// demand, and a group would be sized for work it could already run.
func TestOnlineWorkers_ReadsPastOnePage(t *testing.T) {
	st, s, _ := newCapacityFixture(t, nil)
	want := store.MaxLimit + 3
	for i := range want {
		w := store.Worker{
			ID: uuid.NewString(), FarmID: capFarm, Hostname: fmt.Sprintf("host-%05d", i),
			Status: store.WorkerStatusOnline, CPUCount: 4,
		}
		if _, _, err := st.RegisterWorker(t.Context(), w); err != nil {
			t.Fatalf("RegisterWorker %d: %v", i, err)
		}
	}
	got, err := s.onlineWorkers(t.Context())
	if err != nil {
		t.Fatalf("onlineWorkers: %v", err)
	}
	ids := map[string]bool{}
	for _, w := range got {
		ids[w.ID] = true
	}
	if len(got) != want || len(ids) != want {
		t.Fatalf("onlineWorkers returned %d workers (%d distinct), want %d", len(got), len(ids), want)
	}
}
