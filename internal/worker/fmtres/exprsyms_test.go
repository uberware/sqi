// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres_test

// Tests for exprsyms.go — EXPR sub-project E4a, Task 3: the phase-3 symbol
// table.
//
// Design spec §3's table is the contract for TestTaskSymbols_* and
// TestEnvSymbols_*: every row present with the right TYPE (not just
// presence — a Param.Count bound as string when the parameter is declared
// INT is the defect this file exists to catch) and the right concrete
// value. TestPhase2Phase3Agreement is the wave's own real deliverable: it
// runs a real template's declared parameters through internal/openjd's
// real phase-2 checker (openjd.SymbolsFor) and through this package's
// phase-3 builder, and asserts the types agree — proving, rather than
// assuming, that phase 3 is "the same walk with a different table."

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── TaskSymbols: job/task parameters, typed ─────────────────────────────────

func TestTaskSymbols_JobParamTypes(t *testing.T) {
	msg := &protocol.AssignMsg{
		JobParameters: map[string]string{
			"Str":   "hello",
			"Cnt":   "42",
			"Scale": "3.500",
			"Scene": "/projects/shot.ma",
			"Locs":  `["/a","/b"]`, // LIST[PATH] raw text is irrelevant here; only the declared type drives the symbol's TYPE
		},
		JobParameterTypes: map[string]string{
			"Str":   "STRING",
			"Cnt":   "INT",
			"Scale": "FLOAT",
			"Scene": "PATH",
			"Locs":  "LIST[PATH]",
		},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	// Param.Count must be int, not string — the defect this whole task
	// exists to prevent. Confirmed both by TYPE and by exercising the
	// value in arithmetic.
	cnt, ok := syms.Lookup("Param.Cnt")
	if !ok {
		t.Fatal("Param.Cnt not bound")
	}
	if !cnt.Type.Equal(expr.TInt) {
		t.Errorf("Param.Cnt type = %s, want int", cnt.Type)
	}
	if cnt.AsInt() != 42 {
		t.Errorf("Param.Cnt value = %d, want 42", cnt.AsInt())
	}
	v, err := expr.Eval("Param.Cnt + 1", syms, expr.TInt)
	if err != nil {
		t.Fatalf("Param.Cnt + 1: %v", err)
	}
	if v.AsInt() != 43 {
		t.Errorf("Param.Cnt + 1 = %d, want 43 (addition, not concatenation)", v.AsInt())
	}

	str, ok := syms.Lookup("Param.Str")
	if !ok || !str.Type.Equal(expr.TString) || str.AsStr() != "hello" {
		t.Errorf("Param.Str = %+v, want concrete string %q", str, "hello")
	}

	scale, ok := syms.Lookup("Param.Scale")
	if !ok || !scale.Type.Equal(expr.TFloat) {
		t.Fatalf("Param.Scale = %+v, want float", scale)
	}
	// Section 1.3.4: the submitted text is carried, not canonicalised.
	if got := scale.String(); got != "3.500" {
		t.Errorf("Param.Scale.String() = %q, want carried text %q", got, "3.500")
	}

	scene, ok := syms.Lookup("Param.Scene")
	if !ok || !scene.Type.Equal(expr.TPath) {
		t.Errorf("Param.Scene = %+v, want path", scene)
	}
	// RawParam.Scene stays string for a PATH parameter (section 1.2.2).
	rawScene, ok := syms.Lookup("RawParam.Scene")
	if !ok || !rawScene.Type.Equal(expr.TString) {
		t.Errorf("RawParam.Scene = %+v, want string", rawScene)
	}

	// LIST[PATH] is typed correctly (list[path]/list[string] per section
	// 1.2.2) but is one of the codes expr.ValueFromText never makes
	// concrete "by construction" -- lists are sub-project F's job-parameter
	// types and sqi's template model cannot declare one yet, so this stays
	// Unresolved even in phase 3, identically to how phase 2's
	// concreteJobParamValue already treats it (see that function's doc
	// comment). The TYPE is still exactly what section 3's table promises.
	locs, ok := syms.Lookup("Param.Locs")
	if !ok || !locs.IsUnresolved() || len(locs.Type.Params) != 1 || !locs.Type.Params[0].Equal(expr.ListOf(expr.TPath)) {
		t.Errorf("Param.Locs = %+v, want unresolved[list[path]]", locs)
	}
	rawLocs, ok := syms.Lookup("RawParam.Locs")
	if !ok || !rawLocs.IsUnresolved() || len(rawLocs.Type.Params) != 1 || !rawLocs.Type.Params[0].Equal(expr.ListOf(expr.TString)) {
		t.Errorf("RawParam.Locs = %+v, want unresolved[list[string]]", rawLocs)
	}

	// RawParam is NOT uniformly string: for a non-PATH declared type,
	// RawParam shares Param's type exactly (section 1.2.2: "This is the
	// same as RawParam.<ParamName> for all parameter types except PATH").
	rawCnt, ok := syms.Lookup("RawParam.Cnt")
	if !ok || !rawCnt.Type.Equal(expr.TInt) {
		t.Errorf("RawParam.Cnt = %+v, want int (same as Param.Cnt, not string)", rawCnt)
	}
}

func TestTaskSymbols_TaskParamTypes(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters: map[string]string{
			"Frame": "7",
			"Note":  "hello",
			"Out":   "/renders/out.exr",
		},
		ParameterTypes: map[string]string{
			"Frame": "INT",
			"Note":  "STRING",
			"Out":   "PATH",
		},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	frame, ok := syms.Lookup("Task.Param.Frame")
	if !ok || !frame.Type.Equal(expr.TInt) || frame.AsInt() != 7 {
		t.Errorf("Task.Param.Frame = %+v, want concrete int 7", frame)
	}
	// Task.RawParam shares Task.Param's type for every declared type,
	// including PATH (section 1.2.2: the two differ in whether path
	// mapping was applied, not in type).
	rawFrame, ok := syms.Lookup("Task.RawParam.Frame")
	if !ok || !rawFrame.Type.Equal(expr.TInt) || rawFrame.AsInt() != 7 {
		t.Errorf("Task.RawParam.Frame = %+v, want concrete int 7", rawFrame)
	}

	out, ok := syms.Lookup("Task.Param.Out")
	if !ok || !out.Type.Equal(expr.TPath) {
		t.Errorf("Task.Param.Out = %+v, want path", out)
	}
	rawOut, ok := syms.Lookup("Task.RawParam.Out")
	if !ok || !rawOut.Type.Equal(expr.TPath) {
		t.Errorf("Task.RawParam.Out = %+v, want path (not string)", rawOut)
	}
}

func TestTaskSymbols_ChunkIntTaskParam(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Frames": "1-10"},
		ParameterTypes: map[string]string{"Frames": "CHUNK[INT]"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	got, ok := syms.Lookup("Task.Param.Frames")
	if !ok {
		t.Fatal("Task.Param.Frames not bound")
	}
	// CHUNK[INT] is range_expr, not list[int] — a frame range need not be
	// expanded. Unlike BOOL/LIST[*], CHUNK[INT] IS declarable today and
	// phase 3 has the actual value (expand.go stores a chunked task
	// parameter's value as a range-expression string), so it must go
	// CONCRETE — the brief's contract is "every symbol concrete," and an
	// Unresolved Task.Param.Frames here would mean a chunked render's
	// frame range never reaches the command line on a real worker.
	if got.IsUnresolved() || !got.Type.Equal(expr.TRangeExpr) {
		t.Fatalf("Task.Param.Frames = %+v, want concrete range_expr", got)
	}
	if got.AsRangeExpr() != "1-10" {
		t.Errorf("Task.Param.Frames.AsRangeExpr() = %q, want %q", got.AsRangeExpr(), "1-10")
	}
}

// TestTaskSymbols_ChunkIntTaskParam_InvalidRangeFallsBackToUnresolved pins
// the parse-failure fallback for range_expr, matching INT/FLOAT's existing
// rule in expr.ValueFromText.
func TestTaskSymbols_ChunkIntTaskParam_InvalidRangeFallsBackToUnresolved(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Frames": "not-a-range"},
		ParameterTypes: map[string]string{"Frames": "CHUNK[INT]"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	got, ok := syms.Lookup("Task.Param.Frames")
	if !ok {
		t.Fatal("Task.Param.Frames not bound")
	}
	if !got.IsUnresolved() {
		t.Errorf("Task.Param.Frames = %+v, want Unresolved for an unparseable range", got)
	}
}

// ── TaskSymbols: session symbols ────────────────────────────────────────────

func TestTaskSymbols_SessionSymbols_WithPathMapping(t *testing.T) {
	syms, err := fmtres.TaskSymbols(&protocol.AssignMsg{}, "/work/session1", "/work/session1/path_mapping.json", true)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	wd, ok := syms.Lookup("Session.WorkingDirectory")
	if !ok || !wd.Type.Equal(expr.TPath) {
		t.Fatalf("Session.WorkingDirectory = %+v, want path", wd)
	}

	has, ok := syms.Lookup("Session.HasPathMappingRules")
	if !ok || !has.Type.Equal(expr.TBool) || !has.AsBool() {
		t.Errorf("Session.HasPathMappingRules = %+v, want concrete bool true", has)
	}

	rulesFile, ok := syms.Lookup("Session.PathMappingRulesFile")
	if !ok || !rulesFile.Type.Equal(expr.TPath) {
		t.Errorf("Session.PathMappingRulesFile = %+v, want path", rulesFile)
	}
}

func TestTaskSymbols_SessionSymbols_NoPathMapping(t *testing.T) {
	syms, err := fmtres.TaskSymbols(&protocol.AssignMsg{}, "/work/session1", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	has, ok := syms.Lookup("Session.HasPathMappingRules")
	if !ok || !has.Type.Equal(expr.TBool) || has.AsBool() {
		t.Errorf("Session.HasPathMappingRules = %+v, want concrete bool false", has)
	}

	// No rules: the key is absent entirely, so a reference to it fails
	// loudly rather than resolving to an empty path — matching
	// addPathMappingKeys' existing (non-EXPR) behavior.
	if _, ok := syms.Lookup("Session.PathMappingRulesFile"); ok {
		t.Error("Session.PathMappingRulesFile present with hasPathMap=false, want absent")
	}
}

// ── TaskSymbols: identity ───────────────────────────────────────────────────

func TestTaskSymbols_Identity(t *testing.T) {
	msg := &protocol.AssignMsg{JobName: "RenderJob", StepName: "Render"}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	jn, ok := syms.Lookup("Job.Name")
	if !ok || !jn.Type.Equal(expr.TString) || jn.AsStr() != "RenderJob" {
		t.Errorf("Job.Name = %+v, want concrete string RenderJob", jn)
	}
	sn, ok := syms.Lookup("Step.Name")
	if !ok || !sn.Type.Equal(expr.TString) || sn.AsStr() != "Render" {
		t.Errorf("Step.Name = %+v, want concrete string Render", sn)
	}
}

// ── TaskSymbols: Task.File.* ─────────────────────────────────────────────────

func TestTaskSymbols_TaskFile(t *testing.T) {
	msg := &protocol.AssignMsg{
		EmbeddedFiles: []protocol.EmbeddedFile{
			{Name: "Script", Filename: "run.py"},
		},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work/session1", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	f, ok := syms.Lookup("Task.File.Script")
	if !ok || !f.Type.Equal(expr.TPath) {
		t.Fatalf("Task.File.Script = %+v, want path", f)
	}
	want := filepath.Join("/work/session1", "run.py")
	if f.String() != want {
		t.Errorf("Task.File.Script = %q, want %q (fmtres.AddFileVars' own computation)", f.String(), want)
	}
}

func TestTaskSymbols_EmbeddedFileNameError(t *testing.T) {
	msg := &protocol.AssignMsg{
		EmbeddedFiles: []protocol.EmbeddedFile{
			{Name: "Bad", Filename: "../escape"},
		},
	}
	if _, err := fmtres.TaskSymbols(msg, "/work", "", false); err == nil {
		t.Fatal("TaskSymbols with an invalid embedded filename returned no error")
	}
}

// ── TaskSymbols: negative — Env.File.* must not appear ─────────────────────

func TestTaskSymbols_ExcludesEnvFile(t *testing.T) {
	msg := &protocol.AssignMsg{
		EmbeddedFiles: []protocol.EmbeddedFile{{Name: "TaskFile"}},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	for k := range syms {
		if len(k) >= len("Env.File.") && k[:len("Env.File.")] == "Env.File." {
			t.Errorf("TaskSymbols bound %q; the task-action scope must not expose Env.File.*", k)
		}
	}
}

// ── EnvSymbols ───────────────────────────────────────────────────────────────

func TestEnvSymbols_ExposesParamAndEnvFile(t *testing.T) {
	msg := &protocol.AssignMsg{
		JobParameters:     map[string]string{"Scene": "/x.ma"},
		JobParameterTypes: map[string]string{"Scene": "PATH"},
		JobName:           "J",
	}
	env := &protocol.AssignEnvironment{
		Name:          "Env1",
		EmbeddedFiles: []protocol.EmbeddedFile{{Name: "Config"}},
	}
	syms, err := fmtres.EnvSymbols(msg, env, "/work", "", false)
	if err != nil {
		t.Fatalf("EnvSymbols: %v", err)
	}

	scene, ok := syms.Lookup("Param.Scene")
	if !ok || !scene.Type.Equal(expr.TPath) {
		t.Errorf("Param.Scene = %+v, want path", scene)
	}

	f, ok := syms.Lookup("Env.File.Config")
	if !ok || !f.Type.Equal(expr.TPath) {
		t.Fatalf("Env.File.Config = %+v, want path", f)
	}
	want := filepath.Join("/work", "Config")
	if f.String() != want {
		t.Errorf("Env.File.Config = %q, want %q", f.String(), want)
	}

	jn, ok := syms.Lookup("Job.Name")
	if !ok || jn.AsStr() != "J" {
		t.Errorf("Job.Name = %+v, want concrete string J", jn)
	}

	wd, ok := syms.Lookup("Session.WorkingDirectory")
	if !ok || !wd.Type.Equal(expr.TPath) {
		t.Errorf("Session.WorkingDirectory = %+v, want path", wd)
	}
}

// TestEnvSymbols_ExcludesTaskSymbols is the negative that matters:
// environments are session-scoped and entered once, not per task, so
// Task.Param.*/Task.RawParam.*/Task.File.* must not appear (matching
// fmtres.EnvScope's existing, pre-EXPR behavior). This env is a JOB
// environment (StepEnvironment: false, the zero value), so Step.Name must
// also be absent — see TestEnvSymbols_StepEnvironmentGetsStepName for the
// step-environment case, which must see it.
func TestEnvSymbols_ExcludesTaskSymbols(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Frame": "1"},
		ParameterTypes: map[string]string{"Frame": "INT"},
		StepName:       "S",
	}
	env := &protocol.AssignEnvironment{Name: "Env1"}
	syms, err := fmtres.EnvSymbols(msg, env, "/work", "", false)
	if err != nil {
		t.Fatalf("EnvSymbols: %v", err)
	}

	for _, forbidden := range []string{
		"Task.Param.Frame", "Task.RawParam.Frame", "Task.File.Anything", "Step.Name",
	} {
		if _, ok := syms.Lookup(forbidden); ok {
			t.Errorf("EnvSymbols bound %q; a job environment must not expose it", forbidden)
		}
	}
}

// TestEnvSymbols_StepEnvironmentGetsStepName is the positive counterpart:
// a STEP environment (StepEnvironment: true) must see Step.Name, per
// Template Schemas §3.6.2 (ScopeStepEnvironment = ScopeJobEnvironment +
// Step.Name). protocol.AssignEnvironment.StepEnvironment is the wire bit
// that makes this distinction possible at all.
func TestEnvSymbols_StepEnvironmentGetsStepName(t *testing.T) {
	msg := &protocol.AssignMsg{StepName: "Render"}
	env := &protocol.AssignEnvironment{Name: "Env1", StepEnvironment: true}
	syms, err := fmtres.EnvSymbols(msg, env, "/work", "", false)
	if err != nil {
		t.Fatalf("EnvSymbols: %v", err)
	}
	sn, ok := syms.Lookup("Step.Name")
	if !ok || !sn.Type.Equal(expr.TString) || sn.AsStr() != "Render" {
		t.Errorf("Step.Name = %+v, want concrete string Render for a step environment", sn)
	}
}

func TestEnvSymbols_EmbeddedFileNameError(t *testing.T) {
	msg := &protocol.AssignMsg{}
	env := &protocol.AssignEnvironment{
		EmbeddedFiles: []protocol.EmbeddedFile{{Name: "Bad", Filename: "../escape"}},
	}
	if _, err := fmtres.EnvSymbols(msg, env, "/work", "", false); err == nil {
		t.Fatal("EnvSymbols with an invalid embedded filename returned no error")
	}
}

// ── Phase 2 / phase 3 agreement — the task's real deliverable ──────────────

// exprAgreementYAML declares extensions: [EXPR] and exercises one job
// parameter and one task parameter of each declared type the base spec
// defines for that position (section 1.2.2 / 3.4.1), plus a CHUNK[INT] task
// parameter (TASK_CHUNKING).
const exprAgreementYAML = `
specificationVersion: jobtemplate-2023-09
name: AgreementJob
extensions: [EXPR, TASK_CHUNKING]
parameterDefinitions:
  - name: JStr
    type: STRING
  - name: JInt
    type: INT
    default: "1"
  - name: JFloat
    type: FLOAT
    default: "1.5"
  - name: JPath
    type: PATH
    default: "/x"
steps:
  - name: S
    parameterSpace:
      taskParameterDefinitions:
        - name: TStr
          type: STRING
          range: ["a"]
        - name: TInt
          type: INT
          range: "1-10"
        - name: TFloat
          type: FLOAT
          range: [1.0]
        - name: TPath
          type: PATH
          range: ["/y"]
        - name: TChunk
          type: CHUNK[INT]
          range: "1-10"
          chunks:
            defaultTaskCount: 2
    script:
      embeddedFiles:
        - name: Script
          filename: run.py
          data: "print('hi')"
      actions:
        onRun:
          command: render
    stepEnvironments:
      - name: Env1
        script:
          embeddedFiles:
            - name: Config
              data: "cfg"
          actions:
            onEnter:
              command: setup
`

// constraintOf unwraps an Unresolved type down to its constraint, so a
// phase-2 placeholder (Unresolved(T)) and a phase-3 concrete value (T) can
// be compared on the declared type they both ultimately mean. Phase 2 never
// concretizes Task.Param.*/Task.RawParam.* (symbolsFor's own doc comment:
// "per-task, not per-template"), so those families are ALWAYS Unresolved on
// the phase-2 side regardless of params — that is a difference in
// CONCRETENESS between the phases, which is what "phase" means, not a
// disagreement about the declared TYPE this test is checking.
func constraintOf(t expr.Type) expr.Type {
	if t.Code == expr.CodeUnresolved && len(t.Params) == 1 {
		return t.Params[0]
	}
	return t
}

// keyDiff returns the keys present in a but absent from b, sorted. Used both
// directions: keyDiff(phase2, phase3) is a symbol phase 2 grants that the
// worker never binds (the class of bug this whole strengthening exists to
// catch — a hardcoded name list can only check symbols someone remembered
// to list, never notice one that got dropped); keyDiff(phase3, phase2) is
// the opposite mistake, a symbol the worker binds beyond what its scope
// should grant.
func keyDiff(a, b expr.MapSymbols) []string {
	var missing []string
	for k := range a {
		if _, ok := b[k]; !ok {
			missing = append(missing, k)
		}
	}
	slices.Sort(missing)
	return missing
}

// assertKeysAndTypesAgree is shared by both arms of TestPhase2Phase3Agreement:
// it requires phase2 and phase3 to define EXACTLY the same key set (not just
// agree on the keys someone thought to list) and, for every key both define,
// the same constraint type (constraintOf unwraps phase 2's Unresolved
// placeholders — see that function's own doc comment for why that is not
// itself a disagreement).
func assertKeysAndTypesAgree(t *testing.T, label string, phase2, phase3 expr.MapSymbols) {
	t.Helper()
	if missing := keyDiff(phase2, phase3); len(missing) > 0 {
		t.Errorf("%s: phase-2 exposes %v, phase-3 omits them", label, missing)
	}
	if extra := keyDiff(phase3, phase2); len(extra) > 0 {
		t.Errorf("%s: phase-3 exposes %v, phase-2 does not", label, extra)
	}
	for k, p2 := range phase2 {
		p3, ok := phase3[k]
		if !ok {
			continue // already reported by the key-set check above
		}
		p2c, p3c := constraintOf(p2.Type), constraintOf(p3.Type)
		if !p2c.Equal(p3c) {
			t.Errorf("%s: %s: phase2 type %s (constraint %s) disagrees with phase3 type %s (constraint %s)",
				label, k, p2.Type, p2c, p3.Type, p3c)
		}
	}
}

// buildAgreementMsg builds the phase-3 AssignMsg exercised against
// exprAgreementYAML's declared parameters and files, shared by every arm of
// TestPhase2Phase3Agreement.
func buildAgreementMsg(jobParams map[string]string) *protocol.AssignMsg {
	return &protocol.AssignMsg{
		JobName:       "AgreementJob",
		StepName:      "S",
		JobParameters: jobParams,
		JobParameterTypes: map[string]string{
			"JStr": "STRING", "JInt": "INT", "JFloat": "FLOAT", "JPath": "PATH",
		},
		Parameters: map[string]string{
			"TStr": "a", "TInt": "5", "TFloat": "1.0", "TPath": "/y", "TChunk": "1-5",
		},
		ParameterTypes: map[string]string{
			"TStr": "STRING", "TInt": "INT", "TFloat": "FLOAT", "TPath": "PATH", "TChunk": "CHUNK[INT]",
		},
		EmbeddedFiles: []protocol.EmbeddedFile{{Name: "Script", Filename: "run.py"}},
	}
}

func TestPhase2Phase3Agreement(t *testing.T) {
	tmpl, err := openjd.Parse([]byte(exprAgreementYAML), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tmpl.Steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(tmpl.Steps))
	}
	step := &tmpl.Steps[0]
	if len(step.StepEnvironments) != 1 {
		t.Fatalf("want 1 step environment, got %d", len(step.StepEnvironments))
	}
	stepEnv := &step.StepEnvironments[0]

	jobParams := map[string]string{
		"JStr": "hello", "JInt": "3", "JFloat": "2.500", "JPath": "/projects/shot.ma",
	}
	const workDir = "/work"
	const pathMapFile = "/work/path_mapping.json"

	t.Run("TaskSymbols vs ScopeStepScript", func(t *testing.T) {
		// hasPathMap=true on the phase-3 side so Session.PathMappingRulesFile
		// is present on both sides for the key-set comparison —
		// symbolsFor's scopeFixed binds it UNCONDITIONALLY (as a
		// placeholder) regardless of whether any rules actually exist, so
		// with hasPathMap=false this is the ONE deliberate, expected
		// omission (mirroring addPathMappingKeys' absent-when-no-rules
		// rule) — see TestPhase2Phase3Agreement_NoPathMappingOmitsRulesFile
		// for that case pinned on its own, so it cannot be confused with a
		// real regression here.
		phase2 := openjd.SymbolsFor(tmpl, step, nil, openjd.ScopeStepScript, jobParams)
		phase3, err := fmtres.TaskSymbols(buildAgreementMsg(jobParams), workDir, pathMapFile, true)
		if err != nil {
			t.Fatalf("TaskSymbols: %v", err)
		}
		assertKeysAndTypesAgree(t, "ScopeStepScript", phase2, phase3)
	})

	t.Run("EnvSymbols vs ScopeStepEnvironment", func(t *testing.T) {
		phase2 := openjd.SymbolsFor(tmpl, step, stepEnv, openjd.ScopeStepEnvironment, jobParams)
		msg := buildAgreementMsg(jobParams)
		env := &protocol.AssignEnvironment{
			Name:            "Env1",
			StepEnvironment: true,
			EmbeddedFiles:   []protocol.EmbeddedFile{{Name: "Config"}},
		}
		phase3, err := fmtres.EnvSymbols(msg, env, workDir, pathMapFile, true)
		if err != nil {
			t.Fatalf("EnvSymbols: %v", err)
		}
		assertKeysAndTypesAgree(t, "ScopeStepEnvironment", phase2, phase3)
	})
}

// TestPhase2Phase3Agreement_NoPathMappingOmitsRulesFile pins the ONE
// deliberate key-set difference between phase 2 and phase 3 as its own
// test, precisely so it cannot be mistaken for a real regression by the
// key-set assertion above: symbolsFor's scopeFixed binds
// Session.PathMappingRulesFile UNCONDITIONALLY as a placeholder (a
// submission-time position has no way to know yet whether rules will
// exist), while the worker's phase-3 table only binds it when hasPathMap is
// actually true — mirroring addPathMappingKeys' existing (pre-EXPR) rule
// that a reference with no rules present should fail as an unknown symbol
// rather than resolve to an empty path.
func TestPhase2Phase3Agreement_NoPathMappingOmitsRulesFile(t *testing.T) {
	tmpl, err := openjd.Parse([]byte(exprAgreementYAML), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	step := &tmpl.Steps[0]
	jobParams := map[string]string{
		"JStr": "hello", "JInt": "3", "JFloat": "2.500", "JPath": "/projects/shot.ma",
	}

	phase2 := openjd.SymbolsFor(tmpl, step, nil, openjd.ScopeStepScript, jobParams)
	phase3, err := fmtres.TaskSymbols(buildAgreementMsg(jobParams), "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	missing := keyDiff(phase2, phase3)
	want := []string{"Session.PathMappingRulesFile"}
	if !slices.Equal(missing, want) {
		t.Errorf("phase-2 minus phase-3 (hasPathMap=false) = %v, want exactly %v", missing, want)
	}
	if extra := keyDiff(phase3, phase2); len(extra) > 0 {
		t.Errorf("phase-3 exposes %v beyond phase-2, want none", extra)
	}
}
