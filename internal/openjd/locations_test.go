// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for locations.go and deps.go.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// ── ExtractLocRefs ────────────────────────────────────────────────────────────

func TestExtractLocRefs_None(t *testing.T) {
	refs := openjd.ExtractLocRefs("no loc uris here")
	if refs != nil {
		t.Errorf("expected nil, got %v", refs)
	}
}

func TestExtractLocRefs_Single(t *testing.T) {
	refs := openjd.ExtractLocRefs("loc://nas_shows/projects/hero/scene.hip")
	if len(refs) != 1 {
		t.Fatalf("want 1 ref, got %d", len(refs))
	}
	if refs[0].LocationName != "nas_shows" {
		t.Errorf("LocationName = %q, want nas_shows", refs[0].LocationName)
	}
	if refs[0].RelPath != "/projects/hero/scene.hip" {
		t.Errorf("RelPath = %q, want /projects/hero/scene.hip", refs[0].RelPath)
	}
	if refs[0].Raw != "loc://nas_shows/projects/hero/scene.hip" {
		t.Errorf("Raw = %q", refs[0].Raw)
	}
}

func TestExtractLocRefs_BareRoot(t *testing.T) {
	refs := openjd.ExtractLocRefs("loc://nas_shows")
	if len(refs) != 1 {
		t.Fatalf("want 1, got %d", len(refs))
	}
	if refs[0].RelPath != "" {
		t.Errorf("RelPath should be empty for bare root, got %q", refs[0].RelPath)
	}
}

func TestExtractLocRefs_Multiple(t *testing.T) {
	s := "copy loc://src/file.exr to loc://dst/output.exr"
	refs := openjd.ExtractLocRefs(s)
	if len(refs) != 2 {
		t.Fatalf("want 2 refs, got %d", len(refs))
	}
}

func TestExtractLocRefs_Duplicates(t *testing.T) {
	s := "loc://nas/a loc://nas/b"
	refs := openjd.ExtractLocRefs(s)
	if len(refs) != 2 {
		t.Errorf("duplicates are not deduplicated in ExtractLocRefs; got %d", len(refs))
	}
}

// ── ResolveLocURIs ────────────────────────────────────────────────────────────

