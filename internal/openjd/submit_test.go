// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for submit.go — item 7 of the test roadmap.
//
// Submitter.Submit exercises the full parse→validate→expand→persist pipeline.
// All tests use fake.New() as the store so no real database is required.

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// seedSubmitPrereqs inserts a farm and queue into st and returns their IDs.
func seedSubmitPrereqs(t *testing.T, st *fake.Store) (farmID, queueID string) {
	t.Helper()
	ctx := t.Context()

	farm, err := st.CreateFarm(ctx, store.Farm{
		ID:   uuid.NewString(),
		Name: "test-farm-" + uuid.NewString()[:8],
	})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{
		ID:     uuid.NewString(),
		FarmID: farm.ID,
		Name:   "test-queue-" + uuid.NewString()[:8],
	})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	return farm.ID, queue.ID
}

// minimalJSON returns a minimal valid OpenJD template as a JSON string.
// The name must be unique across submissions in the same test.
func minimalJSON(name string) string {
	return `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "` + name + `",
  "steps": [
    {
      "name": "Step1",
      "script": {
        "actions": {
          "onRun": { "command": "echo", "args": ["hello"] }
        }
      }
    }
  ]
}`
}

// ── Successful submission — minimal template ───────────────────────────────────

func TestSubmitter_Submit_Minimal(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	result, err := sub.Submit(t.Context(), minimalJSON("MinimalJob"), store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
		Owner:   "alice",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if result.Job.ID == "" {
		t.Error("job.ID must not be empty")
	}
	if result.Job.FarmID != farmID {
		t.Errorf("job.FarmID = %q, want %q", result.Job.FarmID, farmID)
	}
	if result.Job.Status != store.JobStatusPending {
		t.Errorf("job.Status = %q, want pending", result.Job.Status)
	}
	if len(result.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(result.Steps))
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("expected 1 task (no parameter space), got %d", len(result.Tasks))
	}
}

// ── Priority ≤ 0 falls back to default 50 ────────────────────────────────────

