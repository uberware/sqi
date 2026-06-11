// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for assign.go — item 8c of the test roadmap.
//
// buildAssignPayload is a pure package-level function with no NATS or bus
// dependency. Tests live in package scheduler (white-box) to access the
// unexported function directly.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// minimalJobJSON is a single-step JSON template with no loc:// URIs.
const minimalJobJSON = `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "TestJob",
  "steps": [
    {
      "name": "Render",
      "script": {
        "actions": {
          "onRun": { "command": "render", "args": ["--frame", "1"] }
        }
      }
    }
  ]
}`

// buildFixture constructs the store.Job, store.Step, store.Task, and
// store.Worker needed by buildAssignPayload.
func buildFixture(
	t *testing.T,
	rawTemplate string,
	format store.TemplateFormat,
	stepName string,
) (task store.Task, worker store.Worker, job store.Job, step store.Step) {
	t.Helper()
	now := time.Now()
	job = store.Job{
		ID:             uuid.NewString(),
		FarmID:         "farm-1",
		QueueID:        "queue-1",
		Name:           "TestJob",
		Status:         store.JobStatusRunning,
		RawTemplate:    rawTemplate,
		TemplateFormat: format,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	step = store.Step{
		ID:        uuid.NewString(),
		JobID:     job.ID,
		Name:      stepName,
		StepOrder: 0,
		Status:    store.StepStatusRunning,
		CreatedAt: now,
		UpdatedAt: now,
	}
	task = store.Task{
		ID:        uuid.NewString(),
		JobID:     job.ID,
		StepID:    step.ID,
		Name:      stepName + "[Frame=1]",
		Status:    store.TaskStatusAssigned,
		CreatedAt: now,
		UpdatedAt: now,
	}
	worker = store.Worker{
		ID:              uuid.NewString(),
		FarmID:          "farm-1",
		Hostname:        "render-node-01",
		Status:          store.WorkerStatusOnline,
		ComputeLocation: "onprem",
		RegisteredAt:    now,
		UpdatedAt:       now,
	}
	return task, worker, job, step
}

// ── Minimal template ──────────────────────────────────────────────────────────

func TestBuildAssignPayload_Minimal(t *testing.T) {
	st := fake.New()
	task, worker, job, step := buildFixture(t, minimalJobJSON, store.TemplateFormatJSON, "Render")

	data, err := buildAssignPayload(t.Context(), task, worker, job, step, uuid.NewString(), st)
	if err != nil {
		t.Fatalf("buildAssignPayload: %v", err)
	}

	var msg protocol.AssignMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.OnRun == nil {
		t.Fatal("OnRun must not be nil")
	}
	if msg.OnRun.Command != "render" {
		t.Errorf("OnRun.Command = %q, want render", msg.OnRun.Command)
	}
	if len(msg.OnRun.Args) == 0 || msg.OnRun.Args[0] != "--frame" {
		t.Errorf("OnRun.Args = %v, want [--frame 1]", msg.OnRun.Args)
	}
	if msg.TaskID != task.ID {
		t.Errorf("TaskID = %q, want %q", msg.TaskID, task.ID)
	}
}

// ── Job-level environments appear before step environments ────────────────────

func TestBuildAssignPayload_EnvironmentOrdering(t *testing.T) {
	tmpl := `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "EnvJob",
  "jobEnvironments": [
    {
      "name": "JobEnv",
      "script": { "actions": { "onEnter": { "command": "setup-job" } } }
    }
  ],
  "steps": [
    {
      "name": "Step1",
      "stepEnvironments": [
        {
          "name": "StepEnv",
          "script": { "actions": { "onEnter": { "command": "setup-step" } } }
        }
      ],
      "script": { "actions": { "onRun": { "command": "work" } } }
    }
  ]
}`
	st := fake.New()
	task, worker, job, step := buildFixture(t, tmpl, store.TemplateFormatJSON, "Step1")

	data, err := buildAssignPayload(t.Context(), task, worker, job, step, uuid.NewString(), st)
	if err != nil {
		t.Fatalf("buildAssignPayload: %v", err)
	}

	var msg protocol.AssignMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msg.Environments) != 2 {
		t.Fatalf("expected 2 environments, got %d", len(msg.Environments))
	}
	if msg.Environments[0].Name != "JobEnv" {
		t.Errorf("environments[0].Name = %q, want JobEnv", msg.Environments[0].Name)
	}
	if msg.Environments[1].Name != "StepEnv" {
		t.Errorf("environments[1].Name = %q, want StepEnv", msg.Environments[1].Name)
	}
}