func TestResolveLocURIs_NoRefs(t *testing.T) {
	result, err := openjd.ResolveLocURIs("plain string", func(_, _ string) (string, error) {
		return "", errors.New("should not be called")
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "plain string" {
		t.Errorf("result = %q, want unchanged", result)
	}
}

func TestResolveLocURIs_Success(t *testing.T) {
	input := "echo loc://nas/renders/frame.exr"
	result, err := openjd.ResolveLocURIs(input, func(_ string, relPath string) (string, error) {
		return "/mnt/nas" + relPath, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "echo /mnt/nas/renders/frame.exr" {
		t.Errorf("result = %q", result)
	}
}

func TestResolveLocURIs_Error(t *testing.T) {
	_, err := openjd.ResolveLocURIs("loc://unknown/path", func(name, _ string) (string, error) {
		return "", fmt.Errorf("no location %q", name)
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ── ExtractTemplateLocRefs ────────────────────────────────────────────────────

func TestExtractTemplateLocRefs_Empty(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	refs := openjd.ExtractTemplateLocRefs(tmpl)
	if refs != nil {
		t.Errorf("expected nil for template with no loc:// refs, got %v", refs)
	}
}

func TestExtractTemplateLocRefs_InCommandArgs(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: render
          args:
            - loc://nas_shows/scene.hip
            - loc://s3_output/renders/
`
	tmpl := mustParse(t, yaml)
	refs := openjd.ExtractTemplateLocRefs(tmpl)
	sort.Strings(refs)
	if len(refs) != 2 {
		t.Fatalf("want 2 refs, got %v", refs)
	}
	if refs[0] != "nas_shows" || refs[1] != "s3_output" {
		t.Errorf("refs = %v", refs)
	}
}

func TestExtractTemplateLocRefs_Deduplicated(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
          args:
            - loc://nas/a
            - loc://nas/b
`
	tmpl := mustParse(t, yaml)
	refs := openjd.ExtractTemplateLocRefs(tmpl)
	if len(refs) != 1 || refs[0] != "nas" {
		t.Errorf("expected deduplication to 1 entry, got %v", refs)
	}
}

// ── ExtractStepLocRefs ────────────────────────────────────────────────────────

func TestExtractStepLocRefs_InEmbeddedFile(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
  - name: Step1
    script:
      embeddedFiles:
        - name: scene_path
          type: TEXT
          data: "loc://nas_shows/scene.hip"
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)
	if len(tmpl.Steps) == 0 {
		t.Fatal("no steps parsed")
	}
	refs := openjd.ExtractStepLocRefs(tmpl.Steps[0])
	if len(refs) != 1 || refs[0] != "nas_shows" {
		t.Errorf("refs = %v, want [nas_shows]", refs)
	}
}

func TestExtractStepLocRefs_NoRefs(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	refs := openjd.ExtractStepLocRefs(tmpl.Steps[0])
	if refs != nil {
		t.Errorf("expected nil, got %v", refs)
	}
}

// ── ResolveRoot ───────────────────────────────────────────────────────────────

func TestResolveRoot_ExactMatch(t *testing.T) {
	roots := map[string]string{
		"london":  "/mnt/london",
		"default": "/mnt/default",
	}
	root, err := openjd.ResolveRoot("nas", roots, "london")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if root != "/mnt/london" {
		t.Errorf("root = %q, want /mnt/london", root)
	}
}

func TestResolveRoot_FallbackToDefault(t *testing.T) {
	roots := map[string]string{"default": "/mnt/default"}
	root, err := openjd.ResolveRoot("nas", roots, "paris")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if root != "/mnt/default" {
		t.Errorf("root = %q, want /mnt/default", root)
	}
}

func TestResolveRoot_EmptyComputeLocation(t *testing.T) {
	roots := map[string]string{"default": "/mnt/default"}
	root, err := openjd.ResolveRoot("nas", roots, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if root != "/mnt/default" {
		t.Errorf("root = %q, want /mnt/default", root)
	}
}

func TestResolveRoot_NoMatch(t *testing.T) {
	roots := map[string]string{"london": "/mnt/london"}
	_, err := openjd.ResolveRoot("nas", roots, "paris")
	if err == nil {
		t.Fatal("expected error for missing root")
	}
}

// ── JoinPath ──────────────────────────────────────────────────────────────────

func TestJoinPath_EmptyRelPath(t *testing.T) {
	result := openjd.JoinPath("/mnt/nas", "")
	if result != "/mnt/nas" {
		t.Errorf("result = %q, want /mnt/nas", result)
	}
}

func TestJoinPath_Filesystem(t *testing.T) {
	result := openjd.JoinPath("/mnt/nas", "/renders/frame.exr")
	if result != "/mnt/nas/renders/frame.exr" {
		t.Errorf("result = %q", result)
	}
}

func TestJoinPath_FilesystemTrailingSlash(t *testing.T) {
	result := openjd.JoinPath("/mnt/nas/", "/renders/frame.exr")
	if result != "/mnt/nas/renders/frame.exr" {
		t.Errorf("result = %q", result)
	}
}

func TestJoinPath_S3(t *testing.T) {
	result := openjd.JoinPath("s3://bucket/prefix", "/renders/frame.exr")
	if result != "s3://bucket/prefix/renders/frame.exr" {
		t.Errorf("result = %q", result)
	}
}

func TestJoinPath_S3TrailingSlash(t *testing.T) {
	result := openjd.JoinPath("s3://bucket/prefix/", "/file.exr")
	if result != "s3://bucket/prefix/file.exr" {
		t.Errorf("result = %q", result)
	}
}

func TestJoinPath_WindowsStyle(t *testing.T) {
	result := openjd.JoinPath(`\\server\share`, "/renders/frame.exr")
	// Windows-style root should use backslash separator.
	if result != `\\server\share\renders\frame.exr` {
		t.Errorf("result = %q", result)
	}
}

// ── ResolveDependencies (deps.go) ─────────────────────────────────────────────

func TestResolveDependencies_NoDeps(t *testing.T) {
	s := fake.New()
	defer s.Close()
	ctx := context.Background()

	// Create a minimal job with one step (no dependencies) in pending state.
	if _, err := s.CreateJob(ctx, store.Job{
		ID: "j1", Name: "j1",
		Status: store.JobStatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step1", JobID: "j1", Name: "Step1",
		Status: store.StepStatusPending,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, store.Task{
		ID: "t1", JobID: "j1", StepID: "step1",
		Status: store.TaskStatusPending,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := openjd.ResolveDependencies(ctx, s, "j1")
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if n != 1 {
		t.Errorf("promoted = %d, want 1", n)
	}

	step, err := s.GetStep(ctx, "step1")
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	if step.Status != store.StepStatusReady {
		t.Errorf("step status = %v, want ready", step.Status)
	}
	task, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != store.TaskStatusReady {
		t.Errorf("task status = %v, want ready", task.Status)
	}
}

func TestResolveDependencies_BlockedStep(t *testing.T) {
	s := fake.New()
	defer s.Close()
	ctx := context.Background()

	if _, err := s.CreateJob(ctx, store.Job{ID: "j1", Name: "j1", Status: store.JobStatusRunning}); err != nil {
		t.Fatal(err)
	}
	// step1 is not completed — step2 depends on it.
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step1", JobID: "j1", Name: "Step1",
		Status: store.StepStatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step2", JobID: "j1", Name: "Step2",
		Status:    store.StepStatusPending,
		DependsOn: []string{"Step1"},
	}); err != nil {
		t.Fatal(err)
	}

	n, err := openjd.ResolveDependencies(ctx, s, "j1")
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if n != 0 {
		t.Errorf("promoted = %d, want 0 (step2 is blocked)", n)
	}
}

func TestResolveDependencies_DepsCompleted(t *testing.T) {
	s := fake.New()
	defer s.Close()
	ctx := context.Background()

	if _, err := s.CreateJob(ctx, store.Job{ID: "j1", Name: "j1", Status: store.JobStatusRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step1", JobID: "j1", Name: "Step1",
		Status: store.StepStatusCompleted,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step2", JobID: "j1", Name: "Step2",
		Status:    store.StepStatusPending,
		DependsOn: []string{"Step1"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, store.Task{
		ID: "t2", JobID: "j1", StepID: "step2",
		Status: store.TaskStatusPending,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := openjd.ResolveDependencies(ctx, s, "j1")
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if n != 1 {
		t.Errorf("promoted = %d, want 1", n)
	}
}

func TestResolveDependencies_SkipsNonPending(t *testing.T) {
	s := fake.New()
	defer s.Close()
	ctx := context.Background()

	if _, err := s.CreateJob(ctx, store.Job{ID: "j1", Name: "j1", Status: store.JobStatusRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step1", JobID: "j1", Name: "Step1",
		Status: store.StepStatusRunning, // already running, not pending
	}); err != nil {
		t.Fatal(err)
	}

	n, err := openjd.ResolveDependencies(ctx, s, "j1")
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if n != 0 {
		t.Errorf("promoted = %d, want 0 (non-pending steps skipped)", n)
	}
}

// ── CancelDependents (deps.go) ────────────────────────────────────────────────

func TestCancelDependents_FailedDep_CancelsPendingDependentAndTasks(t *testing.T) {
	s := fake.New()
	defer s.Close()
	ctx := context.Background()

	if _, err := s.CreateJob(ctx, store.Job{ID: "j1", Name: "j1", Status: store.JobStatusRunning}); err != nil {
		t.Fatal(err)
	}
	// step1 failed; step2 (pending) depends on it and can never run.
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step1", JobID: "j1", Name: "Step1",
		Status: store.StepStatusFailed,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step2", JobID: "j1", Name: "Step2",
		Status:    store.StepStatusPending,
		DependsOn: []string{"Step1"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, store.Task{
		ID: "t2", JobID: "j1", StepID: "step2",
		Status: store.TaskStatusPending,
	}); err != nil {
		t.Fatal(err)
	}

	n, tasks, err := openjd.CancelDependents(ctx, s, "j1")
	if err != nil {
		t.Fatalf("CancelDependents: %v", err)
	}
	if n != 1 {
		t.Errorf("canceled = %d, want 1", n)
	}
	// The canceled pending tasks are returned so the caller can notify clients.
	if len(tasks) != 1 || tasks[0].ID != "t2" {
		t.Errorf("returned tasks = %v, want [t2]", tasks)
	}

	step, err := s.GetStep(ctx, "step2")
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	if step.Status != store.StepStatusCanceled {
		t.Errorf("step2 status = %v, want canceled", step.Status)
	}
	task, err := s.GetTask(ctx, "t2")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != store.TaskStatusCanceled {
		t.Errorf("t2 status = %v, want canceled", task.Status)
	}
}

func TestCancelDependents_CanceledDep_CancelsPendingDependent(t *testing.T) {
	s := fake.New()
	defer s.Close()
	ctx := context.Background()

	if _, err := s.CreateJob(ctx, store.Job{ID: "j1", Name: "j1", Status: store.JobStatusRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step1", JobID: "j1", Name: "Step1",
		Status: store.StepStatusCanceled,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step2", JobID: "j1", Name: "Step2",
		Status:    store.StepStatusPending,
		DependsOn: []string{"Step1"},
	}); err != nil {
		t.Fatal(err)
	}

	n, _, err := openjd.CancelDependents(ctx, s, "j1")
	if err != nil {
		t.Fatalf("CancelDependents: %v", err)
	}
	if n != 1 {
		t.Errorf("canceled = %d, want 1", n)
	}
}

func TestCancelDependents_PartialFailure_CancelsWhenAnyDepFailed(t *testing.T) {
	s := fake.New()
	defer s.Close()
	ctx := context.Background()

	if _, err := s.CreateJob(ctx, store.Job{ID: "j1", Name: "j1", Status: store.JobStatusRunning}); err != nil {
		t.Fatal(err)
	}
	// step3 depends on TWO steps: one completed, one failed. A single failed
	// dependency means step3 can never become ready, so it must be canceled.
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step1", JobID: "j1", Name: "Step1", Status: store.StepStatusCompleted,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step2", JobID: "j1", Name: "Step2", Status: store.StepStatusFailed,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step3", JobID: "j1", Name: "Step3",
		Status: store.StepStatusPending, DependsOn: []string{"Step1", "Step2"},
	}); err != nil {
		t.Fatal(err)
	}

	n, _, err := openjd.CancelDependents(ctx, s, "j1")
	if err != nil {
		t.Fatalf("CancelDependents: %v", err)
	}
	if n != 1 {
		t.Errorf("canceled = %d, want 1", n)
	}
	step, err := s.GetStep(ctx, "step3")
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	if step.Status != store.StepStatusCanceled {
		t.Errorf("step3 status = %v, want canceled", step.Status)
	}
}

func TestCancelDependents_Diamond(t *testing.T) {
	s := fake.New()
	defer s.Close()
	ctx := context.Background()

	if _, err := s.CreateJob(ctx, store.Job{ID: "j1", Name: "j1", Status: store.JobStatusRunning}); err != nil {
		t.Fatal(err)
	}
	// Diamond: A → {B, C} → D. A fails; B and C are direct dependents, D depends
	// on both. One call must cancel B, C and D (D via its now-canceled deps).
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "a", JobID: "j1", Name: "A", Status: store.StepStatusFailed,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "b", JobID: "j1", Name: "B",
		Status: store.StepStatusPending, DependsOn: []string{"A"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "c", JobID: "j1", Name: "C",
		Status: store.StepStatusPending, DependsOn: []string{"A"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "d", JobID: "j1", Name: "D",
		Status: store.StepStatusPending, DependsOn: []string{"B", "C"},
	}); err != nil {
		t.Fatal(err)
	}

	n, _, err := openjd.CancelDependents(ctx, s, "j1")
	if err != nil {
		t.Fatalf("CancelDependents: %v", err)
	}
	if n != 3 {
		t.Errorf("canceled = %d, want 3 (B, C, D)", n)
	}
	for _, id := range []string{"b", "c", "d"} {
		step, err := s.GetStep(ctx, id)
		if err != nil {
			t.Fatalf("GetStep(%s): %v", id, err)
		}
		if step.Status != store.StepStatusCanceled {
			t.Errorf("step %s status = %v, want canceled", id, step.Status)
		}
	}
}

func TestCancelDependents_Idempotent(t *testing.T) {
	s := fake.New()
	defer s.Close()
	ctx := context.Background()

	if _, err := s.CreateJob(ctx, store.Job{ID: "j1", Name: "j1", Status: store.JobStatusRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step1", JobID: "j1", Name: "Step1", Status: store.StepStatusFailed,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step2", JobID: "j1", Name: "Step2",
		Status: store.StepStatusPending, DependsOn: []string{"Step1"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := openjd.CancelDependents(ctx, s, "j1"); err != nil {
		t.Fatalf("CancelDependents (first): %v", err)
	}
	// Second call has nothing left to cancel — must be a no-op.
	n, _, err := openjd.CancelDependents(ctx, s, "j1")
	if err != nil {
		t.Fatalf("CancelDependents (second): %v", err)
	}
	if n != 0 {
		t.Errorf("second call canceled = %d, want 0 (idempotent)", n)
	}
}

// ── ExtractJobLevelLocRefs ────────────────────────────────────────────────────

func TestExtractJobLevelLocRefs_EnvAndParam(t *testing.T) {
	def := "loc://job_assets/setup.json"
	tmpl := &openjd.JobTemplate{
		ParameterDefinitions: []openjd.JobParameter{
			{Name: "Config", Default: &def},
		},
		JobEnvironments: []openjd.Environment{
			{
				Name:      "Setup",
				Variables: map[string]string{"ASSETS": "loc://job_assets/lib"},
			},
		},
		Steps: []openjd.StepTemplate{
			{
				Name: "Step1",
				Script: &openjd.StepScript{
					Actions: openjd.StepActions{
						OnRun: openjd.Action{
							Command: "cp",
							Args:    []string{"loc://step_only/in", "out"},
						},
					},
				},
			},
		},
	}

	got := openjd.ExtractJobLevelLocRefs(tmpl)

	if len(got) != 1 {
		t.Errorf("expected 1 deduplicated ref, got %v", got)
	}

	// job_assets is referenced at job scope; step_only must NOT appear.
	if !slices.Contains(got, "job_assets") {
		t.Errorf("expected job_assets in %v", got)
	}
	if slices.Contains(got, "step_only") {
		t.Errorf("step_only is a step ref and must be excluded; got %v", got)
	}
}

func TestCancelDependents_LeavesHealthyAndNonPendingSteps(t *testing.T) {
	s := fake.New()
	defer s.Close()
	ctx := context.Background()

	if _, err := s.CreateJob(ctx, store.Job{ID: "j1", Name: "j1", Status: store.JobStatusRunning}); err != nil {
		t.Fatal(err)
	}
	// step1 completed (healthy) — its dependent must NOT be canceled.
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step1", JobID: "j1", Name: "Step1",
		Status: store.StepStatusCompleted,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step2", JobID: "j1", Name: "Step2",
		Status: store.StepStatusPending, DependsOn: []string{"Step1"},
	}); err != nil {
		t.Fatal(err)
	}
	// A running step depending on a failed one is not pending — must be left alone.
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step3", JobID: "j1", Name: "Step3", Status: store.StepStatusFailed,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateStep(ctx, store.Step{
		ID: "step4", JobID: "j1", Name: "Step4",
		Status: store.StepStatusRunning, DependsOn: []string{"Step3"},
	}); err != nil {
		t.Fatal(err)
	}

	n, _, err := openjd.CancelDependents(ctx, s, "j1")
	if err != nil {
		t.Fatalf("CancelDependents: %v", err)
	}
	if n != 0 {
		t.Errorf("canceled = %d, want 0", n)
	}
}

// ── ValidLocationName ─────────────────────────────────────────────────────────

func TestValidLocationName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"simple", "nas_shows", true},
		{"with dash and dot", "cloud-aws.us_east", true},
		{"single char", "a", true},
		{"empty", "", false},
		{"space", "nas shows", false},
		{"leading space", " nas", false},
		{"tab", "nas\tshows", false},
		{"newline", "nas\nshows", false},
		{"slash", "nas/shows", false},
		{"double quote", `nas"shows`, false},
		{"single quote", "nas'shows", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := openjd.ValidLocationName(tc.in); got != tc.want {
				t.Errorf("ValidLocationName(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// ── IsS3Root / ValidS3Root / DeriveStorageType ────────────────────────────────

func TestIsS3Root(t *testing.T) {
	cases := []struct {
		root string
		want bool
	}{
		{"s3://bucket", true},
		{"s3://bucket/prefix", true},
		{"/mnt/nas/shows", false},
		{`Z:\shows`, false},
		{"S3://bucket", false}, // uppercase scheme is not S3
		{"s3:/bucket", false},  // single slash
		{"", false},
	}
	for _, c := range cases {
		if got := openjd.IsS3Root(c.root); got != c.want {
			t.Errorf("IsS3Root(%q) = %v, want %v", c.root, got, c.want)
		}
	}
}

func TestValidS3Root(t *testing.T) {
	cases := []struct {
		root string
		want bool
	}{
		{"s3://bucket", true},
		{"s3://bucket/prefix/deep", true},
		{"s3://", false},             // no bucket
		{"s3://bad bucket/x", false}, // whitespace in bucket
		{"s3:/bucket", false},        // single slash → not an s3:// URI
		{"/mnt/nas", false},          // filesystem path is not a valid s3 root
		{"", false},
	}
	for _, c := range cases {
		if got := openjd.ValidS3Root(c.root); got != c.want {
			t.Errorf("ValidS3Root(%q) = %v, want %v", c.root, got, c.want)
		}
	}
}

func TestDeriveStorageType(t *testing.T) {
	cases := []struct {
		name  string
		roots map[string]string
		want  string
	}{
		{"empty", map[string]string{}, "filesystem"},
		{"nil", nil, "filesystem"},
		{"all fs", map[string]string{"default": "/mnt/nas", "win": `Z:\s`}, "filesystem"},
		{"all s3", map[string]string{"default": "s3://b", "aws": "s3://b/p"}, "s3"},
		{"mixed", map[string]string{"default": "/mnt/nas", "aws": "s3://b/p"}, "mixed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := openjd.DeriveStorageType(c.roots); got != c.want {
				t.Errorf("DeriveStorageType = %q, want %q", got, c.want)
			}
		})
	}
}