func TestSubmitter_Submit_PriorityDefault(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	result, err := sub.Submit(t.Context(), minimalJSON("PriorityJob"), store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:   farmID,
		QueueID:  queueID,
		Priority: 0, // should be replaced with 50
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if result.Job.Priority != 50 {
		t.Errorf("priority = %d, want 50 (default)", result.Job.Priority)
	}
}

// ── Explicit priority is preserved ───────────────────────────────────────────

func TestSubmitter_Submit_ExplicitPriority(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	result, err := sub.Submit(t.Context(), minimalJSON("PriJob2"), store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:   farmID,
		QueueID:  queueID,
		Priority: 75,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if result.Job.Priority != 75 {
		t.Errorf("priority = %d, want 75", result.Job.Priority)
	}
}

// ── Multiple steps with dependsOn ─────────────────────────────────────────────

func TestSubmitter_Submit_MultistepWithDependencies(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	template := `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "MultiStep",
  "steps": [
    {
      "name": "Render",
      "script": { "actions": { "onRun": { "command": "render" } } }
    },
    {
      "name": "Composite",
      "dependencies": [{ "dependsOn": "Render" }],
      "script": { "actions": { "onRun": { "command": "composite" } } }
    }
  ]
}`
	result, err := sub.Submit(t.Context(), template, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if len(result.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(result.Steps))
	}

	// First step (Render) has no deps → should be ready.
	renderStep := result.Steps[0]
	if renderStep.Status != store.StepStatusReady {
		t.Errorf("Render step status = %q, want ready", renderStep.Status)
	}

	// Second step (Composite) depends on Render → should be pending.
	compositeStep := result.Steps[1]
	if compositeStep.Status != store.StepStatusPending {
		t.Errorf("Composite step status = %q, want pending", compositeStep.Status)
	}

	// Tasks for Composite should also be pending.
	for _, tk := range result.Tasks {
		if tk.StepID == compositeStep.ID && tk.Status != store.TaskStatusPending {
			t.Errorf("Composite task status = %q, want pending", tk.Status)
		}
	}
}

// ── INT parameter space expands to correct task count ─────────────────────────

func TestSubmitter_Submit_IntParameterSpace(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	template := `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "FrameJob",
  "steps": [
    {
      "name": "Render",
      "script": { "actions": { "onRun": { "command": "render" } } },
      "parameterSpace": {
        "taskParameterDefinitions": [
          { "name": "Frame", "type": "INT", "range": ["1","2","3","4","5"] }
        ]
      }
    }
  ]
}`
	result, err := sub.Submit(t.Context(), template, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(result.Tasks) != 5 {
		t.Errorf("expected 5 tasks, got %d", len(result.Tasks))
	}
}

// ── Invalid template (bad YAML) returns SubmitValidationError ─────────────────

func TestSubmitter_Submit_BadYAML(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	_, err := sub.Submit(t.Context(), ":::not yaml:::", store.TemplateFormatYAML, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
	})
	if err == nil {
		t.Fatal("expected error for bad YAML, got nil")
	}
	var ve *openjd.SubmitValidationError
	if !errors.As(err, &ve) {
		t.Errorf("expected SubmitValidationError, got %T: %v", err, err)
	}
}

// ── Validation failure (missing specificationVersion) → SubmitValidationError ─

func TestSubmitter_Submit_ValidationError(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	invalid := `{
  "specificationVersion": "wrong-version",
  "name": "BadJob",
  "steps": [
    { "name": "S1", "script": { "actions": { "onRun": { "command": "x" } } } }
  ]
}`
	_, err := sub.Submit(t.Context(), invalid, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
	})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	var ve *openjd.SubmitValidationError
	if !errors.As(err, &ve) {
		t.Errorf("expected SubmitValidationError, got %T: %v", err, err)
	}
}

// ── Storage location reference to unregistered location → validation error ────

func TestSubmitter_Submit_UnregisteredStorageLocation(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	template := `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "LocJob",
  "steps": [
    {
      "name": "Step1",
      "script": {
        "actions": {
          "onRun": {
            "command": "cp",
            "args": ["loc://nas_shows/input.ma", "output.ma"]
          }
        }
      }
    }
  ]
}`
	_, err := sub.Submit(t.Context(), template, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
	})
	if err == nil {
		t.Fatal("expected error for unregistered storage location, got nil")
	}
	var ve *openjd.SubmitValidationError
	if !errors.As(err, &ve) {
		t.Errorf("expected SubmitValidationError, got %T: %v", err, err)
	}
}

// ── YAML template format is accepted ─────────────────────────────────────────

func TestSubmitter_Submit_YAMLFormat(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	yaml := `
specificationVersion: jobtemplate-2023-09
name: YAMLSubmit
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
          args: ["hello"]
`
	result, err := sub.Submit(t.Context(), yaml, store.TemplateFormatYAML, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
	})
	if err != nil {
		t.Fatalf("Submit YAML: %v", err)
	}
	if result.Job.TemplateFormat != store.TemplateFormatYAML {
		t.Errorf("template_format = %q, want yaml", result.Job.TemplateFormat)
	}
}

// ── Job metadata fields are stored correctly ──────────────────────────────────

func TestSubmitter_Submit_Metadata(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	result, err := sub.Submit(t.Context(), minimalJSON("MetaJob"), store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:    farmID,
		QueueID:   queueID,
		Owner:     "bob",
		Submitter: "svc-account",
		Project:   "my_project",
		Priority:  80,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	j := result.Job
	if j.Owner != "bob" {
		t.Errorf("owner = %q, want bob", j.Owner)
	}
	if j.Submitter != "svc-account" {
		t.Errorf("submitter = %q, want svc-account", j.Submitter)
	}
	if j.Project != "my_project" {
		t.Errorf("project = %q, want my_project", j.Project)
	}
	if j.Priority != 80 {
		t.Errorf("priority = %d, want 80", j.Priority)
	}
	if j.CreatedAt.IsZero() {
		t.Error("created_at must not be zero")
	}
	if j.UpdatedAt.Before(time.Now().Add(-5 * time.Second)) {
		t.Error("updated_at seems too old")
	}
}