// ── JSON format template is parsed correctly ──────────────────────────────────

func TestBuildAssignPayload_JSONFormat(t *testing.T) {
	st := fake.New()
	task, worker, job, step := buildFixture(t, minimalJobJSON, store.TemplateFormatJSON, "Render")

	data, err := buildAssignPayload(t.Context(), task, worker, job, step, uuid.NewString(), st)
	if err != nil {
		t.Fatalf("buildAssignPayload (JSON format): %v", err)
	}
	var msg protocol.AssignMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.OnRun == nil || msg.OnRun.Command == "" {
		t.Error("expected a non-empty OnRun command")
	}
}

// ── YAML format template ──────────────────────────────────────────────────────

func TestBuildAssignPayload_YAMLFormat(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: YAMLJob
steps:
  - name: Composite
    script:
      actions:
        onRun:
          command: composite
          args: ["--output", "out.exr"]
`
	st := fake.New()
	task, worker, job, step := buildFixture(t, yaml, store.TemplateFormatYAML, "Composite")

	data, err := buildAssignPayload(t.Context(), task, worker, job, step, uuid.NewString(), st)
	if err != nil {
		t.Fatalf("buildAssignPayload (YAML): %v", err)
	}
	var msg protocol.AssignMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.OnRun == nil || msg.OnRun.Command != "composite" {
		t.Errorf("OnRun.Command = %q, want composite", msg.OnRun.Command)
	}
}

// ── Step not found returns an error ──────────────────────────────────────────

func TestBuildAssignPayload_StepNotFound(t *testing.T) {
	st := fake.New()
	task, worker, job, step := buildFixture(t, minimalJobJSON, store.TemplateFormatJSON, "Render")
	step.Name = "NonExistentStep" // doesn't match anything in the template

	_, err := buildAssignPayload(t.Context(), task, worker, job, step, uuid.NewString(), st)
	if err == nil {
		t.Fatal("expected error for step not found in template, got nil")
	}
}

// ── loc:// URI resolved via registered storage location ────────────────────────

func TestBuildAssignPayload_LocURIResolved(t *testing.T) {
	tmpl := `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "LocJob",
  "steps": [
    {
      "name": "Render",
      "script": {
        "actions": {
          "onRun": {
            "command": "render",
            "args": ["loc://nas_shows/scenes/hero.hip"]
          }
        }
      }
    }
  ]
}`
	st := fake.New()
	// Register the storage location so loc:// can be resolved.
	if _, err := st.CreateStorageLocation(t.Context(), store.StorageLocation{
		ID:    uuid.NewString(),
		Name:  "nas_shows",
		Type:  "filesystem",
		Roots: map[string]string{"default": "/mnt/nas/shows"},
	}); err != nil {
		t.Fatalf("CreateStorageLocation: %v", err)
	}

	task, worker, job, step := buildFixture(t, tmpl, store.TemplateFormatJSON, "Render")

	data, err := buildAssignPayload(t.Context(), task, worker, job, step, uuid.NewString(), st)
	if err != nil {
		t.Fatalf("buildAssignPayload: %v", err)
	}

	var msg protocol.AssignMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.OnRun == nil {
		t.Fatal("OnRun nil")
	}
	// The loc:// URI should be resolved to a concrete path.
	if len(msg.OnRun.Args) == 0 {
		t.Fatal("no args in OnRun")
	}
	if msg.OnRun.Args[0] == "loc://nas_shows/scenes/hero.hip" {
		t.Error("loc:// URI was not resolved to a concrete path")
	}
}

// ── Unregistered loc:// URI returns an error ──────────────────────────────────

func TestBuildAssignPayload_UnregisteredLocURI(t *testing.T) {
	tmpl := `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "BadLocJob",
  "steps": [
    {
      "name": "Render",
      "script": {
        "actions": {
          "onRun": {
            "command": "render",
            "args": ["loc://no_such_location/file.hip"]
          }
        }
      }
    }
  ]
}`
	st := fake.New() // no storage locations registered
	task, worker, job, step := buildFixture(t, tmpl, store.TemplateFormatJSON, "Render")

	_, err := buildAssignPayload(t.Context(), task, worker, job, step, uuid.NewString(), st)
	if err == nil {
		t.Fatal("expected error for unregistered loc:// URI, got nil")
	}
}
