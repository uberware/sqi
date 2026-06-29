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
	var ve *openjd.SubmitValidationError
	if !errors.As(err, &ve) {
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
	var ve *openjd.SubmitValidationError
	if !errors.As(err, &ve) {
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
	var ve *openjd.SubmitValidationError
	if !errors.As(err, &ve) {
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
	var ve *openjd.SubmitValidationError
	if !errors.As(err, &ve) {
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
	var ve *openjd.SubmitValidationError
	if !errors.As(err, &ve) {
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
	var ve *openjd.SubmitValidationError
	if !errors.As(err, &ve) {
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
	var ve *openjd.SubmitValidationError
	if !errors.As(err, &ve) {
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
