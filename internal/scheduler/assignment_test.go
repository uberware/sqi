// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for the assignment helpers shared by the lease path in lease.go:
// createAttemptAndClaimUsage's building blocks (buildUsageContext,
// buildUsageClaims, nextAttemptNumber) and the instrumentation gauges.
//
// These are white-box tests in package scheduler. A fake store (fake.New)
// drives all store-backed paths; a recording bus stub satisfies busClient
// without a real NATS broker. A real metrics.New() is used because the gauge
// helpers touch s.metrics directly.

import (
	"context"
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

// recordBus is a no-op busClient used by the metrics/gauge tests. It satisfies
// the interface without a real NATS broker.
type recordBus struct{}

func (*recordBus) ConsumeWorker(_ context.Context, _ jetstream.MessageHandler) (jetstream.ConsumeContext, error) {
	return nil, nil
}

func (*recordBus) ConsumeTaskStatus(_ context.Context, _ jetstream.MessageHandler) (jetstream.ConsumeContext, error) {
	return nil, nil
}

func (*recordBus) ConsumeTaskLogs(_ context.Context, _ jetstream.MessageHandler) (jetstream.ConsumeContext, error) {
	return nil, nil
}

func (*recordBus) PublishTaskCancel(_ context.Context, _ string, _ []byte) error { return nil }

func (*recordBus) SubscribeWorkerDiag(_ func(subject string, data []byte)) (*nats.Subscription, error) {
	return nil, nil
}

func (*recordBus) SubscribeLease(_ func(string, string, []byte) []byte) (*nats.Subscription, error) {
	return nil, nil
}

// newMetricsScheduler builds a Scheduler with a real metrics registry so the
// gauge helpers do not nil-panic. cfg.FarmID is set to farmID.
func newMetricsScheduler(st store.Store, bus busClient, farmID string) *Scheduler {
	cfg := DefaultConfig()
	cfg.FarmID = farmID
	s := New(cfg, st, bus, metrics.New(), slog.New(slog.DiscardHandler), ws.NoopNotifier{}, nil)
	s.ctx = context.Background()
	return s
}

// assignFixture holds the records seeded for a fixture-backed test.
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
