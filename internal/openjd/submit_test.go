// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for submit.go — item 7 of the test roadmap.
//
// Submitter.Submit exercises the full parse→validate→expand→persist pipeline.
// All tests use fake.New() as the store so no real database is required.

import (
	"context"
	"errors"
	"slices"
	"strings"
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
	if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
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
	if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
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
	if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
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

// ── Job-level loc:// ref with no default root → validation error ──────────────

func TestSubmitter_Submit_JobEnvMissingDefaultRoot(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)

	// Location exists but has ONLY a compute-location-specific root, no default.
	if _, err := st.CreateStorageLocation(t.Context(), store.StorageLocation{
		ID:    uuid.NewString(),
		Name:  "job_assets",
		Type:  store.StorageLocationTypeFilesystem,
		Roots: map[string]string{"windows_workers": `Z:\assets`},
	}); err != nil {
		t.Fatalf("CreateStorageLocation: %v", err)
	}

	sub := openjd.NewSubmitter(st)

	// loc:// referenced in a JOB environment variable (job scope, no affinity).
	template := `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "JobEnvLocJob",
  "jobEnvironments": [
    { "name": "Setup", "variables": { "ASSETS": "loc://job_assets/lib" } }
  ],
  "steps": [
    {
      "name": "Step1",
      "script": { "actions": { "onRun": { "command": "echo", "args": ["hi"] } } }
    }
  ]
}`

	_, err := sub.Submit(t.Context(), template, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
	})
	if err == nil {
		t.Fatal("expected error: job-level ref to a location with no default root")
	}
	if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
		t.Errorf("expected SubmitValidationError, got %T: %v", err, err)
	}
}

func TestSubmitter_Submit_JobParamDefaultMissingDefaultRoot(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)

	if _, err := st.CreateStorageLocation(t.Context(), store.StorageLocation{
		ID:    uuid.NewString(),
		Name:  "job_assets",
		Type:  store.StorageLocationTypeFilesystem,
		Roots: map[string]string{"windows_workers": `Z:\assets`},
	}); err != nil {
		t.Fatalf("CreateStorageLocation: %v", err)
	}

	sub := openjd.NewSubmitter(st)

	// loc:// referenced in a JOB PARAMETER default (job scope, no affinity).
	template := `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "JobParamLocJob",
  "parameterDefinitions": [
    { "name": "AssetDir", "type": "STRING", "default": "loc://job_assets/lib" }
  ],
  "steps": [
    {
      "name": "Step1",
      "script": { "actions": { "onRun": { "command": "echo", "args": ["hi"] } } }
    }
  ]
}`

	_, err := sub.Submit(t.Context(), template, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
	})
	if err == nil {
		t.Fatal("expected error: job-param-default ref to a location with no default root")
	}
	if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
		t.Errorf("expected SubmitValidationError, got %T: %v", err, err)
	}
}

func TestSubmitter_Submit_JobEnvWithDefaultRootOK(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)

	if _, err := st.CreateStorageLocation(t.Context(), store.StorageLocation{
		ID:    uuid.NewString(),
		Name:  "job_assets",
		Type:  store.StorageLocationTypeFilesystem,
		Roots: map[string]string{"default": "/mnt/assets"},
	}); err != nil {
		t.Fatalf("CreateStorageLocation: %v", err)
	}

	sub := openjd.NewSubmitter(st)
	template := `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "JobEnvLocOKJob",
  "jobEnvironments": [
    { "name": "Setup", "variables": { "ASSETS": "loc://job_assets/lib" } }
  ],
  "steps": [
    {
      "name": "Step1",
      "script": { "actions": { "onRun": { "command": "echo", "args": ["hi"] } } }
    }
  ]
}`

	if _, err := sub.Submit(t.Context(), template, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
	}); err != nil {
		t.Fatalf("Submit with default root should succeed: %v", err)
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

// ── Parameter binding ─────────────────────────────────────────────────────────

// templateWithParams is a minimal template that declares two job parameters:
// FrameStart (INT, required) and Quality (STRING with default "medium").
const templateWithParams = `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "ParamJob",
  "parameterDefinitions": [
    { "name": "FrameStart", "type": "INT" },
    { "name": "Quality", "type": "STRING", "default": "medium" }
  ],
  "steps": [
    {
      "name": "Render",
      "script": { "actions": { "onRun": { "command": "render" } } }
    }
  ]
}`

// TestSubmitter_Submit_BoundParameters verifies that Submit with valid
// Parameters populates SubmitResult.BoundParameters and applies defaults.
func TestSubmitter_Submit_BoundParameters(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	result, err := sub.Submit(t.Context(), templateWithParams, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
		Parameters: map[string]string{
			"FrameStart": "1",
			// Quality is omitted; its default "medium" should be applied.
		},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if result.BoundParameters == nil {
		t.Fatal("BoundParameters must not be nil")
	}
	if result.BoundParameters["FrameStart"] != "1" {
		t.Errorf("BoundParameters[FrameStart] = %q, want %q", result.BoundParameters["FrameStart"], "1")
	}
	if result.BoundParameters["Quality"] != "medium" {
		t.Errorf("BoundParameters[Quality] = %q, want default %q", result.BoundParameters["Quality"], "medium")
	}
}

// TestSubmitter_Submit_MissingRequiredParam verifies that Submit returns a
// SubmitValidationError when a required parameter has no value and no default.
func TestSubmitter_Submit_MissingRequiredParam(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	_, err := sub.Submit(t.Context(), templateWithParams, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
		// FrameStart is required but not provided.
	})
	if err == nil {
		t.Fatal("expected error for missing required parameter, got nil")
	}
	if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
		t.Errorf("expected SubmitValidationError, got %T: %v", err, err)
	}
}

