// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for the assignment path in scheduler.go: tryAssign, pickWorker,
// createAttemptAndClaimUsage, buildUsageContext, nextAttemptNumber,
// revertTaskToReady, dispatchBatch, and the instrumentation gauges.
//
// These are white-box tests in package scheduler. A fake store (fake.New)
// drives all store-backed paths; a recording bus stub satisfies busClient so
// PublishWorkAssign calls can be observed without a real NATS broker. A real
// metrics.New() is used because the assignment/gauge helpers touch
// s.metrics directly.

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/ws"
)

// ── recording bus ─────────────────────────────────────────────────────────────

// recordBus records PublishWorkAssign calls and can be configured to fail.
type recordBus struct {
	assignErr   error
	assignCalls []string // queueIDs published to
}

func (*recordBus) ConsumeWorker(_ context.Context, _ jetstream.MessageHandler) (jetstream.ConsumeContext, error) {
	return nil, nil
}

func (*recordBus) ConsumeTaskStatus(_ context.Context, _ jetstream.MessageHandler) (jetstream.ConsumeContext, error) {
	return nil, nil
}

func (*recordBus) ConsumeTaskLogs(_ context.Context, _ jetstream.MessageHandler) (jetstream.ConsumeContext, error) {
	return nil, nil
}

func (b *recordBus) PublishWorkAssign(_ context.Context, queueID string, _ []byte) error {
	b.assignCalls = append(b.assignCalls, queueID)
	return b.assignErr
}

func (*recordBus) PublishTaskCancel(_ context.Context, _ string, _ []byte) error { return nil }

func (*recordBus) SubscribeWorkerDiag(_ func(subject string, data []byte)) (*nats.Subscription, error) {
	return nil, nil
}

func (*recordBus) SubscribeLease(_ func(string, []byte) []byte) (*nats.Subscription, error) {
	return nil, nil
}

// newMetricsScheduler builds a Scheduler with a real metrics registry so the
// gauge/observe helpers do not nil-panic. cfg.FarmID is set to farmID.
func newMetricsScheduler(st store.Store, bus busClient, farmID string) *Scheduler {
	cfg := DefaultConfig()
	cfg.FarmID = farmID
	s := New(cfg, st, bus, metrics.New(), slog.New(slog.DiscardHandler), ws.NoopNotifier{}, nil)
	s.ctx = context.Background()
	return s
}

// assignFixture holds the records seeded for an assignment test.
type assignFixture struct {
	farm   store.Farm
	queue  store.Queue
	job    store.Job
	step   store.Step
	task   store.Task
	worker store.Worker
}

