// SPDX-License-Identifier: AGPL-3.0-only

package openjd_test

// Tests for locations.go and deps.go.

import (
	"context"
	"errors"
	"fmt"
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
