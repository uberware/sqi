// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres_test

// Tests for exprsyms.go — EXPR sub-project E4a, Task 3 (the phase-3 symbol
// table) and Task 5 (re-evaluating let: bindings against it).
//
// Design spec §3's table is the contract for TestTaskSymbols_* and
// TestEnvSymbols_*: every row present with the right TYPE (not just
// presence — a Param.Count bound as string when the parameter is declared
// INT is the defect this file exists to catch) and the right concrete
// value. TestPhase2Phase3Agreement is Task 3's real deliverable: it runs a
// real template's declared parameters through internal/openjd's real
// phase-2 checker (openjd.SymbolsFor) and through this package's phase-3
// builder, and asserts the types agree — proving, rather than assuming,
// that phase 3 is "the same walk with a different table."
//
// TestApplyTaskLet_* / TestApplyEnvLet_* are Task 5's tests for
// ApplyTaskLet/ApplyEnvLet, and TestPhase2Phase3Agreement_LetBindings is
// Task 5's own agreement test: the same claim TestPhase2Phase3Agreement
// makes for the base symbol table, extended to a let: block's bound names,
// whose types are the natural result of evaluating each binding rather than
// a table row copied from a spec.

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
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

// ── PATH parameters: mapping rules applied at bind time (FIX ROUND 1, Task 4
//    review) ─────────────────────────────────────────────────────────────

