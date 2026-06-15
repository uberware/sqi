// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

func TestSmoke(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer s.Close()

	// Test Farm
	farmID := "farm-1"
	farm := store.Farm{
		ID:          farmID,
		Name:        "test-farm",
		Description: "test",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	createdFarm, err := s.CreateFarm(ctx, farm)
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	readFarm, err := s.GetFarm(ctx, farmID)
	if err != nil {
		t.Fatalf("GetFarm: %v", err)
	}
	if readFarm.Name != createdFarm.Name {
		t.Errorf("Farm mismatch: %q != %q", readFarm.Name, createdFarm.Name)
	}

	// Test Queue
	queueID := "queue-1"
	queue := store.Queue{
		ID:       queueID,
		FarmID:   farmID,
		Name:     "test-queue",
		Priority: 50,
		Paused:   false,
	}
	createdQueue, err := s.CreateQueue(ctx, queue)
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	readQueue, err := s.GetQueue(ctx, queueID)
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if readQueue.Name != createdQueue.Name {
		t.Errorf("Queue mismatch: %q != %q", readQueue.Name, createdQueue.Name)
	}

	// Test StorageLocation
	locID := "loc-1"
	loc := store.StorageLocation{
		ID:    locID,
		Name:  "test-loc",
		Type:  store.StorageLocationTypeFilesystem,
		Roots: map[string]string{"default": "/tmp"},
	}
	createdLoc, err := s.CreateStorageLocation(ctx, loc)
	if err != nil {
		t.Fatalf("CreateStorageLocation: %v", err)
	}
	readLoc, err := s.GetStorageLocation(ctx, locID)
	if err != nil {
		t.Fatalf("GetStorageLocation: %v", err)
	}
	if readLoc.Name != createdLoc.Name {
		t.Errorf("Location mismatch: %q != %q", readLoc.Name, createdLoc.Name)
	}

	// Test UsagePool
	poolID := "pool-1"
	pool := store.UsagePool{
		ID:            poolID,
		Name:          "test-pool",
		MaxConcurrent: 10,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	createdPool, err := s.CreateUsagePool(ctx, pool)
	if err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}
	readPool, err := s.GetUsagePool(ctx, poolID)
	if err != nil {
		t.Fatalf("GetUsagePool: %v", err)
	}
	if readPool.Name != createdPool.Name {
		t.Errorf("Pool mismatch: %q != %q", readPool.Name, createdPool.Name)
	}

	// Test Worker
	workerID := "worker-1"
	worker := store.Worker{
		ID:       workerID,
		FarmID:   farmID,
		QueueID:  queueID,
		Hostname: "localhost",
		Status:   store.WorkerStatusOnline,
		Tags:     map[string]string{"tag1": "value1"},
	}
	createdWorker, err := s.RegisterWorker(ctx, worker)
	if err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	readWorker, err := s.GetWorker(ctx, workerID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if readWorker.Hostname != createdWorker.Hostname {
		t.Errorf("Worker mismatch: %q != %q", readWorker.Hostname, createdWorker.Hostname)
	}

	// Test Job
	jobID := "job-1"
	job := store.Job{
		ID:        jobID,
		FarmID:    farmID,
		QueueID:   queueID,
		Name:      "test-job",
		Status:    store.JobStatusPending,
		Priority:  50,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	createdJob, err := s.CreateJob(ctx, job)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	readJob, err := s.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if readJob.Name != createdJob.Name {
		t.Errorf("Job mismatch: %q != %q", readJob.Name, createdJob.Name)
	}

	// Test Step
	stepID := "step-1"
	step := store.Step{
		ID:        stepID,
		JobID:     jobID,
		Name:      "test-step",
		Status:    store.StepStatusPending,
		StepOrder: 0,
		DependsOn: []string{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	createdStep, err := s.CreateStep(ctx, step)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	readStep, err := s.GetStep(ctx, stepID)
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	if readStep.Name != createdStep.Name {
		t.Errorf("Step mismatch: %q != %q", readStep.Name, createdStep.Name)
	}

	// Test Task
	taskID := "task-1"
	task := store.Task{
		ID:         taskID,
		JobID:      jobID,
		StepID:     stepID,
		Name:       "test-task",
		Status:     store.TaskStatusPending,
		Parameters: map[string]string{"param1": "value1"},
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	createdTask, err := s.CreateTask(ctx, task)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	readTask, err := s.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if readTask.Name != createdTask.Name {
		t.Errorf("Task mismatch: %q != %q", readTask.Name, createdTask.Name)
	}

	// Test TaskAttempt
	attemptID := "attempt-1"
	attempt := store.TaskAttempt{
		ID:            attemptID,
		TaskID:        taskID,
		WorkerID:      workerID,
		AttemptNumber: 1,
		Status:        store.AttemptStatusRunning,
		StartedAt:     time.Now(),
		CreatedAt:     time.Now(),
	}
	createdAttempt, err := s.CreateTaskAttempt(ctx, attempt)
	if err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}
	readAttempt, err := s.GetTaskAttempt(ctx, attemptID)
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	if readAttempt.AttemptNumber != createdAttempt.AttemptNumber {
		t.Errorf("TaskAttempt mismatch: %d != %d", readAttempt.AttemptNumber, createdAttempt.AttemptNumber)
	}

	// Test UsageClaim
	claim := store.UsageClaim{
		ID:            "claim-1",
		PoolID:        poolID,
		TaskAttemptID: attemptID,
		ClaimedAt:     time.Now(),
	}
	_, err = s.CreateClaim(ctx, claim)
	if err != nil {
		t.Fatalf("CreateClaim: %v", err)
	}
	activeCount, err := s.ActiveClaimCount(ctx, poolID)
	if err != nil {
		t.Fatalf("ActiveClaimCount: %v", err)
	}
	if activeCount != 1 {
		t.Errorf("Expected 1 active claim, got %d", activeCount)
	}

	// Test AuditEntry
	entryID := "entry-1"
	entry := store.AuditEntry{
		ID:         entryID,
		EntityType: "job",
		EntityID:   jobID,
		Action:     "created",
		CreatedAt:  time.Now(),
		Details:    map[string]any{"key": "value"},
	}
	err = s.AppendAuditEntry(ctx, entry)
	if err != nil {
		t.Fatalf("AppendAuditEntry: %v", err)
	}
	entries, err := s.ListAuditEntries(ctx, "job", jobID)
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("Expected 1 audit entry, got %d", len(entries))
	}

	// Test Close
	err = s.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Test Reset
	s.Reset()
	_, err = s.GetFarm(ctx, farmID)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Expected ErrNotFound after Reset, got %v", err)
	}
}
