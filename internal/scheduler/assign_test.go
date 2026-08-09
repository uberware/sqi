// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for assign.go — item 8c of the test roadmap.
//
// buildAssignPayload is a pure package-level function with no NATS or bus
// dependency. Tests live in package scheduler (white-box) to access the
// unexported function directly.

import (
	"encoding/json"
	"slices"
	"strings"
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

// buildFixture constructs the store.Job, store.Step, store.Task,
// store.Worker, and store.Queue needed by buildAssignPayload. The returned
// queue carries no RunAsUser/RunAsGroup (no isolation); tests that need
// isolation override the returned queue's fields directly.
func buildFixture(
	t *testing.T,
	rawTemplate string,
	format store.TemplateFormat,
	stepName string,
) (task store.Task, worker store.Worker, job store.Job, step store.Step, queue store.Queue) {
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
	queue = store.Queue{
		ID:        job.QueueID,
		FarmID:    "farm-1",
		Name:      "queue-1",
		CreatedAt: now,
		UpdatedAt: now,
	}
	return task, worker, job, step, queue
}

// ── Minimal template ──────────────────────────────────────────────────────────

func TestBuildAssignPayload_Minimal(t *testing.T) {
	st := fake.New()
	task, worker, job, step, queue := buildFixture(t, minimalJobJSON, store.TemplateFormatJSON, "Render")

	data, err := buildAssignPayload(t.Context(), task, worker, job, step, queue, uuid.NewString(), st)
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
	task, worker, job, step, queue := buildFixture(t, tmpl, store.TemplateFormatJSON, "Step1")

	data, err := buildAssignPayload(t.Context(), task, worker, job, step, queue, uuid.NewString(), st)
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
	task, worker, job, step, queue := buildFixture(t, minimalJobJSON, store.TemplateFormatJSON, "Render")

	data, err := buildAssignPayload(t.Context(), task, worker, job, step, queue, uuid.NewString(), st)
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
	task, worker, job, step, queue := buildFixture(t, yaml, store.TemplateFormatYAML, "Composite")

	data, err := buildAssignPayload(t.Context(), task, worker, job, step, queue, uuid.NewString(), st)
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
	task, worker, job, step, queue := buildFixture(t, minimalJobJSON, store.TemplateFormatJSON, "Render")
	step.Name = "NonExistentStep" // doesn't match anything in the template

	_, err := buildAssignPayload(t.Context(), task, worker, job, step, queue, uuid.NewString(), st)
	if err == nil {
		t.Fatal("expected error for step not found in template, got nil")
	}
}

// ── EXPR extension: flag, let blocks, and declared parameter types ─────────────

// exprJobYAML declares the EXPR extension and exercises all three additions
// this test file is here to cover: a job-level and task-level parameter of
// each declared type, a step-template let block, a distinct step-script let
// block that references the step-template's binding, and a step-environment
// let block.
const exprJobYAML = `
specificationVersion: jobtemplate-2023-09
name: ExprJob
extensions: [EXPR]
parameterDefinitions:
  - name: Scene
    type: PATH
  - name: Count
    type: INT
    default: "3"
steps:
  - name: S
    let:
      - a = 1
      - b = a + 1
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "1-10"
        - name: Note
          type: STRING
    script:
      let:
        - c = b + 1
      actions:
        onRun:
          command: render
          args: ["--frame", "{{Task.Param.Frame}}"]
    stepEnvironments:
      - name: StepEnv
        script:
          let:
            - d = 1
          actions:
            onEnter:
              command: setup
`

func TestBuildAssignPayload_EXPRFields(t *testing.T) {
	msg := buildAssignForTemplate(t, exprJobYAML, map[string]string{"Scene": "/projects/shot.ma", "Count": "3"})

	if !msg.EXPR {
		t.Error("EXPR = false, want true for a template declaring extensions: [EXPR]")
	}

	// The step-template and step-script let blocks are ordered and distinct.
	wantStepTemplateLet := []string{"a = 1", "b = a + 1"}
	if !slices.Equal(msg.StepTemplateLet, wantStepTemplateLet) {
		t.Errorf("StepTemplateLet = %v, want %v", msg.StepTemplateLet, wantStepTemplateLet)
	}
	wantStepScriptLet := []string{"c = b + 1"}
	if !slices.Equal(msg.StepScriptLet, wantStepScriptLet) {
		t.Errorf("StepScriptLet = %v, want %v", msg.StepScriptLet, wantStepScriptLet)
	}
	if slices.Contains(msg.StepTemplateLet, "c = b + 1") {
		t.Error("step-script binding leaked into StepTemplateLet")
	}

	// The step environment's own let block travels with it, not merged into
	// the step-level blocks.
	if len(msg.Environments) != 1 {
		t.Fatalf("Environments = %+v, want 1 entry", msg.Environments)
	}
	wantEnvLet := []string{"d = 1"}
	if !slices.Equal(msg.Environments[0].Let, wantEnvLet) {
		t.Errorf("Environments[0].Let = %v, want %v", msg.Environments[0].Let, wantEnvLet)
	}

	// This environment came from the step's stepEnvironments, not the
	// job's jobEnvironments, so StepEnvironment must be true -- the bit
	// that lets the worker's EnvSymbols grant Step.Name here.
	if !msg.Environments[0].StepEnvironment {
		t.Error("Environments[0].StepEnvironment = false, want true for a step environment")
	}

	// Declared parameter types, not inferred from value text.
	if got := msg.JobParameterTypes["Scene"]; got != "PATH" {
		t.Errorf("JobParameterTypes[Scene] = %q, want PATH", got)
	}
	if got := msg.JobParameterTypes["Count"]; got != "INT" {
		t.Errorf("JobParameterTypes[Count] = %q, want INT", got)
	}
	if got := msg.ParameterTypes["Frame"]; got != "INT" {
		t.Errorf("ParameterTypes[Frame] = %q, want INT", got)
	}
	if got := msg.ParameterTypes["Note"]; got != "STRING" {
		t.Errorf("ParameterTypes[Note] = %q, want STRING", got)
	}

	// The job's and step's own declared names — distinct from TaskName,
	// which decorates the step name with task-parameter values (see
	// buildTaskName): this step declares task parameters, so TaskName and
	// StepName must differ.
	// buildFixture sets store.Job.Name to "TestJob" regardless of the
	// template's own "name" field — JobName carries the STORE record's
	// name, which is what submission persisted, not the raw template text.
	if msg.JobName != "TestJob" {
		t.Errorf("JobName = %q, want TestJob", msg.JobName)
	}
	if msg.StepName != "S" {
		t.Errorf("StepName = %q, want S", msg.StepName)
	}
	if msg.TaskName == msg.StepName {
		t.Errorf("TaskName (%q) unexpectedly equals bare StepName (%q); this step has task "+
			"parameters so TaskName should be decorated", msg.TaskName, msg.StepName)
	}
}

// TestBuildAssignPayload_BaseSpecWireBytesUnchanged proves, rather than
// assumes, that a base-spec template (no extensions: [EXPR]) produces an
// assignment with none of the nine new EXPR-phase-3 fields anywhere on the
// wire — the requirement that motivates marking every one of them omitempty.
func TestBuildAssignPayload_BaseSpecWireBytesUnchanged(t *testing.T) {
	st := fake.New()
	task, worker, job, step, queue := buildFixture(t, minimalJobJSON, store.TemplateFormatJSON, "Render")

	data, err := buildAssignPayload(t.Context(), task, worker, job, step, queue, uuid.NewString(), st)
	if err != nil {
		t.Fatalf("buildAssignPayload: %v", err)
	}

	forbidden := []string{
		`"expr"`,
		`"step_template_let"`,
		`"step_script_let"`,
		`"parameter_types"`,
		`"job_parameter_types"`,
		`"let"`,
		`"job_name"`,
		`"step_name"`,
		`"step_environment"`,
	}
	s := string(data)
	for _, key := range forbidden {
		if strings.Contains(s, key) {
			t.Errorf("base-spec assignment unexpectedly contains %s: %s", key, s)
		}
	}

	var msg protocol.AssignMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.EXPR {
		t.Error("EXPR = true for a base-spec template")
	}
	if msg.StepTemplateLet != nil || msg.StepScriptLet != nil {
		t.Errorf("StepTemplateLet/StepScriptLet = %v/%v, want nil/nil", msg.StepTemplateLet, msg.StepScriptLet)
	}
	if msg.ParameterTypes != nil || msg.JobParameterTypes != nil {
		t.Errorf("ParameterTypes/JobParameterTypes = %v/%v, want nil/nil", msg.ParameterTypes, msg.JobParameterTypes)
	}
	if msg.JobName != "" || msg.StepName != "" {
		t.Errorf("JobName/StepName = %q/%q, want empty/empty", msg.JobName, msg.StepName)
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

	task, worker, job, step, queue := buildFixture(t, tmpl, store.TemplateFormatJSON, "Render")

	data, err := buildAssignPayload(t.Context(), task, worker, job, step, queue, uuid.NewString(), st)
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

// ── JobParameters carried from persisted job.Parameters ───────────────────────

// templateWithJobParam is a JSON template that declares one job-level
// parameter (Frame) with a default of "1".
const templateWithJobParam = `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "JobParamJob",
  "parameterDefinitions": [
    { "name": "Frame", "type": "INT", "default": "1" }
  ],
  "steps": [
    {
      "name": "Render",
      "script": {
        "actions": {
          "onRun": { "command": "render" }
        }
      }
    }
  ]
}`

// TestBuildAssignPayload_JobParametersFromPersistedValues verifies that when
// job.Parameters is non-empty (persisted at submit), the assignment carries
// those values — not just the template defaults.
func TestBuildAssignPayload_JobParametersFromPersistedValues(t *testing.T) {
	st := fake.New()
	task, worker, job, step, queue := buildFixture(t, templateWithJobParam, store.TemplateFormatJSON, "Render")

	// Simulate a job submitted with a non-default value for Frame.
	job.Parameters = map[string]string{"Frame": "42"}

	data, err := buildAssignPayload(t.Context(), task, worker, job, step, queue, "attempt-1", st)
	if err != nil {
		t.Fatalf("buildAssignPayload: %v", err)
	}

	var msg protocol.AssignMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if msg.JobParameters == nil {
		t.Fatal("JobParameters must not be nil")
	}
	if got := msg.JobParameters["Frame"]; got != "42" {
		t.Errorf("JobParameters[Frame] = %q, want 42 (from persisted job.Parameters)", got)
	}
}

// TestBuildAssignPayload_JobParametersFallbackToDefaults verifies that when
// job.Parameters is empty (pre-migration jobs), the assignment falls back to
// extracting defaults from the template.
func TestBuildAssignPayload_JobParametersFallbackToDefaults(t *testing.T) {
	st := fake.New()
	task, worker, job, step, queue := buildFixture(t, templateWithJobParam, store.TemplateFormatJSON, "Render")

	// job.Parameters is nil — simulate a pre-migration job.
	job.Parameters = nil

	data, err := buildAssignPayload(t.Context(), task, worker, job, step, queue, "attempt-1", st)
	if err != nil {
		t.Fatalf("buildAssignPayload: %v", err)
	}

	var msg protocol.AssignMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if msg.JobParameters == nil {
		t.Fatal("JobParameters must not be nil (fallback to defaults)")
	}
	if got := msg.JobParameters["Frame"]; got != "1" {
		t.Errorf("JobParameters[Frame] = %q, want 1 (template default fallback)", got)
	}
}

// TestBuildAssignPayload_JobParameterLocURIResolved verifies that a loc:// URI
// in a persisted job-parameter value is concretized server-side (so the worker
// never substitutes a raw loc:// URI into a {{Param.*}} expansion), and that the
// resolution does NOT mutate the caller's job.Parameters map.
func TestBuildAssignPayload_JobParameterLocURIResolved(t *testing.T) {
	st := fake.New()
	if _, err := st.CreateStorageLocation(t.Context(), store.StorageLocation{
		ID:    uuid.NewString(),
		Name:  "nas_shows",
		Type:  "filesystem",
		Roots: map[string]string{"default": "/mnt/nas/shows"},
	}); err != nil {
		t.Fatalf("CreateStorageLocation: %v", err)
	}

	task, worker, job, step, queue := buildFixture(t, templateWithJobParam, store.TemplateFormatJSON, "Render")
	const rawURI = "loc://nas_shows/scenes/hero.hip"
	job.Parameters = map[string]string{"Frame": rawURI}

	data, err := buildAssignPayload(t.Context(), task, worker, job, step, queue, "attempt-1", st)
	if err != nil {
		t.Fatalf("buildAssignPayload: %v", err)
	}

	var msg protocol.AssignMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := msg.JobParameters["Frame"]; got == rawURI {
		t.Errorf("JobParameters[Frame] = %q, want it resolved to a concrete path", got)
	}
	// The original job.Parameters map must be untouched (buildJobParameters clones).
	if got := job.Parameters["Frame"]; got != rawURI {
		t.Errorf("job.Parameters[Frame] was mutated to %q; want original %q", got, rawURI)
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
	task, worker, job, step, queue := buildFixture(t, tmpl, store.TemplateFormatJSON, "Render")

	_, err := buildAssignPayload(t.Context(), task, worker, job, step, queue, uuid.NewString(), st)
	if err == nil {
		t.Fatal("expected error for unregistered loc:// URI, got nil")
	}
}

// ── buildPathMap ──────────────────────────────────────────────────────────────

// TestBuildPathMap verifies that buildPathMap populates SourcePath (the shared
// root), DestinationPath (the worker-local root), and SourcePathFormat (the
// WINDOWS-vs-POSIX heuristic) for representative storage locations, and that it
// omits locations needing no translation.
func TestBuildPathMap(t *testing.T) {
	tests := []struct {
		name      string
		locs      []store.StorageLocation
		compute   string
		wantRules []protocol.PathMapRule
	}{
		{
			name: "posix source root",
			locs: []store.StorageLocation{
				{Name: "proj", Roots: map[string]string{
					"default":   "/mnt/shared/proj",
					"cloud_aws": "/local/proj",
				}},
			},
			compute: "cloud_aws",
			wantRules: []protocol.PathMapRule{
				{SourcePathFormat: "POSIX", SourcePath: "/mnt/shared/proj", DestinationPath: "/local/proj"},
			},
		},
		{
			name: "windows drive-letter source root",
			locs: []store.StorageLocation{
				{Name: "proj", Roots: map[string]string{
					"default":   `Z:\shared\proj`,
					"cloud_aws": "/local/proj",
				}},
			},
			compute: "cloud_aws",
			wantRules: []protocol.PathMapRule{
				{SourcePathFormat: "WINDOWS", SourcePath: `Z:\shared\proj`, DestinationPath: "/local/proj"},
			},
		},
		{
			name: "windows backslash UNC source root",
			locs: []store.StorageLocation{
				{Name: "proj", Roots: map[string]string{
					"default":   `\\nas\share\proj`,
					"cloud_aws": "/local/proj",
				}},
			},
			compute: "cloud_aws",
			wantRules: []protocol.PathMapRule{
				{SourcePathFormat: "WINDOWS", SourcePath: `\\nas\share\proj`, DestinationPath: "/local/proj"},
			},
		},
		{
			name: "no compute-specific root — omitted",
			locs: []store.StorageLocation{
				{Name: "proj", Roots: map[string]string{"default": "/mnt/shared/proj"}},
			},
			compute:   "cloud_aws",
			wantRules: nil,
		},
		{
			name: "compute root equals default — omitted",
			locs: []store.StorageLocation{
				{Name: "proj", Roots: map[string]string{
					"default":   "/mnt/shared/proj",
					"cloud_aws": "/mnt/shared/proj",
				}},
			},
			compute:   "cloud_aws",
			wantRules: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildPathMap(tc.locs, tc.compute)
			if len(got) != len(tc.wantRules) {
				t.Fatalf("buildPathMap returned %d rules; want %d (%+v)", len(got), len(tc.wantRules), got)
			}
			for i, w := range tc.wantRules {
				if got[i] != w {
					t.Errorf("rule[%d] = %+v; want %+v", i, got[i], w)
				}
			}
		})
	}
}

// ── Path deliveries + staging manifest ───────────────────────────────────────

// buildAssignForTemplate is a test helper that parses a YAML job template,
// constructs the required store.Job/Step/Task/Worker fixtures (step "S"),
// calls buildAssignPayload, and unmarshals the result into protocol.AssignMsg.
// jobParams is set as job.Parameters; nil is a valid (empty) value.
func buildAssignForTemplate(t *testing.T, templateYAML string, jobParams map[string]string) protocol.AssignMsg {
	t.Helper()
	st := fake.New()
	task, worker, job, step, queue := buildFixture(t, templateYAML, store.TemplateFormatYAML, "S")
	if len(jobParams) > 0 {
		job.Parameters = jobParams
	}
	data, err := buildAssignPayload(t.Context(), task, worker, job, step, queue, uuid.NewString(), st)
	if err != nil {
		t.Fatalf("buildAssignPayload: %v", err)
	}
	var msg protocol.AssignMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return msg
}

func TestBuildAssignPayload_DefaultDeliveriesWhenExtensionAbsent(t *testing.T) {
	msg := buildAssignForTemplate(t, `
specificationVersion: jobtemplate-2023-09
name: T
steps:
  - name: S
    script: { actions: { onRun: { command: echo } } }
`, nil)
	if len(msg.PathDeliveries) != 2 ||
		msg.PathDeliveries[0].Kind != "swap_in_place" ||
		msg.PathDeliveries[1].Kind != "translation_file" {
		t.Fatalf("PathDeliveries = %+v", msg.PathDeliveries)
	}
	if msg.Staging != nil {
		t.Fatalf("Staging = %+v, want nil", msg.Staging)
	}
}

func TestBuildAssignPayload_StagingManifestFromDataFlow(t *testing.T) {
	tmpl := `
specificationVersion: jobtemplate-2023-09
name: T
extensions: [ SQI_PATH_TRANSLATION ]
SQI_PATH_TRANSLATION:
  deliveries: [ stage_locally ]
parameterDefinitions:
  - { name: InScene, type: PATH, dataFlow: IN, objectType: FILE }
  - { name: OutDir, type: PATH, dataFlow: OUT, objectType: DIRECTORY }
  - { name: Note, type: STRING }
steps:
  - name: S
    script: { actions: { onRun: { command: echo } } }
`
	params := map[string]string{"InScene": "/projects/shot.ma", "OutDir": "/projects/out", "Note": "x"}
	msg := buildAssignForTemplate(t, tmpl, params)
	if len(msg.Staging) != 2 {
		t.Fatalf("Staging = %+v, want 2 entries", msg.Staging)
	}
	got := map[string]string{}
	for _, e := range msg.Staging {
		got[e.Path] = e.Direction
	}
	if got["/projects/shot.ma"] != "IN" || got["/projects/out"] != "OUT" {
		t.Fatalf("Staging directions = %+v", got)
	}
}

// TestDetectPathFormat verifies the WINDOWS-vs-POSIX heuristic directly.
func TestDetectPathFormat(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"/mnt/shared/proj", "POSIX"},
		{"s3://bucket/assets", "POSIX"},
		{`C:\proj`, "WINDOWS"},
		{"C:/proj", "WINDOWS"},
		{`\\nas\share`, "WINDOWS"},
		{`relative\path`, "WINDOWS"},
		{"d:/lower/drive", "WINDOWS"},
		{"", "POSIX"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := detectPathFormat(tc.in); got != tc.want {
				t.Errorf("detectPathFormat(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ── Queue run-as-user isolation carried into the assignment ──────────────────

// TestBuildAssignPayload_CarriesQueueIsolation verifies that a queue with
// RunAsUser/RunAsGroup set produces a non-nil AssignMsg.Isolation carrying
// those values.
func TestBuildAssignPayload_CarriesQueueIsolation(t *testing.T) {
	st := fake.New()
	task, worker, job, step, queue := buildFixture(t, minimalJobJSON, store.TemplateFormatJSON, "Render")
	user := "render-svc"
	group := "render-grp"
	queue.RunAsUser = &user
	queue.RunAsGroup = &group

	data, err := buildAssignPayload(t.Context(), task, worker, job, step, queue, uuid.NewString(), st)
	if err != nil {
		t.Fatalf("buildAssignPayload: %v", err)
	}

	var msg protocol.AssignMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Isolation == nil {
		t.Fatal("Isolation = nil, want spec carrying render-svc")
	}
	if msg.Isolation.User != "render-svc" {
		t.Errorf("Isolation.User = %q, want render-svc", msg.Isolation.User)
	}
	if msg.Isolation.Group != "render-grp" {
		t.Errorf("Isolation.Group = %q, want render-grp", msg.Isolation.Group)
	}
}

// TestBuildAssignPayload_OmitsIsolationWhenQueueHasNone verifies that a queue
// with no RunAsUser produces a nil AssignMsg.Isolation — nil means no
// isolation end to end, not an empty struct with a blank username.
func TestBuildAssignPayload_OmitsIsolationWhenQueueHasNone(t *testing.T) {
	st := fake.New()
	task, worker, job, step, queue := buildFixture(t, minimalJobJSON, store.TemplateFormatJSON, "Render")
	// queue.RunAsUser is nil by default from buildFixture.

	data, err := buildAssignPayload(t.Context(), task, worker, job, step, queue, uuid.NewString(), st)
	if err != nil {
		t.Fatalf("buildAssignPayload: %v", err)
	}

	var msg protocol.AssignMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Isolation != nil {
		t.Errorf("Isolation = %+v, want nil for a queue with no run_as_user", msg.Isolation)
	}
}