// TestTaskSymbols_JobPathParamIsMappedAtBindTime pins section 1.2.2's rule
// that Param.<name> for a PATH-declared job parameter carries the value
// with path mapping rules ALREADY APPLIED, while RawParam.<name> carries
// "the original unmapped value". Earlier revisions of TaskSymbols bound
// BOTH from identical, unmapped raw text -- silently wrong on every
// platform, since Param./Task.Param. were the only way most templates would
// ever reach a path-mapped value (apply_path_mapping() being the sole
// explicit alternative).
func TestTaskSymbols_JobPathParamIsMappedAtBindTime(t *testing.T) {
	msg := &protocol.AssignMsg{
		JobParameters:     map[string]string{"Scene": "/mnt/shared/project/shot.ma"},
		JobParameterTypes: map[string]string{"Scene": "PATH"},
		PathMap: []protocol.PathMapRule{
			{SourcePathFormat: "POSIX", SourcePath: "/mnt/shared", DestinationPath: "/local/cache"},
		},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	param, ok := syms.Lookup("Param.Scene")
	if !ok || !param.Type.Equal(expr.TPath) {
		t.Fatalf("Param.Scene = %+v, want path", param)
	}
	// FIX ROUND 2 (item B): built through the SAME machinery
	// mapPathParamValue (expres.go) uses -- expr.Eval("apply_path_mapping(src)")
	// under expr.PathNative EXPLICITLY -- rather than a hardcoded
	// POSIX-separator literal, which only agreed with production by
	// coincidence on a POSIX host. See expres_test.go's wantPathText.
	wantParam := wantPathText(t, "apply_path_mapping(src)",
		expr.MapSymbols{"src": expr.String("/mnt/shared/project/shot.ma")},
		expr.WithPathMapping(fmtres.ConvertPathMapRules(msg.PathMap)))
	if param.String() != wantParam {
		t.Errorf("Param.Scene = %q, want the MAPPED value %q", param.String(), wantParam)
	}

	// RawParam degrades to string for a job PATH parameter (section 1.2.2),
	// but must still carry the UNMAPPED original text, not the mapped one.
	raw, ok := syms.Lookup("RawParam.Scene")
	if !ok || !raw.Type.Equal(expr.TString) {
		t.Fatalf("RawParam.Scene = %+v, want string", raw)
	}
	if raw.AsStr() != "/mnt/shared/project/shot.ma" {
		t.Errorf("RawParam.Scene = %q, want the ORIGINAL unmapped text %q",
			raw.AsStr(), "/mnt/shared/project/shot.ma")
	}
}

// TestTaskSymbols_TaskPathParamIsMappedAtBindTime is the task-parameter
// counterpart. Unlike the job-parameter pair, Task.RawParam shares
// Task.Param's TYPE (path, not string -- TestTaskSymbols_TaskParamTypes
// above), so this is the one place the two symbols can hold the SAME type
// while differing in exactly one respect: whether mapping was applied.
func TestTaskSymbols_TaskPathParamIsMappedAtBindTime(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Scene": "/mnt/shared/project/shot.ma"},
		ParameterTypes: map[string]string{"Scene": "PATH"},
		PathMap: []protocol.PathMapRule{
			{SourcePathFormat: "POSIX", SourcePath: "/mnt/shared", DestinationPath: "/local/cache"},
		},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	param, ok := syms.Lookup("Task.Param.Scene")
	if !ok || !param.Type.Equal(expr.TPath) {
		t.Fatalf("Task.Param.Scene = %+v, want path", param)
	}
	// FIX ROUND 2 (item B): built through the SAME machinery
	// mapPathParamValue (expres.go) uses, rather than a hardcoded
	// POSIX-separator literal -- see the JobPathParam test above.
	wantParam := wantPathText(t, "apply_path_mapping(src)",
		expr.MapSymbols{"src": expr.String("/mnt/shared/project/shot.ma")},
		expr.WithPathMapping(fmtres.ConvertPathMapRules(msg.PathMap)))
	if param.String() != wantParam {
		t.Errorf("Task.Param.Scene = %q, want the MAPPED value %q", param.String(), wantParam)
	}

	raw, ok := syms.Lookup("Task.RawParam.Scene")
	if !ok || !raw.Type.Equal(expr.TPath) {
		t.Fatalf("Task.RawParam.Scene = %+v, want path (same type as Task.Param, section 1.2.2)", raw)
	}
	// raw is UNMAPPED but still a concrete path Value, so its own rendering
	// is equally flavor-sensitive -- built the same way, with no mapping
	// rules, rather than the original text hardcoded.
	wantRaw := wantPathText(t, "RAW", expr.MapSymbols{"RAW": expr.Path("/mnt/shared/project/shot.ma", expr.PathNative)})
	if raw.String() != wantRaw {
		t.Errorf("Task.RawParam.Scene = %q, want the ORIGINAL unmapped text %q", raw.String(), wantRaw)
	}
}

// TestTaskSymbols_PathParamNoRulesPassesThrough confirms the common case --
// most tasks reference no named storage location -- is unaffected: with no
// PathMap entries, Param.<name> still binds to a concrete path built from
// the raw text, exactly as before this fix round (apply_path_mapping's own
// documented passthrough contract, now also true at bind time).
func TestTaskSymbols_PathParamNoRulesPassesThrough(t *testing.T) {
	msg := &protocol.AssignMsg{
		JobParameters:     map[string]string{"Scene": "/mnt/shared/project/shot.ma"},
		JobParameterTypes: map[string]string{"Scene": "PATH"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	param, ok := syms.Lookup("Param.Scene")
	// FIX ROUND 2 (item B): built through the SAME machinery
	// mapPathParamValue calls with no rules, rather than the original raw
	// text hardcoded -- see the JobPathParam test above.
	wantParam := wantPathText(t, "apply_path_mapping(src)",
		expr.MapSymbols{"src": expr.String("/mnt/shared/project/shot.ma")})
	if !ok || param.String() != wantParam {
		t.Errorf("Param.Scene = %+v, want unmapped passthrough %q", param, wantParam)
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
	// FIX ROUND 2 (item B): AddFileVars' own on-disk-path computation
	// (filepath.Join) is genuinely host-OS-driven and not this package's
	// concern; what IS this package's concern -- and what must be built the
	// same way bindFileSymbols builds it -- is the re-render of that string
	// as an expr path VALUE under expr.PathNative, not a hardcoded
	// POSIX-separator literal. See expres_test.go's wantPathText.
	want := wantPathText(t, "F",
		expr.MapSymbols{"F": expr.Path(filepath.Join("/work/session1", "run.py"), expr.PathNative)})
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
	// FIX ROUND 2 (item B): see TestTaskSymbols_TaskFile's identical fix,
	// above -- built via wantPathText rather than a hardcoded literal.
	want := wantPathText(t, "F", expr.MapSymbols{"F": expr.Path(filepath.Join("/work", "Config"), expr.PathNative)})
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
      let:
        - s = Param.JStr
        - n = Param.JInt + 1
        - f = Param.JFloat
        - p = Param.JPath
        - flag = Param.JInt > 0
        - chunk = Task.Param.TChunk
        - lst = [Param.JInt, 1, 2]
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

// ── ApplyTaskLet / ApplyEnvLet — Task 5 ─────────────────────────────────────

// TestApplyTaskLet_SequentialPropagation pins the core mechanism
// checkLetBindings implements and ApplyTaskLet mirrors: bindings evaluate in
// declaration order, each result inserted so a LATER binding sees it — both
// within the step template's own block and, crucially, across the boundary
// into the step script's block (section 3.6.2 row 1/2's ordering).
func TestApplyTaskLet_SequentialPropagation(t *testing.T) {
	msg := &protocol.AssignMsg{
		StepTemplateLet: []string{"x = 1", "y = x + 1"},
		StepScriptLet:   []string{"z = y + 1"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	if err := fmtres.ApplyTaskLet(msg, syms, nil); err != nil {
		t.Fatalf("ApplyTaskLet: %v", err)
	}

	for name, want := range map[string]int64{"x": 1, "y": 2, "z": 3} {
		v, ok := syms.Lookup(name)
		if !ok {
			t.Errorf("%s not bound", name)
			continue
		}
		if !v.Type.Equal(expr.TInt) || v.AsInt() != want {
			t.Errorf("%s = %+v, want concrete int %d", name, v, want)
		}
	}
}

// TestApplyTaskLet_HostOnlySymbol is the design spec's own headline example
// (§1, "a <StepScript>.let binding over Session.WorkingDirectory produces a
// path a command actually uses"): a host-only symbol phase 1/2 could only
// ever bind as a placeholder is, at phase 3, a REAL concrete value, because
// this function runs against TaskSymbols' table on the worker host itself.
//
// FIX ROUND 1 (Task 5 review): this binding moved from StepTemplateLet to
// StepScriptLet. Section 3.6.2 row 1 evaluates a step TEMPLATE's own let:
// block at ScopeStepTemplate, which grants NO Session.* symbols at all
// (scope.go's scopeFixed) -- only the step SCRIPT's block (row 2,
// ScopeStepScript) may reference Session.WorkingDirectory. The version of
// this test that bound it via StepTemplateLet passed only because
// ApplyTaskLet's first implementation evaluated both blocks against one
// undifferentiated table; see TestApplyTaskLet_StepTemplateLetCannotSeeHostSymbols,
// below, for the negative this move makes room for.
func TestApplyTaskLet_HostOnlySymbol(t *testing.T) {
	msg := &protocol.AssignMsg{
		StepScriptLet: []string{"wd = Session.WorkingDirectory"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work/session1", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	if err := fmtres.ApplyTaskLet(msg, syms, nil); err != nil {
		t.Fatalf("ApplyTaskLet: %v", err)
	}

	wd, ok := syms.Lookup("wd")
	if !ok || !wd.Type.Equal(expr.TPath) {
		t.Fatalf("wd = %+v, want a concrete path", wd)
	}
	want := wantPathText(t, "WD", expr.MapSymbols{"WD": expr.Path("/work/session1", expr.PathNative)})
	if wd.String() != want {
		t.Errorf("wd = %q, want %q (Session.WorkingDirectory's own value)", wd.String(), want)
	}
}

// TestApplyTaskLet_StepTemplateLetCannotSeeHostSymbols is FIX ROUND 1's
// regression test for the review's item 1: a StepTemplateLet binding that
// references a host-only symbol (Session.*) or a task-scoped symbol
// (Task.Param.*) must fail as an unknown symbol, exactly as
// internal/openjd's checkStepExpressions rejects the same reference at
// ScopeStepTemplate server-side -- proven directly against the real checker
// in the review (col 1: unknown symbol "Session.WorkingDirectory" /
// "Task.Param.TChunk" at scope=step template). A StepScriptLet binding
// referencing the SAME symbols must succeed, pinning that the restriction is
// scoped to the template block only, not the whole step.
func TestApplyTaskLet_StepTemplateLetCannotSeeHostSymbols(t *testing.T) {
	for _, ref := range []string{"Session.WorkingDirectory", "Task.Param.Frame"} {
		t.Run(ref, func(t *testing.T) {
			msg := &protocol.AssignMsg{
				Parameters:      map[string]string{"Frame": "1"},
				ParameterTypes:  map[string]string{"Frame": "INT"},
				StepTemplateLet: []string{"x = " + ref},
			}
			syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
			if err != nil {
				t.Fatalf("TaskSymbols: %v", err)
			}
			if err := fmtres.ApplyTaskLet(msg, syms, nil); err == nil {
				t.Fatalf("ApplyTaskLet: want an unknown-symbol error for StepTemplateLet referencing %s, got nil", ref)
			}
			if _, ok := syms.Lookup("x"); ok {
				t.Error(`"x" must not be bound when its own evaluation failed`)
			}
		})
	}

	// The positive: the SAME references, from StepScriptLet, must succeed --
	// this is not a blanket restriction, only ScopeStepTemplate's.
	for _, ref := range []string{"Session.WorkingDirectory", "Task.Param.Frame"} {
		t.Run(ref+"/script", func(t *testing.T) {
			msg := &protocol.AssignMsg{
				Parameters:     map[string]string{"Frame": "1"},
				ParameterTypes: map[string]string{"Frame": "INT"},
				StepScriptLet:  []string{"x = " + ref},
			}
			syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
			if err != nil {
				t.Fatalf("TaskSymbols: %v", err)
			}
			if err := fmtres.ApplyTaskLet(msg, syms, nil); err != nil {
				t.Fatalf("ApplyTaskLet: %v (StepScriptLet referencing %s should succeed)", err, ref)
			}
			if _, ok := syms.Lookup("x"); !ok {
				t.Error(`"x" should be bound`)
			}
		})
	}
}

// TestApplyTaskLet_StepTemplateLetSeesParamAndIdentity is the positive
// complement to the previous test's negative: ScopeStepTemplate is narrow,
// not empty -- Param.<name>, RawParam.<name>, Job.Name and Step.Name are all
// still available to a StepTemplateLet binding (scope.go's scopeFixed/
// scopeFamilies for ScopeStepTemplate), and the bound name is visible to the
// FOLLOWING StepScriptLet block, exactly as section 3.6.2 row 1/2 requires.
func TestApplyTaskLet_StepTemplateLetSeesParamAndIdentity(t *testing.T) {
	msg := &protocol.AssignMsg{
		JobParameters:     map[string]string{"Scale": "2"},
		JobParameterTypes: map[string]string{"Scale": "INT"},
		JobName:           "J",
		StepName:          "S",
		StepTemplateLet:   []string{"doubled = Param.Scale * 2", "label = Job.Name + '/' + Step.Name"},
		StepScriptLet:     []string{"tripled = doubled + Param.Scale"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	if err := fmtres.ApplyTaskLet(msg, syms, nil); err != nil {
		t.Fatalf("ApplyTaskLet: %v", err)
	}

	doubled, ok := syms.Lookup("doubled")
	if !ok || doubled.AsInt() != 4 {
		t.Errorf("doubled = %+v, want concrete int 4", doubled)
	}
	label, ok := syms.Lookup("label")
	if !ok || label.AsStr() != "J/S" {
		t.Errorf("label = %+v, want concrete string %q", label, "J/S")
	}
	tripled, ok := syms.Lookup("tripled")
	if !ok || tripled.AsInt() != 6 {
		t.Errorf("tripled = %+v, want concrete int 6 (doubled(4) + Param.Scale(2))", tripled)
	}
}

// TestApplyTaskLet_ScriptShadowingTemplateRejected pins section 3.6's
// shadowing rule at the specific boundary the wire format exists to keep
// enforceable: the step SCRIPT's block may not rebind a name the step
// TEMPLATE's block already bound. The template's original value must
// survive untouched — the rejected script binding must not silently
// overwrite it.
func TestApplyTaskLet_ScriptShadowingTemplateRejected(t *testing.T) {
	msg := &protocol.AssignMsg{
		StepTemplateLet: []string{"x = 1"},
		StepScriptLet:   []string{"x = 2"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	if err := fmtres.ApplyTaskLet(msg, syms, nil); err == nil {
		t.Fatal("ApplyTaskLet: want an error for the script shadowing the template's binding, got nil")
	}

	x, ok := syms.Lookup("x")
	if !ok || !x.Type.Equal(expr.TInt) || x.AsInt() != 1 {
		t.Errorf("x = %+v, want the TEMPLATE's binding (1) preserved, not overwritten by the rejected script binding", x)
	}
}

// TestApplyTaskLet_WithinBlockShadowingRejected is the same rule's
// same-block case: two bindings in ONE block sharing a name.
func TestApplyTaskLet_WithinBlockShadowingRejected(t *testing.T) {
	msg := &protocol.AssignMsg{
		StepTemplateLet: []string{"x = 1", "x = 2"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	if err := fmtres.ApplyTaskLet(msg, syms, nil); err == nil {
		t.Fatal("ApplyTaskLet: want an error for a same-block shadow, got nil")
	}
	x, ok := syms.Lookup("x")
	if !ok || x.AsInt() != 1 {
		t.Errorf("x = %+v, want the FIRST binding (1) preserved", x)
	}
}

// TestApplyTaskLet_FailureSurfacesWithoutHidingOthers is the brief's own
// requirement: "a binding failure surfacing without taking down the whole
// resolution silently". A binding that fails (here, an unknown symbol) must
// (a) produce a non-nil error rather than being swallowed, (b) not be
// inserted into syms, and (c) not prevent OTHER, independent bindings in the
// same block from evaluating and being bound — mirroring checkLetBindings'
// "evaluation continues past a failure" rule exactly.
func TestApplyTaskLet_FailureSurfacesWithoutHidingOthers(t *testing.T) {
	msg := &protocol.AssignMsg{
		StepTemplateLet: []string{"bad = NoSuchSymbol", "good = 1"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	err = fmtres.ApplyTaskLet(msg, syms, nil)
	if err == nil {
		t.Fatal("ApplyTaskLet: want an error for the unknown-symbol binding, got nil")
	}

	if _, ok := syms.Lookup("bad"); ok {
		t.Error(`"bad" must not be bound after its own evaluation failed`)
	}
	good, ok := syms.Lookup("good")
	if !ok || good.AsInt() != 1 {
		t.Errorf(`good = %+v, want 1 -- one failed binding must not silently stop the rest of the block`, good)
	}
}

// TestApplyTaskLet_CapEnforced pins the 50-binding cap: this task's brief
// calls it out by name as "not optional", because the worker has no
// validateLetElementCounts of its own to report an over-count — the
// evaluator's own truncation IS the only bound on this path. See
// TestApplyTaskLet_CapEnforced_MutationCheck, below, for the mutation-tested
// proof that this assertion is not vacuous.
func TestApplyTaskLet_CapEnforced(t *testing.T) {
	syms, err := fmtres.TaskSymbols(&protocol.AssignMsg{}, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	msg := &protocol.AssignMsg{StepTemplateLet: makeLetSequence(51)}
	if err := fmtres.ApplyTaskLet(msg, syms, nil); err != nil {
		t.Fatalf("ApplyTaskLet: %v", err)
	}

	if _, ok := syms.Lookup("a49"); !ok {
		t.Error(`"a49" (the 50th binding) should be bound`)
	}
	if _, ok := syms.Lookup("a50"); ok {
		t.Error(`"a50" (the 51st binding) should NOT be bound -- the 50-binding cap was not enforced`)
	}
}

// makeLetSequence builds n independent, non-referencing let bindings
// "a0 = 0", "a1 = 1", ... "a<n-1> = <n-1>", used by the cap test above.
func makeLetSequence(n int) []string {
	lets := make([]string, n)
	for i := range lets {
		lets[i] = fmt.Sprintf("a%d = %d", i, i)
	}
	return lets
}

// TestApplyEnvLet_BasicAndNil covers ApplyEnvLet's own wiring: a real
// environment binding is evaluated against EnvSymbols' table (proving it is
// not simply an alias for ApplyTaskLet against the wrong field), and a nil
// env is a safe no-op, matching EnvSymbols' own nil handling.
func TestApplyEnvLet_BasicAndNil(t *testing.T) {
	env := &protocol.AssignEnvironment{
		Name: "Env1",
		Let:  []string{"cfg = Env.File.Config"},
		EmbeddedFiles: []protocol.EmbeddedFile{
			{Name: "Config"},
		},
	}
	msg := &protocol.AssignMsg{}
	syms, err := fmtres.EnvSymbols(msg, env, "/work/session1", "", false)
	if err != nil {
		t.Fatalf("EnvSymbols: %v", err)
	}
	if err := fmtres.ApplyEnvLet(env, syms, nil); err != nil {
		t.Fatalf("ApplyEnvLet: %v", err)
	}
	cfg, ok := syms.Lookup("cfg")
	if !ok || !cfg.Type.Equal(expr.TPath) {
		t.Fatalf("cfg = %+v, want a concrete path (Env.File.Config's own value)", cfg)
	}
	envFile, ok := syms.Lookup("Env.File.Config")
	if !ok || cfg.String() != envFile.String() {
		t.Errorf("cfg = %q, want it to equal Env.File.Config = %q", cfg.String(), envFile.String())
	}

	if err := fmtres.ApplyEnvLet(nil, expr.MapSymbols{}, nil); err != nil {
		t.Errorf("ApplyEnvLet(nil, ...) = %v, want nil (a no-op)", err)
	}
}

// TestApplyEnvLet_ShadowRejected is TestApplyTaskLet_WithinBlockShadowingRejected's
// environment counterpart.
func TestApplyEnvLet_ShadowRejected(t *testing.T) {
	env := &protocol.AssignEnvironment{Let: []string{"x = 1", "x = 2"}}
	syms, err := fmtres.EnvSymbols(&protocol.AssignMsg{}, env, "/work", "", false)
	if err != nil {
		t.Fatalf("EnvSymbols: %v", err)
	}
	if err := fmtres.ApplyEnvLet(env, syms, nil); err == nil {
		t.Fatal("ApplyEnvLet: want an error for a same-block shadow, got nil")
	}
	x, ok := syms.Lookup("x")
	if !ok || x.AsInt() != 1 {
		t.Errorf("x = %+v, want the FIRST binding (1) preserved", x)
	}
}

// ── ApplyTaskLet/ApplyEnvLet cap: mutation check ────────────────────────────
//
// TestApplyTaskLet_CapEnforced_MutationCheck exists so the cap assertion
// above is not merely "a test that happens to pass" — it independently
// proves the SAME scenario would FAIL without the guard, by exercising
// exactly what the guard bounds (the truncation itself) rather than
// re-deriving it. This is not a substitute for the hands-on mutation the
// brief requires (commenting out evalLetBindings' own truncation and
// re-running go test) — that was done manually and is recorded in the task
// report — but it keeps a permanent, automated version of the same
// assertion in the suite: with 51 independent bindings, exactly 50 must be
// bound, never 51.
func TestApplyTaskLet_CapEnforced_MutationCheck(t *testing.T) {
	syms, err := fmtres.TaskSymbols(&protocol.AssignMsg{}, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	msg := &protocol.AssignMsg{StepTemplateLet: makeLetSequence(51)}
	if err := fmtres.ApplyTaskLet(msg, syms, nil); err != nil {
		t.Fatalf("ApplyTaskLet: %v", err)
	}

	bound := 0
	for i := range 51 {
		if _, ok := syms.Lookup(fmt.Sprintf("a%d", i)); ok {
			bound++
		}
	}
	if bound != 50 {
		t.Errorf("bound %d of 51 let bindings, want exactly 50 (the cap)", bound)
	}
}

// ── Phase 2 / phase 3 agreement for let bindings — Task 5's real deliverable ─

// letNameAndSrc is a minimal, test-only "name = expression" split, used only
// to derive the two halves of one of exprAgreementYAML's OWN let: strings so
// this test can replay it against the phase-2 table. It deliberately does
// NOT reach into fmtres' unexported splitLetBinding (this is an external
// _test package) and does not need to: the strings below are authored by
// this test file, not user input, so a bare strings.Cut is enough.
func letNameAndSrc(t *testing.T, raw string) (name, src string) {
	t.Helper()
	before, after, ok := strings.Cut(raw, "=")
	if !ok {
		t.Fatalf("malformed let binding in test fixture: %q", raw)
	}
	return strings.TrimSpace(before), strings.TrimSpace(after)
}

// TestPhase2Phase3Agreement_LetBindings is this task's own version of
// TestPhase2Phase3Agreement's claim, extended to a let: block's bound
// names: section 3.6.1 says "the type of the binding is the natural result
// type of the expression", so there is no declared-type table to compare —
// the type IS whatever expr.Eval decides, evaluated against each phase's own
// symbol table. One binding per interesting result type: string, int
// (arithmetic), float, path, bool (comparison), range_expr (a CHUNK[INT]
// task parameter, exercising the SAME concreteness difference
// TestPhase2Phase3Agreement's own constraintOf already handles — task
// parameters are never concretized at phase 2), and list[int] (a literal,
// included for completeness though it cannot itself disagree between phases).
//
// Phase 2's side cannot call checkLetBindings directly (unexported, and this
// is an external _test package) — but checkLetBindings' own TYPING decision
// is made entirely by expr.Parse + Expression.Eval(syms, expr.TAny, ...)
// against the symbol table; its own contribution beyond that is bookkeeping
// (declaration-order insertion, shadow rejection) that does not change the
// TYPE a successful binding produces. So this test reproduces phase 2's
// typing decision exactly, using the identical exported primitives
// checkLetBindings itself calls, without re-implementing that bookkeeping —
// exactly as constraintOf's own doc comment already establishes for the
// base (non-let) symbol table this test extends.
//
// If any type disagrees, that is a finding about the phase-2/phase-3
// one-code-path design, to report and stop on — not a mismatch to paper
// over by adjusting this assertion.
func TestPhase2Phase3Agreement_LetBindings(t *testing.T) {
	tmpl, err := openjd.Parse([]byte(exprAgreementYAML), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	step := &tmpl.Steps[0]
	if step.Script == nil || len(step.Script.Let) == 0 {
		t.Fatal("exprAgreementYAML must declare a step script let: block for this test to exercise")
	}

	jobParams := map[string]string{
		"JStr": "hello", "JInt": "3", "JFloat": "2.500", "JPath": "/projects/shot.ma",
	}

	// Phase 2: SymbolsFor's own (pre-let) table, then replay
	// step.Script.Let over it via the exact primitives checkLetBindings
	// itself uses -- see the doc comment above for why this is not a
	// second, drifting implementation of the typing decision.
	phase2 := openjd.SymbolsFor(tmpl, step, nil, openjd.ScopeStepScript, jobParams)
	phase2Let := make(map[string]expr.Value, len(step.Script.Let))
	for _, raw := range step.Script.Let {
		name, src := letNameAndSrc(t, raw)
		e, perr := expr.Parse(src)
		if perr != nil {
			t.Fatalf("phase2 parse %q: %v", src, perr)
		}
		v, everr := e.Eval(phase2, expr.TAny)
		if everr != nil {
			t.Fatalf("phase2 eval %q: %v", src, everr)
		}
		phase2[name] = v
		phase2Let[name] = v
	}

	// Phase 3: the SAME raw strings, shipped verbatim on the wire exactly
	// as Task 2 designed (StepScriptLet carries step.Script.Let unchanged),
	// evaluated via ApplyTaskLet against TaskSymbols' table.
	msg := buildAgreementMsg(jobParams)
	msg.StepScriptLet = step.Script.Let
	phase3, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	if err := fmtres.ApplyTaskLet(msg, phase3, nil); err != nil {
		t.Fatalf("ApplyTaskLet: %v", err)
	}

	if len(phase2Let) == 0 {
		t.Fatal("no let bindings exercised -- test fixture is broken")
	}
	for name, p2v := range phase2Let {
		p3v, ok := phase3[name]
		if !ok {
			t.Errorf("let %q: phase-3 table is missing it", name)
			continue
		}
		p2c, p3c := constraintOf(p2v.Type), constraintOf(p3v.Type)
		if !p2c.Equal(p3c) {
			t.Errorf("let %q: phase2 type %s (constraint %s) disagrees with phase3 type %s (constraint %s)",
				name, p2v.Type, p2c, p3v.Type, p3c)
		}
	}
}