// seedAssignFixture creates a complete farm/queue/job/step/ready-task plus a
// single online worker eligible to run the task. mutate may adjust records
// before they are persisted (e.g. add host requirements or compute location).
func seedAssignFixture(t *testing.T, st *fake.Store, mutate func(*assignFixture)) assignFixture {
	t.Helper()
	ctx := t.Context()
	now := time.Now()

	f := assignFixture{
		farm:  store.Farm{ID: "farm-1", Name: "farm"},
		queue: store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "queue"},
		job: store.Job{
			ID:             uuid.NewString(),
			FarmID:         "farm-1",
			QueueID:        "queue-1",
			Name:           "TestJob",
			Status:         store.JobStatusRunning,
			RawTemplate:    minimalJobJSON,
			TemplateFormat: store.TemplateFormatJSON,
			CreatedAt:      now,
			UpdatedAt:      now,
		},
	}
	f.step = store.Step{
		ID: uuid.NewString(), JobID: f.job.ID, Name: "Render",
		Status: store.StepStatusRunning, CreatedAt: now, UpdatedAt: now,
	}
	f.task = store.Task{
		ID: uuid.NewString(), JobID: f.job.ID, StepID: f.step.ID,
		Name: "Render[1]", Status: store.TaskStatusReady, CreatedAt: now, UpdatedAt: now,
	}
	f.worker = store.Worker{
		ID: "worker-1", FarmID: "farm-1", Hostname: "node-1",
		Status: store.WorkerStatusOnline, RegisteredAt: now, UpdatedAt: now,
	}

	if mutate != nil {
		mutate(&f)
	}

	if _, err := st.CreateFarm(ctx, f.farm); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, f.queue); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	if _, err := st.CreateJob(ctx, f.job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := st.CreateStep(ctx, f.step); err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	if _, err := st.CreateTask(ctx, f.task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := st.RegisterWorker(ctx, f.worker); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	return f
}

// ── tryAssign happy path ──────────────────────────────────────────────────────

func TestTryAssign_Success(t *testing.T) {
	st := fake.New()
	bus := &recordBus{}
	s := newMetricsScheduler(st, bus, "farm-1")

	f := seedAssignFixture(t, st, nil)

	if err := s.tryAssign(t.Context(), f.task); err != nil {
		t.Fatalf("tryAssign: %v", err)
	}

	// Task assigned to the worker.
	got, err := st.GetTask(t.Context(), f.task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusAssigned {
		t.Errorf("task status = %q, want assigned", got.Status)
	}
	if got.AssignedWorkerID != f.worker.ID {
		t.Errorf("assigned worker = %q, want %q", got.AssignedWorkerID, f.worker.ID)
	}

	// An attempt was created (number 1).
	att, err := st.LatestTaskAttempt(t.Context(), f.task.ID)
	if err != nil {
		t.Fatalf("LatestTaskAttempt: %v", err)
	}
	if att.AttemptNumber != 1 {
		t.Errorf("attempt number = %d, want 1", att.AttemptNumber)
	}

	// Assignment was published to the job's queue exactly once.
	if len(bus.assignCalls) != 1 || bus.assignCalls[0] != f.job.QueueID {
		t.Errorf("assign publishes = %v, want [%q]", bus.assignCalls, f.job.QueueID)
	}
}

// ── no eligible worker ─────────────────────────────────────────────────────────

func TestTryAssign_NoOnlineWorker_Deferred(t *testing.T) {
	st := fake.New()
	bus := &recordBus{}
	s := newMetricsScheduler(st, bus, "farm-1")

	f := seedAssignFixture(t, st, func(f *assignFixture) {
		f.worker.Status = store.WorkerStatusOffline // no online worker
	})

	err := s.tryAssign(t.Context(), f.task)
	if !errors.Is(err, errNoWorkerAvailable) {
		t.Fatalf("tryAssign err = %v, want errNoWorkerAvailable", err)
	}
	// Task stays ready, no publish.
	got, err := st.GetTask(t.Context(), f.task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusReady {
		t.Errorf("task status = %q, want ready (unchanged)", got.Status)
	}
	if len(bus.assignCalls) != 0 {
		t.Errorf("expected no publishes, got %d", len(bus.assignCalls))
	}
}

// ── policy gate blocks assignment ─────────────────────────────────────────────

func TestTryAssign_QueuePolicyBlocked_Deferred(t *testing.T) {
	st := fake.New()
	bus := &recordBus{}
	s := newMetricsScheduler(st, bus, "farm-1")

	f := seedAssignFixture(t, st, func(f *assignFixture) {
		f.queue.MaxConcurrentTasks = 1
	})

	// Seed an already-active task in the same queue so the limit is reached.
	now := time.Now()
	if _, err := st.CreateTask(t.Context(), store.Task{
		ID: uuid.NewString(), JobID: f.job.ID, StepID: f.step.ID,
		Name: "busy", Status: store.TaskStatusRunning, AssignedWorkerID: "other",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateTask(busy): %v", err)
	}

	err := s.tryAssign(t.Context(), f.task)
	if !errors.Is(err, errNoWorkerAvailable) {
		t.Fatalf("tryAssign err = %v, want errNoWorkerAvailable (policy deferral)", err)
	}
	if len(bus.assignCalls) != 0 {
		t.Errorf("expected no publishes when policy-blocked, got %d", len(bus.assignCalls))
	}
}

// ── missing job/step/queue/farm propagate errors ──────────────────────────────

func TestTryAssign_MissingJob_Error(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")

	// Task referencing a job that does not exist.
	orphan := store.Task{ID: uuid.NewString(), JobID: "no-job", StepID: "no-step"}
	err := s.tryAssign(t.Context(), orphan)
	if err == nil || errors.Is(err, errNoWorkerAvailable) {
		t.Fatalf("tryAssign err = %v, want a real (non-deferral) error", err)
	}
}

// ── retry path: nextAttemptNumber increments ──────────────────────────────────

func TestTryAssign_Retry_IncrementsAttemptNumber(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")

	f := seedAssignFixture(t, st, nil)

	// Seed a prior (terminal) attempt #1 so the new one must be #2.
	now := time.Now()
	if _, err := st.CreateTaskAttempt(t.Context(), store.TaskAttempt{
		ID: uuid.NewString(), TaskID: f.task.ID, AttemptNumber: 1,
		Status: store.AttemptStatusFailed, StartedAt: now, EndedAt: &now, CreatedAt: now,
	}); err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}

	if err := s.tryAssign(t.Context(), f.task); err != nil {
		t.Fatalf("tryAssign: %v", err)
	}
	att, err := st.LatestTaskAttempt(t.Context(), f.task.ID)
	if err != nil {
		t.Fatalf("LatestTaskAttempt: %v", err)
	}
	if att.AttemptNumber != 2 {
		t.Errorf("attempt number = %d, want 2 on retry", att.AttemptNumber)
	}
}

// ── usage pool admission: saturated pool defers, freeing capacity resumes ─────

func TestTryAssign_UsagePool_DeferThenResume(t *testing.T) {
	st := fake.New()
	bus := &recordBus{}
	s := newMetricsScheduler(st, bus, "farm-1")

	poolID := uuid.NewString()
	f := seedAssignFixture(t, st, func(f *assignFixture) {
		f.step.HostRequirements = &store.StepHostRequirements{UsagePools: []string{"maya"}}
	})

	// Create a maya pool with capacity 1 and saturate it with an active claim.
	if _, err := st.CreateUsagePool(t.Context(), store.UsagePool{
		ID: poolID, Name: "maya", MaxConcurrent: 1,
	}); err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}
	heldClaimID := uuid.NewString()
	if err := st.TryClaimSlots(t.Context(), "held-attempt",
		[]store.UsagePoolClaim{{ClaimID: heldClaimID, PoolID: poolID, PoolName: "maya", MaxConcurrent: 1}},
		time.Now()); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	// Pool saturated → matcher rejects → deferred, task stays ready.
	if err := s.tryAssign(t.Context(), f.task); !errors.Is(err, errNoWorkerAvailable) {
		t.Fatalf("tryAssign (saturated) err = %v, want errNoWorkerAvailable", err)
	}
	got, err := st.GetTask(t.Context(), f.task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusReady {
		t.Errorf("task status = %q, want ready while pool saturated", got.Status)
	}

	// Free the slot; assignment should now succeed and claim a slot.
	if err := st.ReleaseClaim(t.Context(), heldClaimID, time.Now()); err != nil {
		t.Fatalf("ReleaseClaim: %v", err)
	}
	if err := s.tryAssign(t.Context(), f.task); err != nil {
		t.Fatalf("tryAssign (freed) err = %v, want success", err)
	}
	got, err = st.GetTask(t.Context(), f.task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusAssigned {
		t.Errorf("task status = %q, want assigned after capacity freed", got.Status)
	}
	n, err := st.ActiveClaimCount(t.Context(), poolID)
	if err != nil {
		t.Fatalf("ActiveClaimCount: %v", err)
	}
	if n != 1 {
		t.Errorf("active claims = %d, want 1 (claimed by new attempt)", n)
	}
}

// ── usage claim race: TryClaimSlots returns ErrUsageAtCapacity ───────────────

// claimCapacityErrSt forces TryClaimSlots to report the pool full even
// though buildUsageContext saw capacity, exercising the rollback branch of
// createAttemptAndClaimUsage.
type claimCapacityErrSt struct {
	store.Store
}

func (*claimCapacityErrSt) TryClaimSlots(_ context.Context, _ string, _ []store.UsagePoolClaim, _ time.Time) error {
	return store.ErrUsageAtCapacity
}

func TestTryAssign_ClaimRace_RevertsAndDefers(t *testing.T) {
	inner := fake.New()
	st := &claimCapacityErrSt{Store: inner}
	bus := &recordBus{}
	s := newMetricsScheduler(st, bus, "farm-1")

	poolID := uuid.NewString()
	f := seedAssignFixture(t, inner, func(f *assignFixture) {
		f.step.HostRequirements = &store.StepHostRequirements{UsagePools: []string{"nuke"}}
	})
	if _, err := inner.CreateUsagePool(t.Context(), store.UsagePool{
		ID: poolID, Name: "nuke", MaxConcurrent: 4, // capacity available at check time
	}); err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}

	err := s.tryAssign(t.Context(), f.task)
	if !errors.Is(err, errNoWorkerAvailable) {
		t.Fatalf("tryAssign err = %v, want errNoWorkerAvailable on claim-at-capacity", err)
	}
	// Task must have been reverted to ready by revertTaskToReady.
	got, err := inner.GetTask(t.Context(), f.task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusReady {
		t.Errorf("task status = %q, want ready (reverted after failed claim)", got.Status)
	}
	if len(bus.assignCalls) != 0 {
		t.Errorf("expected no publish after failed claim, got %d", len(bus.assignCalls))
	}
}

// ── publish failure surfaces an error after the store mutation ────────────────

func TestTryAssign_PublishFailure_ReturnsError(t *testing.T) {
	st := fake.New()
	bus := &recordBus{assignErr: errors.New("nats down")}
	s := newMetricsScheduler(st, bus, "farm-1")

	f := seedAssignFixture(t, st, nil)

	err := s.tryAssign(t.Context(), f.task)
	if err == nil {
		t.Fatal("expected error when PublishWorkAssign fails")
	}
	// Task is already marked assigned in the store (recovered later by sweep).
	got, err := st.GetTask(t.Context(), f.task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusAssigned {
		t.Errorf("task status = %q, want assigned even though publish failed", got.Status)
	}
}

// ── buildUsageContext / buildUsageClaims / nextAttemptNumber units ────────────

func TestBuildUsageContext_NoRequirements(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	pools, counts, err := s.buildUsageContext(t.Context(), store.Step{})
	if err != nil {
		t.Fatalf("buildUsageContext: %v", err)
	}
	if len(pools) != 0 || len(counts) != 0 {
		t.Errorf("expected empty maps, got pools=%v counts=%v", pools, counts)
	}
}

func TestBuildUsageContext_WithPool(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	poolID := uuid.NewString()
	if _, err := st.CreateUsagePool(t.Context(), store.UsagePool{ID: poolID, Name: "maya", MaxConcurrent: 2}); err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}
	if err := st.TryClaimSlots(t.Context(), "a1",
		[]store.UsagePoolClaim{{ClaimID: uuid.NewString(), PoolID: poolID, PoolName: "maya", MaxConcurrent: 2}},
		time.Now()); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	step := store.Step{HostRequirements: &store.StepHostRequirements{UsagePools: []string{"maya"}}}
	pools, counts, err := s.buildUsageContext(t.Context(), step)
	if err != nil {
		t.Fatalf("buildUsageContext: %v", err)
	}
	if _, ok := pools["maya"]; !ok {
		t.Error("expected maya pool in context")
	}
	if counts["maya"] != 1 {
		t.Errorf("active count for maya = %d, want 1", counts["maya"])
	}
}

func TestBuildUsageClaims(t *testing.T) {
	pools := map[string]store.UsagePool{
		"maya": {ID: "p1", Name: "maya", MaxConcurrent: 3},
	}
	tests := []struct {
		name      string
		step      store.Step
		wantCount int
	}{
		{"nil host requirements", store.Step{}, 0},
		{
			"unknown pool skipped",
			store.Step{HostRequirements: &store.StepHostRequirements{UsagePools: []string{"ghost"}}},
			0,
		},
		{
			"known pool claimed",
			store.Step{HostRequirements: &store.StepHostRequirements{UsagePools: []string{"maya"}}},
			1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildUsageClaims(tt.step, pools)
			if len(got) != tt.wantCount {
				t.Errorf("claims = %d, want %d", len(got), tt.wantCount)
			}
		})
	}
}

func TestNextAttemptNumber(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	// No prior attempts → 1.
	n, err := s.nextAttemptNumber(t.Context(), "fresh-task")
	if err != nil {
		t.Fatalf("nextAttemptNumber: %v", err)
	}
	if n != 1 {
		t.Errorf("first attempt = %d, want 1", n)
	}

	// One prior attempt #3 → 4.
	now := time.Now()
	if _, err := st.CreateTaskAttempt(t.Context(), store.TaskAttempt{
		ID: uuid.NewString(), TaskID: "task-x", AttemptNumber: 3,
		Status: store.AttemptStatusFailed, StartedAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}
	n, err = s.nextAttemptNumber(t.Context(), "task-x")
	if err != nil {
		t.Fatalf("nextAttemptNumber: %v", err)
	}
	if n != 4 {
		t.Errorf("retry attempt = %d, want 4", n)
	}
}

// ── dispatchBatch pushes ready tasks onto taskCh and refreshes gauges ─────────

func TestDispatchBatch_PushesReadyTasks(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")

	f := seedAssignFixture(t, st, nil)

	// Drain taskCh concurrently so dispatchBatch does not block on the buffer.
	got := make(chan store.Task, 1)
	go func() { got <- <-s.taskCh }()

	s.dispatchBatch(t.Context())

	select {
	case tk := <-got:
		if tk.ID != f.task.ID {
			t.Errorf("dispatched task = %q, want %q", tk.ID, f.task.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatchBatch did not push the ready task")
	}
}

func TestDispatchBatch_NoReadyTasks_NoBlock(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")

	// No tasks seeded — dispatchBatch should refresh gauges and return.
	s.dispatchBatch(t.Context())
}

// ── observeAssignment result labels ───────────────────────────────────────────

func TestObserveAssignment_Labels(_ *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	// Should not panic for any of the three classification branches.
	s.observeAssignment(nil, time.Millisecond)
	s.observeAssignment(errNoWorkerAvailable, time.Millisecond)
	s.observeAssignment(errors.New("boom"), time.Millisecond)
}

// ── gauges run without error against a seeded store ────────────────────────────

func TestRefreshGauges_Smoke(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")
	seedAssignFixture(t, st, nil)

	// Seed a usage pool with an active claim so refreshUsageClaimGauge
	// iterates its pool/count loop body.
	poolID := uuid.NewString()
	if _, err := st.CreateUsagePool(t.Context(), store.UsagePool{ID: poolID, Name: "maya", MaxConcurrent: 2}); err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}
	if err := st.TryClaimSlots(t.Context(), "a1",
		[]store.UsagePoolClaim{{ClaimID: uuid.NewString(), PoolID: poolID, PoolName: "maya", MaxConcurrent: 2}},
		time.Now()); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	// Each refresh helper should complete without panic and read the store.
	s.refreshQueueDepthGauge(t.Context())
	s.refreshIdleWorkerGauge(t.Context())
	s.refreshUsageClaimGauge(t.Context())
	s.refreshWorkerGauge(t.Context())
}