// TestSubmitter_Submit_InvalidParamValue verifies that Submit returns a
// SubmitValidationError when a provided parameter value fails type validation.
func TestSubmitter_Submit_InvalidParamValue(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	_, err := sub.Submit(t.Context(), templateWithParams, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
		Parameters: map[string]string{
			"FrameStart": "not-an-int", // INT type — must be an integer
		},
	})
	if err == nil {
		t.Fatal("expected error for invalid INT value, got nil")
	}
	if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
		t.Errorf("expected SubmitValidationError, got %T: %v", err, err)
	}
}

// TestSubmitter_Submit_UnknownParam verifies that Submit returns a
// SubmitValidationError when an unknown parameter name is provided.
func TestSubmitter_Submit_UnknownParam(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	_, err := sub.Submit(t.Context(), templateWithParams, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
		Parameters: map[string]string{
			"FrameStart": "1",
			"Unknown":    "value", // not declared in the template
		},
	})
	if err == nil {
		t.Fatal("expected error for unknown parameter, got nil")
	}
	if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
		t.Errorf("expected SubmitValidationError, got %T: %v", err, err)
	}
}

// TestSubmitter_Submit_PersistedParameters verifies that after a successful
// Submit with caller-supplied parameters, result.Job.Parameters holds the
// fully-bound values (including defaults applied for omitted parameters).
func TestSubmitter_Submit_PersistedParameters(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	result, err := sub.Submit(t.Context(), templateWithParams, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
		Parameters: map[string]string{
			"FrameStart": "10",
			// Quality is omitted; default "medium" should be applied.
		},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// The persisted job row must carry the bound values.
	if result.Job.Parameters == nil {
		t.Fatal("result.Job.Parameters must not be nil after submit with params")
	}
	if result.Job.Parameters["FrameStart"] != "10" {
		t.Errorf("Job.Parameters[FrameStart] = %q, want 10", result.Job.Parameters["FrameStart"])
	}
	if result.Job.Parameters["Quality"] != "medium" {
		t.Errorf("Job.Parameters[Quality] = %q, want default medium", result.Job.Parameters["Quality"])
	}

	// Re-fetch from the store and confirm the values survived the round-trip.
	fetched, err := st.GetJob(t.Context(), result.Job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if fetched.Parameters["FrameStart"] != "10" {
		t.Errorf("fetched Job.Parameters[FrameStart] = %q, want 10", fetched.Parameters["FrameStart"])
	}
	if fetched.Parameters["Quality"] != "medium" {
		t.Errorf("fetched Job.Parameters[Quality] = %q, want medium", fetched.Parameters["Quality"])
	}

	// BoundParameters on the result must also match.
	if result.BoundParameters["FrameStart"] != "10" {
		t.Errorf("BoundParameters[FrameStart] = %q, want 10", result.BoundParameters["FrameStart"])
	}
}

// TestSubmitter_Submit_NoParamsNoBinding verifies that a template with no
// parameterDefinitions succeeds with empty Parameters and returns an empty
// (non-nil) BoundParameters map.
func TestSubmitter_Submit_NoParamsNoBinding(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	result, err := sub.Submit(t.Context(), minimalJSON("NoParamJob"), store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if result.BoundParameters == nil {
		t.Error("BoundParameters must not be nil even when template has no parameters")
	}
	if len(result.BoundParameters) != 0 {
		t.Errorf("BoundParameters = %v, want empty map", result.BoundParameters)
	}
}

// ── {{Param.*}} resolved in parameter space before expansion ─────────────────

// TestSubmitter_Submit_ParamInRangeExpr verifies that a step whose INT range
// expression contains {{Param.*}} references is resolved against bound job
// parameters before expansion, producing the correct number of tasks.
func TestSubmitter_Submit_ParamInRangeExpr(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	// StartFrame=1, EndFrame=3 → "1-3" → frames 1, 2, 3 → 3 tasks
	template := `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "FrameRangeParamJob",
  "parameterDefinitions": [
    { "name": "StartFrame", "type": "INT" },
    { "name": "EndFrame",   "type": "INT" }
  ],
  "steps": [
    {
      "name": "Render",
      "script": { "actions": { "onRun": { "command": "render" } } },
      "parameterSpace": {
        "taskParameterDefinitions": [
          { "name": "Frame", "type": "INT", "range": "{{Param.StartFrame}}-{{Param.EndFrame}}" }
        ]
      }
    }
  ]
}`
	result, err := sub.Submit(t.Context(), template, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
		Parameters: map[string]string{
			"StartFrame": "1",
			"EndFrame":   "3",
		},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(result.Tasks) != 3 {
		t.Errorf("expected 3 tasks (frames 1–3), got %d", len(result.Tasks))
	}
	// Verify the task parameters carry the resolved per-frame integer values.
	wantFrames := map[string]bool{"1": true, "2": true, "3": true}
	for _, tk := range result.Tasks {
		frame, ok := tk.Parameters["Frame"]
		if !ok {
			t.Errorf("task %q has no Frame parameter", tk.Name)
			continue
		}
		if !wantFrames[frame] {
			t.Errorf("unexpected Frame value %q in task %q", frame, tk.Name)
		}
		delete(wantFrames, frame)
	}
	for f := range wantFrames {
		t.Errorf("Frame=%q was not produced by any task", f)
	}
}

// TestSubmitter_Submit_ParamInRangeExpr_ExceedsValueLimit verifies that a
// parameterized range cannot smuggle past the per-parameter value limit: the
// limit is skipped at template-validation time because the range still contains
// {{...}}, so it must be re-checked on the RESOLVED space. Here Start=1,End=2000
// resolves to "1-2000" → 2000 values, exceeding maxTaskParamValues (1024).
func TestSubmitter_Submit_ParamInRangeExpr_ExceedsValueLimit(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	template := `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "OverLimitParamJob",
  "parameterDefinitions": [
    { "name": "Start", "type": "INT" },
    { "name": "End",   "type": "INT" }
  ],
  "steps": [
    {
      "name": "Render",
      "script": { "actions": { "onRun": { "command": "render" } } },
      "parameterSpace": {
        "taskParameterDefinitions": [
          { "name": "Frame", "type": "INT", "range": "{{Param.Start}}-{{Param.End}}" }
        ]
      }
    }
  ]
}`
	_, err := sub.Submit(t.Context(), template, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
		Parameters: map[string]string{
			"Start": "1",
			"End":   "2000",
		},
	})
	if err == nil {
		t.Fatal("expected SubmitValidationError for resolved range exceeding the value limit, got nil")
	}
	if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
		t.Errorf("expected SubmitValidationError, got %T: %v", err, err)
	}
}

// TestSubmitter_Submit_ParamInRangeExpr_UnknownParam verifies that a step
// whose range expression references an undeclared job parameter produces a
// SubmitValidationError (HTTP 422-class).
func TestSubmitter_Submit_ParamInRangeExpr_UnknownParam(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	template := `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "MissingParamJob",
  "steps": [
    {
      "name": "Render",
      "script": { "actions": { "onRun": { "command": "render" } } },
      "parameterSpace": {
        "taskParameterDefinitions": [
          { "name": "Frame", "type": "INT", "range": "{{Param.Missing}}-5" }
        ]
      }
    }
  ]
}`
	_, err := sub.Submit(t.Context(), template, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
	})
	if err == nil {
		t.Fatal("expected SubmitValidationError for unknown {{Param.Missing}}, got nil")
	}
	if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
		t.Errorf("expected SubmitValidationError, got %T: %v", err, err)
	}
}

// ── SubmitOptions.Name override ───────────────────────────────────────────────

func TestSubmit_NameOverride(t *testing.T) {
	// Arrange a submitter + farm/queue as the existing submit tests do.
	st := fake.New()
	sub := openjd.NewSubmitter(st)
	farmID, queueID := seedSubmitPrereqs(t, st) // reuse the file's existing helper

	tmpl := `specificationVersion: jobtemplate-2023-09
name: TemplateName
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: echo
          args: ["hi"]`

	// With an explicit Name, the job uses it.
	res, err := sub.Submit(t.Context(), tmpl, store.TemplateFormatYAML, openjd.SubmitOptions{
		FarmID: farmID, QueueID: queueID, Name: "My Job 2026",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Job.Name != "My Job 2026" {
		t.Fatalf("Name = %q, want override", res.Job.Name)
	}

	// Without a Name, it falls back to the template's name.
	res2, err := sub.Submit(t.Context(), tmpl, store.TemplateFormatYAML, openjd.SubmitOptions{
		FarmID: farmID, QueueID: queueID,
	})
	if err != nil {
		t.Fatalf("submit2: %v", err)
	}
	if res2.Job.Name != "TemplateName" {
		t.Fatalf("Name = %q, want template fallback", res2.Job.Name)
	}
}

func TestSubmitter_Submit_ChunkBoundsDerived(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	template := `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "BoundsJob",
  "extensions": ["TASK_CHUNKING", "SQI_CHUNK_BOUNDS"],
  "steps": [
    {
      "name": "Render",
      "script": { "actions": { "onRun": { "command": "render" } } },
      "parameterSpace": {
        "taskParameterDefinitions": [
          { "name": "Frame", "type": "CHUNK[INT]", "range": "1-10",
            "chunks": { "defaultTaskCount": 10, "rangeConstraint": "CONTIGUOUS" } }
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
	if len(result.Tasks) != 1 {
		t.Fatalf("expected 1 task (chunk of 10), got %d", len(result.Tasks))
	}
	got := result.Tasks[0].Parameters
	if got["Frame.Start"] != "1" {
		t.Errorf("Frame.Start = %q, want 1", got["Frame.Start"])
	}
	if got["Frame.End"] != "10" {
		t.Errorf("Frame.End = %q, want 10", got["Frame.End"])
	}
}

// ── Cross-job dependencies (SubmitOptions.DependsOn) ───────────────────────────

// submitFixture bundles a Submitter over a fake store with a seeded farm/queue
// and a reusable minimal template, for tests that only care about job-level
// behavior (not step/task shape).
type submitFixture struct {
	submitter *openjd.Submitter
	store     *fake.Store
	farmID    string
	queueID   string
	template  string
	format    store.TemplateFormat
}

// newSubmitFixture returns a submitFixture backed by a fresh fake store.
func newSubmitFixture(t *testing.T) submitFixture {
	t.Helper()
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	return submitFixture{
		submitter: openjd.NewSubmitter(st),
		store:     st,
		farmID:    farmID,
		queueID:   queueID,
		template:  minimalJSON("DepsJob-" + uuid.NewString()[:8]),
		format:    store.TemplateFormatJSON,
	}
}

func TestSubmit_DependsOn_BlocksWhenUpstreamPending(t *testing.T) {
	ctx := context.Background()
	f := newSubmitFixture(t)

	// Upstream job, left pending.
	up, err := f.submitter.Submit(ctx, f.template, f.format,
		openjd.SubmitOptions{FarmID: f.farmID, QueueID: f.queueID})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.submitter.Submit(ctx, f.template, f.format, openjd.SubmitOptions{
		FarmID: f.farmID, QueueID: f.queueID, DependsOn: []string{up.Job.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Job.Status != store.JobStatusBlocked {
		t.Fatalf("status = %q, want blocked", res.Job.Status)
	}
	for _, tk := range res.Tasks {
		if tk.Status != store.TaskStatusPending {
			t.Fatalf("task %s status = %q, want pending (held)", tk.Name, tk.Status)
		}
	}
	ids, err := f.store.ListJobDependencyIDs(ctx, res.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != up.Job.ID {
		t.Fatalf("edges = %v, want [%s]", ids, up.Job.ID)
	}
}

func TestSubmit_DependsOn_RunsWhenUpstreamAlreadyCompleted(t *testing.T) {
	ctx := context.Background()
	f := newSubmitFixture(t)

	up, err := f.submitter.Submit(ctx, f.template, f.format,
		openjd.SubmitOptions{FarmID: f.farmID, QueueID: f.queueID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateJobStatus(ctx, up.Job.ID, store.JobStatusCompleted); err != nil {
		t.Fatal(err)
	}

	res, err := f.submitter.Submit(ctx, f.template, f.format, openjd.SubmitOptions{
		FarmID: f.farmID, QueueID: f.queueID, DependsOn: []string{up.Job.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Job.Status != store.JobStatusPending {
		t.Fatalf("status = %q, want pending (upstream already done)", res.Job.Status)
	}
}

func TestSubmit_DependsOn_Rejections(t *testing.T) {
	ctx := context.Background()
	f := newSubmitFixture(t)

	// (a) missing upstream
	_, err := f.submitter.Submit(ctx, f.template, f.format, openjd.SubmitOptions{
		FarmID: f.farmID, QueueID: f.queueID, DependsOn: []string{"does-not-exist"},
	})
	var ve *openjd.SubmitValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("missing upstream: got %v, want validation error", err)
	}

	// (b) already-failed upstream
	up, err := f.submitter.Submit(ctx, f.template, f.format,
		openjd.SubmitOptions{FarmID: f.farmID, QueueID: f.queueID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateJobStatus(ctx, up.Job.ID, store.JobStatusFailed); err != nil {
		t.Fatal(err)
	}
	_, err = f.submitter.Submit(ctx, f.template, f.format, openjd.SubmitOptions{
		FarmID: f.farmID, QueueID: f.queueID, DependsOn: []string{up.Job.ID},
	})
	if !errors.As(err, &ve) {
		t.Fatalf("failed upstream: got %v, want validation error", err)
	}
}

// ── Sub-project E2's Task 10: submit-time expression re-check ───────────────
//
// The real, end-to-end proof that Submit's phase-2 wiring works -- driving
// the brief's own division-by-zero template through the public Submit API,
// with the EXPR extension's registry entry temporarily flipped to
// StatusSupported -- lives in submit_exprcheck_test.go's package-openjd
// (white-box) tests, because only that file can reach the unexported
// registry var to perform the flip. A first draft of this file asserted that
// such a test was "impossible today"; a review disproved that by writing it,
// and that claim is withdrawn (see the report's "Fix round 1" section).
//
// SUB-PROJECT H2 MADE THAT TEMPORARY FLIP UNNECESSARY: EXPR is StatusSupported
// in the real registry, so the external-API tests below drive the production
// path with no registry surgery at all.
//
// The two tests below cover what Submit does for an EXPR template and for the
// common EXPR-off case: that the phase-2 re-check with concrete parameters
// surfaces as a SubmitValidationError, and that the same call site is inert --
// no false rejection -- without the extension declared, even when a job
// parameter used in a format string is submitted as "0".

// TestSubmitter_Submit_EXPRTemplateRejectedByPhase2Recheck drives the brief's
// own division-by-zero template through the public Submit API and asserts the
// rejection comes from the submit-time expression re-check with concrete
// parameters -- Param.N bound to "0", which only phase 2 can see.
//
// It asserted the exact opposite until sub-project H2. Under the unflipped
// registry the same template was rejected at /extensions/0 for the
// extension-registration reason, before parameter binding ran at all, and this
// test pinned that message so "a change unblocking EXPR at some later date is
// required to touch (and re-examine) this test." That change is H2, this is
// that re-examination, and the test is strictly stronger for it: it now proves
// the phase-2 wiring end to end through the public API, which is what the
// header above says only a white-box test with a temporary flip could do.
func TestSubmitter_Submit_EXPRTemplateRejectedByPhase2Recheck(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	const tmpl = `
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: ExprTestJob
parameterDefinitions:
- name: N
  type: INT
steps:
- name: Step1
  script:
    actions:
      onRun:
        command: echo
        args:
        - "{{ 10 / Param.N }}"
`
	_, err := sub.Submit(t.Context(), tmpl, store.TemplateFormatYAML, openjd.SubmitOptions{
		FarmID:     farmID,
		QueueID:    queueID,
		Parameters: map[string]string{"N": "0"},
	})
	if err == nil {
		t.Fatal("expected an error for an EXPR template dividing by a zero-valued parameter, got nil")
	}
	if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
		t.Fatalf("expected SubmitValidationError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "division by zero") {
		t.Fatalf("expected the phase-2 division-by-zero rejection; got: %v", err)
	}
	if !strings.Contains(err.Error(), "expression re-check with concrete parameters") {
		t.Fatalf("expected the rejection to come from the submit-time re-check, "+
			"not from phase 1 (which cannot see Param.N's value); got: %v", err)
	}
}

// TestSubmitter_Submit_ParamZeroWithoutEXPRUnaffected confirms the new
// checkExpressionsAtSubmit call site does not change behavior for a template
// that does not declare EXPR: a job parameter referenced in a base-spec
// format string and submitted as "0" -- the same value that trips the
// division-by-zero check under EXPR -- must submit normally, because
// checkTemplateExpressions no-ops without the EXPR extension declared.
func TestSubmitter_Submit_ParamZeroWithoutEXPRUnaffected(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)

	const tmpl = `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "NoExprTestJob",
  "parameterDefinitions": [
    {"name": "N", "type": "INT"}
  ],
  "steps": [
    {
      "name": "Step1",
      "script": {
        "actions": {
          "onRun": {"command": "echo", "args": ["{{Param.N}}"]}
        }
      }
    }
  ]
}`
	result, err := sub.Submit(t.Context(), tmpl, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:     farmID,
		QueueID:    queueID,
		Parameters: map[string]string{"N": "0"},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if result.BoundParameters["N"] != "0" {
		t.Errorf("BoundParameters[N] = %q, want %q", result.BoundParameters["N"], "0")
	}
}

// ── Declared extensions persisted on the job row (migration 00027) ───────────

// TestSubmit_RecordsDeclaredExtensions pins that submission writes the parsed
// extension list onto the job row. This is the only place in the system where
// the list is known EXACTLY: internal/openjd has already decoded the document
// and validated every name against the registry, so a declaration spelled with
// a JSON escape is the same value here as a plain one.
//
// The scheduler reads it per candidate task per lease request to decide whether
// a job needs a worker capable of phase-3 expression evaluation. Before this it
// scanned the raw template for the bytes "EXPR" -- wrong in both directions,
// which is what makes writing the field at submission worth a column.
func TestSubmit_RecordsDeclaredExtensions(t *testing.T) {
	tests := []struct {
		name string
		tmpl string
		want []string
	}{
		{
			name: "no extensions declared is RECORDED, not absent",
			tmpl: minimalJSON("NoExtensions"),
			want: []string{},
		},
		{
			name: "EXPR declared",
			tmpl: `{
  "specificationVersion": "jobtemplate-2023-09",
  "extensions": ["EXPR"],
  "name": "DeclaresEXPR",
  "steps": [
    {"name": "Step1", "script": {"actions": {"onRun": {"command": "echo"}}}}
  ]
}`,
			want: []string{"EXPR"},
		},
		{
			name: "an escaped declaration the raw bytes do not contain",
			tmpl: `{
  "specificationVersion": "jobtemplate-2023-09",
  "extensions": ["\u0045XPR"],
  "name": "EscapedEXPR",
  "steps": [
    {"name": "Step1", "script": {"actions": {"onRun": {"command": "echo"}}}}
  ]
}`,
			want: []string{"EXPR"},
		},
		{
			name: "another extension, and the bytes EXPR elsewhere",
			tmpl: `{
  "specificationVersion": "jobtemplate-2023-09",
  "extensions": ["TASK_CHUNKING"],
  "name": "ChunkingOnly",
  "description": "writes to HOUDINI_EXPR_CACHE",
  "steps": [
    {"name": "Step1", "script": {"actions": {"onRun": {"command": "echo"}}}}
  ]
}`,
			want: []string{"TASK_CHUNKING"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := fake.New()
			farmID, queueID := seedSubmitPrereqs(t, st)
			res, err := openjd.NewSubmitter(st).Submit(
				t.Context(), tc.tmpl, store.TemplateFormatJSON,
				openjd.SubmitOptions{FarmID: farmID, QueueID: queueID, Owner: "alice"},
			)
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}

			stored, err := st.GetJob(t.Context(), res.Job.ID)
			if err != nil {
				t.Fatalf("GetJob: %v", err)
			}
			for label, job := range map[string]store.Job{"SubmitResult": res.Job, "GetJob": stored} {
				if !job.ExtensionsRecorded {
					t.Fatalf("%s: ExtensionsRecorded = false; a submitted job always knows "+
						"what it declared, and 'not recorded' is reserved for rows written "+
						"before the column existed", label)
				}
				if !slices.Equal(job.DeclaredExtensions, tc.want) {
					t.Fatalf("%s: DeclaredExtensions = %#v, want %#v",
						label, job.DeclaredExtensions, tc.want)
				}
			}
		})
	}
}
