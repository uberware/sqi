// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres_test

// Tests for exprsyms.go: the worker's (phase-3) symbol table, and evaluating
// let: bindings against it.
//
// TestTaskSymbols_* and TestEnvSymbols_* check that every symbol is present
// with the right type and concrete value. The type matters as much as the
// value: a Param.Count bound as string for an INT parameter would concatenate
// instead of add. TestPhase2Phase3Agreement runs a template's declared
// parameters through internal/openjd's submission-time (phase-2) symbol
// table, openjd.SymbolsFor, and through this package's builder, and checks
// that the types agree.
//
// TestApplyTaskLet_* and TestApplyEnvLet_* test ApplyTaskLet and ApplyEnvLet.
// TestPhase2Phase3Agreement_LetBindings makes the same type-agreement check
// for the names a let: block binds, whose types come from evaluating each
// binding.

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
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	// Param.Cnt must be int, not string. Checked by type and by using the
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

	// LIST[PATH] resolves to a concrete value: expr.ValueFromText decodes
	// lists.
	locs, ok := syms.Lookup("Param.Locs")
	if !ok || !locs.Type.Equal(expr.ListOf(expr.TPath)) {
		t.Errorf("Param.Locs = %+v, want list[path]", locs)
	}
	if locs.IsUnresolved() {
		t.Error("Param.Locs is still Unresolved; expr.ValueFromText's " +
			"list case exists so a list parameter resolves in phase 3")
	}
	// RawParam is list[string] for a LIST[PATH]: path mapping applies per
	// element, so the raw form drops to the unmapped strings.
	rawLocs, ok := syms.Lookup("RawParam.Locs")
	if !ok || !rawLocs.Type.Equal(expr.ListOf(expr.TString)) {
		t.Errorf("RawParam.Locs = %+v, want list[string]", rawLocs)
	}
	if rawLocs.IsUnresolved() {
		t.Error("RawParam.Locs is still Unresolved")
	}

	// For a non-PATH declared type, RawParam has Param's type (section 1.2.2:
	// "This is the same as RawParam.<ParamName> for all parameter types
	// except PATH").
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
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
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

// ── PATH parameters: mapping rules applied at bind time ────────────────────

// TestTaskSymbols_JobPathParamIsMappedAtBindTime pins section 1.2.2's rule
// that Param.<name> for a PATH-declared job parameter carries the value with
// path mapping rules already applied, while RawParam.<name> carries "the
// original unmapped value". Apart from calling apply_path_mapping(), Param.
// and Task.Param. are how a template reaches a mapped path.
func TestTaskSymbols_JobPathParamIsMappedAtBindTime(t *testing.T) {
	msg := &protocol.AssignMsg{
		JobParameters:     map[string]string{"Scene": "/mnt/shared/project/shot.ma"},
		JobParameterTypes: map[string]string{"Scene": "PATH"},
		PathMap: []protocol.PathMapRule{
			{SourcePathFormat: "POSIX", SourcePath: "/mnt/shared", DestinationPath: "/local/cache"},
		},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	param, ok := syms.Lookup("Param.Scene")
	if !ok || !param.Type.Equal(expr.TPath) {
		t.Fatalf("Param.Scene = %+v, want path", param)
	}
	// The expected text comes from the same evaluation mapPathParamValue
	// (expres.go) runs, apply_path_mapping(src) under expr.PathNative, so it
	// has the host's separators. See expres_test.go's wantPathText.
	wantParam := wantPathText(t, "apply_path_mapping(src)",
		expr.MapSymbols{"src": expr.String("/mnt/shared/project/shot.ma")},
		expr.WithPathMapping(fmtres.ConvertPathMapRules(msg.PathMap)))
	if param.String() != wantParam {
		t.Errorf("Param.Scene = %q, want the MAPPED value %q", param.String(), wantParam)
	}

	// RawParam is a string for a job PATH parameter (section 1.2.2) and
	// carries the unmapped original text.
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
// counterpart. Here Task.RawParam has Task.Param's type (path; see
// TestTaskSymbols_TaskParamTypes), and the two differ only in whether mapping
// was applied.
func TestTaskSymbols_TaskPathParamIsMappedAtBindTime(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Scene": "/mnt/shared/project/shot.ma"},
		ParameterTypes: map[string]string{"Scene": "PATH"},
		PathMap: []protocol.PathMapRule{
			{SourcePathFormat: "POSIX", SourcePath: "/mnt/shared", DestinationPath: "/local/cache"},
		},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	param, ok := syms.Lookup("Task.Param.Scene")
	if !ok || !param.Type.Equal(expr.TPath) {
		t.Fatalf("Task.Param.Scene = %+v, want path", param)
	}
	// Expected text built as in the job-parameter test above.
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
	// raw is unmapped but still a path, so its rendering also uses the host's
	// separators; the expected text is built the same way, with no rules.
	wantRaw := wantPathText(t, "RAW", expr.MapSymbols{"RAW": expr.Path("/mnt/shared/project/shot.ma", expr.PathNative)})
	if raw.String() != wantRaw {
		t.Errorf("Task.RawParam.Scene = %q, want the ORIGINAL unmapped text %q", raw.String(), wantRaw)
	}
}

// TestTaskSymbols_PathParamNoRulesPassesThrough checks that with no PathMap
// entries, Param.<name> binds to a concrete path built from the raw text, as
// apply_path_mapping does with no rules.
func TestTaskSymbols_PathParamNoRulesPassesThrough(t *testing.T) {
	msg := &protocol.AssignMsg{
		JobParameters:     map[string]string{"Scene": "/mnt/shared/project/shot.ma"},
		JobParameterTypes: map[string]string{"Scene": "PATH"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	param, ok := syms.Lookup("Param.Scene")
	// Expected text built as in the job-parameter test above, with no rules.
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
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	got, ok := syms.Lookup("Task.Param.Frames")
	if !ok {
		t.Fatal("Task.Param.Frames not bound")
	}
	// CHUNK[INT] binds as a range_expr, so a frame range is not expanded.
	// The worker has the actual value (internal/openjd's expansion stores a
	// chunk as a range-expression string), so it must be concrete; an
	// Unresolved value would keep a chunk's frames off the command line.
	if got.IsUnresolved() || !got.Type.Equal(expr.TRangeExpr) {
		t.Fatalf("Task.Param.Frames = %+v, want concrete range_expr", got)
	}
	if got.AsRangeExpr() != "1-10" {
		t.Errorf("Task.Param.Frames.AsRangeExpr() = %q, want %q", got.AsRangeExpr(), "1-10")
	}
}

// TestTaskSymbols_ChunkIntTaskParam_EveryChunkShape reads each chunk shape
// internal/openjd's expansion can hand a worker as a concrete range_expr, and
// checks what an EXPR template computes from it (husk-usd-render takes min()
// and len() of its chunk). The shapes are a CONTIGUOUS "a-b" and a
// NONCONTIGUOUS compact <IntRangeExpr> with steps, listed pairs, negative
// frames, or values in first-seen rather than increasing order.
func TestTaskSymbols_ChunkIntTaskParam_EveryChunkShape(t *testing.T) {
	tests := []struct {
		chunk         string
		len, min, max int64
	}{
		{"5-5", 1, 5, 5},
		{"1-10", 10, 1, 10},
		{"1-19:2", 10, 1, 19},
		{"1-3,7,8", 5, 1, 8},
		{"10-14:2,1", 4, 1, 14},
		{"-3--1", 3, -3, -1},
		{"-10,-8", 2, -10, -8},
	}
	for _, tc := range tests {
		t.Run(tc.chunk, func(t *testing.T) {
			msg := &protocol.AssignMsg{
				Parameters:     map[string]string{"Frames": tc.chunk},
				ParameterTypes: map[string]string{"Frames": "CHUNK[INT]"},
			}
			syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
			if err != nil {
				t.Fatalf("TaskSymbols: %v", err)
			}
			got, ok := syms.Lookup("Task.Param.Frames")
			if !ok || got.IsUnresolved() || !got.Type.Equal(expr.TRangeExpr) {
				t.Fatalf("Task.Param.Frames = %+v, want concrete range_expr", got)
			}
			for fn, want := range map[string]int64{"len": tc.len, "min": tc.min, "max": tc.max} {
				v, err := expr.Eval(fn+"(Task.Param.Frames)", syms, expr.TInt)
				if err != nil {
					t.Fatalf("%s(Task.Param.Frames): %v", fn, err)
				}
				if v.AsInt() != want {
					t.Errorf("%s(Task.Param.Frames) = %d, want %d", fn, v.AsInt(), want)
				}
			}
		})
	}
}

// TestTaskSymbols_ChunkIntTaskParam_InvalidRangeFallsBackToUnresolved pins
// that an unparseable range_expr binds as Unresolved, as INT and FLOAT do in
// expr.ValueFromText.
func TestTaskSymbols_ChunkIntTaskParam_InvalidRangeFallsBackToUnresolved(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Frames": "not-a-range"},
		ParameterTypes: map[string]string{"Frames": "CHUNK[INT]"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
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
	syms, err := fmtres.TaskSymbols(&protocol.AssignMsg{}, "/work/session1", "/work/session1/path_mapping.json", true, nil)
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
	syms, err := fmtres.TaskSymbols(&protocol.AssignMsg{}, "/work/session1", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	has, ok := syms.Lookup("Session.HasPathMappingRules")
	if !ok || !has.Type.Equal(expr.TBool) || has.AsBool() {
		t.Errorf("Session.HasPathMappingRules = %+v, want concrete bool false", has)
	}

	// With no rules the key is absent, so a reference to it fails instead of
	// resolving to an empty path, as addPathMappingKeys does for non-EXPR
	// templates.
	if _, ok := syms.Lookup("Session.PathMappingRulesFile"); ok {
		t.Error("Session.PathMappingRulesFile present with hasPathMap=false, want absent")
	}
}

// ── TaskSymbols: identity ───────────────────────────────────────────────────

func TestTaskSymbols_Identity(t *testing.T) {
	msg := &protocol.AssignMsg{JobName: "RenderJob", StepName: "Render"}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
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
	syms, err := fmtres.TaskSymbols(msg, "/work/session1", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	f, ok := syms.Lookup("Task.File.Script")
	if !ok || !f.Type.Equal(expr.TPath) {
		t.Fatalf("Task.File.Script = %+v, want path", f)
	}
	// The on-disk path comes from filepath.Join, as in AddFileVars. The
	// expected text renders it as an expr path under expr.PathNative, as
	// bindFileSymbols does. See expres_test.go's wantPathText.
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
	if _, err := fmtres.TaskSymbols(msg, "/work", "", false, nil); err == nil {
		t.Fatal("TaskSymbols with an invalid embedded filename returned no error")
	}
}

// ── TaskSymbols: Env.File.* must not appear ─────────────────────────────────

func TestTaskSymbols_ExcludesEnvFile(t *testing.T) {
	msg := &protocol.AssignMsg{
		EmbeddedFiles: []protocol.EmbeddedFile{{Name: "TaskFile"}},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
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
	syms, err := fmtres.EnvSymbols(msg, env, "/work", "", false, nil)
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
	// Expected text built as in TestTaskSymbols_TaskFile.
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

// TestEnvSymbols_ExcludesTaskSymbols checks that an environment, which is
// entered once per session rather than per task, sees no Task.Param.*,
// Task.RawParam.* or Task.File.* symbols, as fmtres.EnvScope does for non-EXPR
// templates. This is a job environment (StepEnvironment is false), so
// Step.Name is absent too; TestEnvSymbols_StepEnvironmentGetsStepName covers
// a step environment.
func TestEnvSymbols_ExcludesTaskSymbols(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Frame": "1"},
		ParameterTypes: map[string]string{"Frame": "INT"},
		StepName:       "S",
	}
	env := &protocol.AssignEnvironment{Name: "Env1"}
	syms, err := fmtres.EnvSymbols(msg, env, "/work", "", false, nil)
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

// TestEnvSymbols_StepEnvironmentGetsStepName checks that a step environment
// (protocol.AssignEnvironment.StepEnvironment true) sees Step.Name, per
// Template Schemas §3.6.2 (ScopeStepEnvironment is ScopeJobEnvironment plus
// Step.Name).
func TestEnvSymbols_StepEnvironmentGetsStepName(t *testing.T) {
	msg := &protocol.AssignMsg{StepName: "Render"}
	env := &protocol.AssignEnvironment{Name: "Env1", StepEnvironment: true}
	syms, err := fmtres.EnvSymbols(msg, env, "/work", "", false, nil)
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
	if _, err := fmtres.EnvSymbols(msg, env, "/work", "", false, nil); err == nil {
		t.Fatal("EnvSymbols with an invalid embedded filename returned no error")
	}
}

// ── Phase 2 / phase 3 agreement ─────────────────────────────────────────────

// exprAgreementYAML declares extensions: [EXPR, TASK_CHUNKING] and has one
// job parameter of each base-spec and RFC 0007 type, one task parameter of
// each base-spec task type (section 1.2.2 / 3.4.1), and a CHUNK[INT] task
// parameter.
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
  # RFC 0007's types. Phase 2 and phase 3 share the section 1.2.2 type
  # mapping and value decoding in internal/openjd/expr; each type is listed
  # so one that resolves on only one side fails the test.
  - name: JBool
    type: BOOL
    default: true
  - name: JRange
    type: RANGE_EXPR
    default: "1-10"
  - name: JListStr
    type: LIST[STRING]
    default: ["a", "b"]
  - name: JListInt
    type: LIST[INT]
    default: [1, 2]
  - name: JListFloat
    type: LIST[FLOAT]
    default: [1.5]
  - name: JListBool
    type: LIST[BOOL]
    default: [true]
  - name: JListPath
    type: LIST[PATH]
    default: ["/a"]
  - name: JListListInt
    type: LIST[LIST[INT]]
    default: [[1, 2]]
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

// constraintOf unwraps an Unresolved type to its constraint, so a phase-2
// placeholder (Unresolved(T)) and a phase-3 concrete value (T) compare on
// their declared type. Phase 2 has no per-task values, so Task.Param.* and
// Task.RawParam.* are always Unresolved there; that is expected and not a
// type disagreement.
func constraintOf(t expr.Type) expr.Type {
	if t.Code == expr.CodeUnresolved && len(t.Params) == 1 {
		return t.Params[0]
	}
	return t
}

// keyDiff returns the keys present in a but absent from b, sorted.
// keyDiff(phase2, phase3) lists symbols submission grants that the worker
// never binds; keyDiff(phase3, phase2) lists symbols the worker binds beyond
// what the scope grants. Comparing whole key sets catches a dropped symbol
// that a hardcoded name list would miss.
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

// assertKeysAndTypesAgree requires phase2 and phase3 to define the same key
// set and, for every key, the same constraint type (see constraintOf). Both
// subtests of TestPhase2Phase3Agreement use it.
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

// buildAgreementMsg builds the AssignMsg matching exprAgreementYAML's
// declared parameters and files.
func buildAgreementMsg(jobParams map[string]string) *protocol.AssignMsg {
	return &protocol.AssignMsg{
		JobName:       "AgreementJob",
		StepName:      "S",
		JobParameters: jobParams,
		JobParameterTypes: map[string]string{
			"JStr": "STRING", "JInt": "INT", "JFloat": "FLOAT", "JPath": "PATH",
			"JBool": "BOOL", "JRange": "RANGE_EXPR",
			"JListStr": "LIST[STRING]", "JListInt": "LIST[INT]",
			"JListFloat": "LIST[FLOAT]", "JListBool": "LIST[BOOL]",
			"JListPath": "LIST[PATH]", "JListListInt": "LIST[LIST[INT]]",
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

	// List values are the canonical JSON internal/openjd/paramjson.go
	// produces, which expr.ValueFromText decodes.
	jobParams := map[string]string{
		"JStr": "hello", "JInt": "3", "JFloat": "2.500", "JPath": "/projects/shot.ma",
		"JBool": "true", "JRange": "1-10",
		"JListStr": `["a","b"]`, "JListInt": "[1,2]", "JListFloat": "[1.5]",
		"JListBool": "[true]", "JListPath": `["/a"]`, "JListListInt": "[[1,2]]",
	}
	const workDir = "/work"
	const pathMapFile = "/work/path_mapping.json"

	t.Run("TaskSymbols vs ScopeStepScript", func(t *testing.T) {
		// hasPathMap=true so Session.PathMappingRulesFile is on both sides.
		// Phase 2 always binds it as a placeholder; with hasPathMap=false the
		// worker omits it, which
		// TestPhase2Phase3Agreement_NoPathMappingOmitsRulesFile covers.
		phase2 := openjd.SymbolsFor(tmpl, step, nil, openjd.ScopeStepScript, jobParams)
		phase3, err := fmtres.TaskSymbols(buildAgreementMsg(jobParams), workDir, pathMapFile, true, nil)
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
		phase3, err := fmtres.EnvSymbols(msg, env, workDir, pathMapFile, true, nil)
		if err != nil {
			t.Fatalf("EnvSymbols: %v", err)
		}
		assertKeysAndTypesAgree(t, "ScopeStepEnvironment", phase2, phase3)
	})
}

// TestPhase2Phase3Agreement_NoPathMappingOmitsRulesFile pins the one expected
// key-set difference between phase 2 and phase 3. Phase 2 always binds
// Session.PathMappingRulesFile as a placeholder, because at submission it
// cannot know whether rules will exist. The worker binds it only when
// hasPathMap is true, so with no rules a reference fails as an unknown symbol
// instead of resolving to an empty path, as addPathMappingKeys does.
func TestPhase2Phase3Agreement_NoPathMappingOmitsRulesFile(t *testing.T) {
	tmpl, err := openjd.Parse([]byte(exprAgreementYAML), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	step := &tmpl.Steps[0]
	// List values are the canonical JSON internal/openjd/paramjson.go
	// produces, which expr.ValueFromText decodes.
	jobParams := map[string]string{
		"JStr": "hello", "JInt": "3", "JFloat": "2.500", "JPath": "/projects/shot.ma",
		"JBool": "true", "JRange": "1-10",
		"JListStr": `["a","b"]`, "JListInt": "[1,2]", "JListFloat": "[1.5]",
		"JListBool": "[true]", "JListPath": `["/a"]`, "JListListInt": "[[1,2]]",
	}

	phase2 := openjd.SymbolsFor(tmpl, step, nil, openjd.ScopeStepScript, jobParams)
	phase3, err := fmtres.TaskSymbols(buildAgreementMsg(jobParams), "/work", "", false, nil)
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

// ── ApplyTaskLet / ApplyEnvLet ──────────────────────────────────────────────

// TestApplyTaskLet_SequentialPropagation pins the ordering internal/openjd's
// checkLetBindings uses and ApplyTaskLet mirrors: bindings evaluate in
// declaration order and each result is visible to later bindings, within the
// step template's block and on into the step script's block (section 3.6.2
// rows 1 and 2).
func TestApplyTaskLet_SequentialPropagation(t *testing.T) {
	msg := &protocol.AssignMsg{
		StepTemplateLet: []string{"x = 1", "y = x + 1"},
		StepScriptLet:   []string{"z = y + 1"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	if err := fmtres.ApplyTaskLet(msg, syms, nil, nil); err != nil {
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

// TestApplyTaskLet_HostOnlySymbol checks that a <StepScript>.let binding over
// Session.WorkingDirectory gets the concrete path on the worker. At submission
// a host-only symbol is only a placeholder.
//
// The binding is in StepScriptLet because section 3.6.2 row 1 evaluates a
// step template's let: block at ScopeStepTemplate, which grants no Session.*
// symbols; only the step script's block (row 2, ScopeStepScript) may reference
// them. TestApplyTaskLet_StepTemplateLetCannotSeeHostSymbols covers the
// template block.
func TestApplyTaskLet_HostOnlySymbol(t *testing.T) {
	msg := &protocol.AssignMsg{
		StepScriptLet: []string{"wd = Session.WorkingDirectory"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work/session1", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	if err := fmtres.ApplyTaskLet(msg, syms, nil, nil); err != nil {
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

// TestApplyTaskLet_StepTemplateLetCannotSeeHostSymbols checks that a
// StepTemplateLet binding referencing a host-only symbol (Session.*) or a
// task-scoped symbol (Task.Param.*) fails as an unknown symbol, as
// internal/openjd's checkStepExpressions rejects it at ScopeStepTemplate on
// the server. The same references from StepScriptLet must succeed.
func TestApplyTaskLet_StepTemplateLetCannotSeeHostSymbols(t *testing.T) {
	for _, ref := range []string{"Session.WorkingDirectory", "Task.Param.Frame"} {
		t.Run(ref, func(t *testing.T) {
			msg := &protocol.AssignMsg{
				Parameters:      map[string]string{"Frame": "1"},
				ParameterTypes:  map[string]string{"Frame": "INT"},
				StepTemplateLet: []string{"x = " + ref},
			}
			syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
			if err != nil {
				t.Fatalf("TaskSymbols: %v", err)
			}
			if err := fmtres.ApplyTaskLet(msg, syms, nil, nil); err == nil {
				t.Fatalf("ApplyTaskLet: want an unknown-symbol error for StepTemplateLet referencing %s, got nil", ref)
			}
			if _, ok := syms.Lookup("x"); ok {
				t.Error(`"x" must not be bound when its own evaluation failed`)
			}
		})
	}

	// The same references from StepScriptLet succeed.
	for _, ref := range []string{"Session.WorkingDirectory", "Task.Param.Frame"} {
		t.Run(ref+"/script", func(t *testing.T) {
			msg := &protocol.AssignMsg{
				Parameters:     map[string]string{"Frame": "1"},
				ParameterTypes: map[string]string{"Frame": "INT"},
				StepScriptLet:  []string{"x = " + ref},
			}
			syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
			if err != nil {
				t.Fatalf("TaskSymbols: %v", err)
			}
			if err := fmtres.ApplyTaskLet(msg, syms, nil, nil); err != nil {
				t.Fatalf("ApplyTaskLet: %v (StepScriptLet referencing %s should succeed)", err, ref)
			}
			if _, ok := syms.Lookup("x"); !ok {
				t.Error(`"x" should be bound`)
			}
		})
	}
}

// TestApplyTaskLet_StepTemplateLetSeesParamAndIdentity checks what
// ScopeStepTemplate does grant: Param.<name>, RawParam.<name>, Job.Name and
// Step.Name are available to a StepTemplateLet binding, and the bound name is
// visible to the following StepScriptLet block (section 3.6.2 rows 1 and 2).
func TestApplyTaskLet_StepTemplateLetSeesParamAndIdentity(t *testing.T) {
	msg := &protocol.AssignMsg{
		JobParameters:     map[string]string{"Scale": "2"},
		JobParameterTypes: map[string]string{"Scale": "INT"},
		JobName:           "J",
		StepName:          "S",
		StepTemplateLet:   []string{"doubled = Param.Scale * 2", "label = Job.Name + '/' + Step.Name"},
		StepScriptLet:     []string{"tripled = doubled + Param.Scale"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	if err := fmtres.ApplyTaskLet(msg, syms, nil, nil); err != nil {
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
// shadowing rule across blocks: the step script's block may not rebind a name
// the step template's block bound. The assignment carries the two blocks
// separately so this can be checked. The template's value must survive.
func TestApplyTaskLet_ScriptShadowingTemplateRejected(t *testing.T) {
	msg := &protocol.AssignMsg{
		StepTemplateLet: []string{"x = 1"},
		StepScriptLet:   []string{"x = 2"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	if err := fmtres.ApplyTaskLet(msg, syms, nil, nil); err == nil {
		t.Fatal("ApplyTaskLet: want an error for the script shadowing the template's binding, got nil")
	}

	x, ok := syms.Lookup("x")
	if !ok || !x.Type.Equal(expr.TInt) || x.AsInt() != 1 {
		t.Errorf("x = %+v, want the TEMPLATE's binding (1) preserved, not overwritten by the rejected script binding", x)
	}
}

// TestApplyTaskLet_WithinBlockShadowingRejected is the same rule within one
// block: two bindings sharing a name.
func TestApplyTaskLet_WithinBlockShadowingRejected(t *testing.T) {
	msg := &protocol.AssignMsg{
		StepTemplateLet: []string{"x = 1", "x = 2"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	if err := fmtres.ApplyTaskLet(msg, syms, nil, nil); err == nil {
		t.Fatal("ApplyTaskLet: want an error for a same-block shadow, got nil")
	}
	x, ok := syms.Lookup("x")
	if !ok || x.AsInt() != 1 {
		t.Errorf("x = %+v, want the FIRST binding (1) preserved", x)
	}
}

// TestApplyTaskLet_FailureSurfacesWithoutHidingOthers checks that a binding
// that fails (here, an unknown symbol) returns an error, is not inserted into
// syms, and does not stop later independent bindings in the same block from
// being bound. internal/openjd's checkLetBindings also continues past a
// failure.
func TestApplyTaskLet_FailureSurfacesWithoutHidingOthers(t *testing.T) {
	msg := &protocol.AssignMsg{
		StepTemplateLet: []string{"bad = NoSuchSymbol", "good = 1"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	err = fmtres.ApplyTaskLet(msg, syms, nil, nil)
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

// TestApplyTaskLet_CapEnforced pins the 50-binding cap. The server rejects a
// block over the cap at submission; the worker has no such check, so
// evalLetBindings truncating to 50 is its only bound on the count.
func TestApplyTaskLet_CapEnforced(t *testing.T) {
	syms, err := fmtres.TaskSymbols(&protocol.AssignMsg{}, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	msg := &protocol.AssignMsg{StepTemplateLet: makeLetSequence(51)}
	if err := fmtres.ApplyTaskLet(msg, syms, nil, nil); err != nil {
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

// TestApplyEnvLet_BasicAndNil checks that ApplyEnvLet evaluates the
// environment's own let: block against EnvSymbols' table, and that a nil env
// is a no-op, as in EnvSymbols.
func TestApplyEnvLet_BasicAndNil(t *testing.T) {
	env := &protocol.AssignEnvironment{
		Name: "Env1",
		Let:  []string{"cfg = Env.File.Config"},
		EmbeddedFiles: []protocol.EmbeddedFile{
			{Name: "Config"},
		},
	}
	msg := &protocol.AssignMsg{}
	syms, err := fmtres.EnvSymbols(msg, env, "/work/session1", "", false, nil)
	if err != nil {
		t.Fatalf("EnvSymbols: %v", err)
	}
	if err := fmtres.ApplyEnvLet(env, syms, nil, nil); err != nil {
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

	if err := fmtres.ApplyEnvLet(nil, expr.MapSymbols{}, nil, nil); err != nil {
		t.Errorf("ApplyEnvLet(nil, ...) = %v, want nil (a no-op)", err)
	}
}

// TestApplyEnvLet_ShadowRejected is TestApplyTaskLet_WithinBlockShadowingRejected's
// environment counterpart.
func TestApplyEnvLet_ShadowRejected(t *testing.T) {
	env := &protocol.AssignEnvironment{Let: []string{"x = 1", "x = 2"}}
	syms, err := fmtres.EnvSymbols(&protocol.AssignMsg{}, env, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("EnvSymbols: %v", err)
	}
	if err := fmtres.ApplyEnvLet(env, syms, nil, nil); err == nil {
		t.Fatal("ApplyEnvLet: want an error for a same-block shadow, got nil")
	}
	x, ok := syms.Lookup("x")
	if !ok || x.AsInt() != 1 {
		t.Errorf("x = %+v, want the FIRST binding (1) preserved", x)
	}
}

// ── ApplyTaskLet/ApplyEnvLet cap: bound count ───────────────────────────────
//
// TestApplyTaskLet_CapEnforced_MutationCheck counts every bound name rather
// than checking two specific ones: with 51 independent bindings, exactly 50
// must be bound.
func TestApplyTaskLet_CapEnforced_MutationCheck(t *testing.T) {
	syms, err := fmtres.TaskSymbols(&protocol.AssignMsg{}, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	msg := &protocol.AssignMsg{StepTemplateLet: makeLetSequence(51)}
	if err := fmtres.ApplyTaskLet(msg, syms, nil, nil); err != nil {
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

// ── Phase 2 / phase 3 agreement for let bindings ────────────────────────────

// letNameAndSrc splits one of exprAgreementYAML's let: strings into name and
// expression so the test can replay it against the phase-2 table. The strings
// are authored in this file, so strings.Cut is enough; fmtres'
// splitLetBinding is unexported.
func letNameAndSrc(t *testing.T, raw string) (name, src string) {
	t.Helper()
	before, after, ok := strings.Cut(raw, "=")
	if !ok {
		t.Fatalf("malformed let binding in test fixture: %q", raw)
	}
	return strings.TrimSpace(before), strings.TrimSpace(after)
}

// TestPhase2Phase3Agreement_LetBindings extends TestPhase2Phase3Agreement to
// the names a let: block binds. Section 3.6.1 makes a binding's type "the
// natural result type of the expression", so the test compares the types
// expr evaluation produces against each phase's symbol table. There is one
// binding per result type: string, int (arithmetic), float, path, bool
// (comparison), range_expr (a CHUNK[INT] task parameter, Unresolved at phase
// 2; see constraintOf) and list[int].
//
// checkLetBindings is unexported, so the phase-2 side replays the bindings
// with the same exported calls it makes, expr.Parse and Expression.Eval with
// expr.TAny. Its other work (insertion order, shadow rejection) does not
// change the type a successful binding produces.
//
// A type disagreement means the two phases have drifted; fix the cause rather
// than adjusting this assertion.
func TestPhase2Phase3Agreement_LetBindings(t *testing.T) {
	tmpl, err := openjd.Parse([]byte(exprAgreementYAML), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	step := &tmpl.Steps[0]
	if step.Script == nil || len(step.Script.Let) == 0 {
		t.Fatal("exprAgreementYAML must declare a step script let: block for this test to exercise")
	}

	// List values are the canonical JSON internal/openjd/paramjson.go
	// produces, which expr.ValueFromText decodes.
	jobParams := map[string]string{
		"JStr": "hello", "JInt": "3", "JFloat": "2.500", "JPath": "/projects/shot.ma",
		"JBool": "true", "JRange": "1-10",
		"JListStr": `["a","b"]`, "JListInt": "[1,2]", "JListFloat": "[1.5]",
		"JListBool": "[true]", "JListPath": `["/a"]`, "JListListInt": "[[1,2]]",
	}

	// Phase 2: SymbolsFor's table before let:, then step.Script.Let replayed
	// over it as checkLetBindings evaluates it.
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

	// Phase 3: the same strings (StepScriptLet carries step.Script.Let
	// unchanged), evaluated by ApplyTaskLet against TaskSymbols' table.
	msg := buildAgreementMsg(jobParams)
	msg.StepScriptLet = step.Script.Let
	phase3, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	if err := fmtres.ApplyTaskLet(msg, phase3, nil, nil); err != nil {
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

// ── EnvSymbols and the enclosing step template's let: block ─────────────────
//
// Template Schemas §3.6.2 row 1 makes a <StepTemplate>.let binding's names
// available in stepEnvironments as well as in the step's script, and §3.6
// says "a let binding in a <StepTemplate>'s stepEnvironments cannot shadow a
// binding from that step's let block". The server checks environments with
// those names in scope (checkStepExpressions passes them to
// checkEnvironmentExpressions as outerLet), so the worker must bind them too
// or every task in an accepted step would fail.

// stepEnvMsg builds an AssignMsg carrying a step-template let: block, shared
// by the tests below.
func stepEnvMsg(lets ...string) *protocol.AssignMsg {
	return &protocol.AssignMsg{
		EXPR:              true,
		JobName:           "J",
		StepName:          "S",
		JobParameters:     map[string]string{"Scene": "shot"},
		JobParameterTypes: map[string]string{"Scene": "STRING"},
		StepTemplateLet:   lets,
	}
}

func TestEnvSymbols_StepEnvironmentSeesStepTemplateLet(t *testing.T) {
	msg := stepEnvMsg(`outdir = "/tmp/out"`, `tagged = Param.Scene + "-v1"`)
	env := &protocol.AssignEnvironment{Name: "E", StepEnvironment: true}

	syms, err := fmtres.EnvSymbols(msg, env, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("EnvSymbols: %v", err)
	}
	for name, want := range map[string]string{"outdir": "/tmp/out", "tagged": "shot-v1"} {
		v, ok := syms[name]
		if !ok {
			t.Errorf("step environment table is missing let binding %q", name)
			continue
		}
		if got := v.String(); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestEnvSymbols_JobEnvironmentIgnoresStepTemplateLet(t *testing.T) {
	msg := stepEnvMsg(`outdir = "/tmp/out"`)
	env := &protocol.AssignEnvironment{Name: "E"} // StepEnvironment false

	syms, err := fmtres.EnvSymbols(msg, env, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("EnvSymbols: %v", err)
	}
	if _, ok := syms["outdir"]; ok {
		t.Error(`job environment table binds "outdir": a job environment has no enclosing step template (§3.6.2 row 1)`)
	}
	if _, ok := syms["Step.Name"]; ok {
		t.Error(`job environment table binds "Step.Name"`)
	}
}

// TestEnvSymbols_StepTemplateLetCannotSeeEnvSymbols checks that the
// step-template block is evaluated at ScopeStepTemplate on the environment
// path too, so Session.* and Env.File.* stay invisible to it, as in phase 2's
// checkLetBindings at ScopeStepTemplate.
func TestEnvSymbols_StepTemplateLetCannotSeeEnvSymbols(t *testing.T) {
	for _, src := range []string{
		`bad = str(Session.WorkingDirectory)`,
		`bad = str(Env.File.Config)`,
	} {
		msg := stepEnvMsg(src)
		env := &protocol.AssignEnvironment{
			Name:            "E",
			StepEnvironment: true,
			EmbeddedFiles:   []protocol.EmbeddedFile{{Name: "Config", Filename: "cfg.txt"}},
		}
		_, err := fmtres.EnvSymbols(msg, env, "/work", "/work/pm.json", true, nil)
		if err == nil {
			t.Errorf("%s: want an unknown-symbol error, got nil", src)
			continue
		}
		if !strings.Contains(err.Error(), "unknown symbol") {
			t.Errorf("%s: error = %v, want unknown symbol", src, err)
		}
	}
}

// TestApplyEnvLet_ShadowsStepTemplateLetRejected is §3.6's shadow rule across
// the two blocks: "a let binding in a <StepTemplate>'s stepEnvironments
// cannot shadow a binding from that step's let block". The worker can detect
// the collision because EnvSymbols binds the step-template name.
func TestApplyEnvLet_ShadowsStepTemplateLetRejected(t *testing.T) {
	msg := stepEnvMsg(`outdir = "/tmp/out"`)
	env := &protocol.AssignEnvironment{
		Name:            "E",
		StepEnvironment: true,
		Let:             []string{`outdir = "/tmp/elsewhere"`},
	}

	syms, err := fmtres.EnvSymbols(msg, env, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("EnvSymbols: %v", err)
	}
	err = fmtres.ApplyEnvLet(env, syms, nil, nil)
	if err == nil {
		t.Fatal("ApplyEnvLet: want a shadow rejection, got nil")
	}
	if !strings.Contains(err.Error(), "shadows") {
		t.Errorf("error = %v, want it to report shadowing", err)
	}
	if got := syms["outdir"].String(); got != "/tmp/out" {
		t.Errorf("outdir = %q after the rejected rebinding; want the step template's %q", got, "/tmp/out")
	}
}

// TestPhase2Phase3Agreement_StepEnvironmentLet checks that a template using a
// step-template let: binding in a step environment, which
// openjd.ValidateWithOptions accepts with no errors under the default
// options, also resolves on the worker with no errors.
func TestPhase2Phase3Agreement_StepEnvironmentLet(t *testing.T) {
	const yaml = `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: R
steps:
- name: S
  let:
    - 'outdir = "/tmp/out"'
  stepEnvironments:
  - name: E
    variables:
      OUT: "{{ outdir }}"
    script:
      actions:
        onEnter:
          command: echo
          args: ["{{ outdir }}"]
        onExit:
          command: echo
          args: ["{{ outdir }}"]
  script:
    actions:
      onRun:
        command: echo
`
	tmpl, err := openjd.Parse([]byte(yaml), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, ve := range openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{}) {
		t.Errorf("phase 2 reports %s: %s -- the fixture must be phase-2 clean", ve.Pointer, ve.Message)
	}

	step := tmpl.Steps[0]
	msg := &protocol.AssignMsg{
		EXPR:            true,
		JobName:         "R",
		StepName:        step.Name,
		StepTemplateLet: step.Let,
	}
	env := &protocol.AssignEnvironment{
		Name:            "E",
		StepEnvironment: true,
		Variables:       map[string]string{"OUT": "{{ outdir }}"},
		OnEnter:         &protocol.Action{Command: "echo", Args: []string{"{{ outdir }}"}},
		OnExit:          &protocol.Action{Command: "echo", Args: []string{"{{ outdir }}"}},
	}
	syms, err := fmtres.EnvSymbols(msg, env, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("phase 3 EnvSymbols: %v", err)
	}
	vars, err := fmtres.ResolveVarsExpr(env.Variables, syms, nil, nil)
	if err != nil {
		t.Fatalf("phase 3 ResolveVarsExpr: %v", err)
	}
	if vars["OUT"] != "/tmp/out" {
		t.Errorf("OUT = %q, want %q", vars["OUT"], "/tmp/out")
	}
	for label, action := range map[string]*protocol.Action{"onEnter": env.OnEnter, "onExit": env.OnExit} {
		got, aerr := fmtres.ResolveActionExpr(action, syms, nil, nil)
		if aerr != nil {
			t.Fatalf("phase 3 ResolveActionExpr(%s): %v", label, aerr)
		}
		if len(got.Args) != 1 || got.Args[0] != "/tmp/out" {
			t.Errorf("%s args = %v, want [/tmp/out]", label, got.Args)
		}
	}
}

// ── Retained-bytes bound ────────────────────────────────────────────────────

// bigLetSequence builds n bindings that each construct about 9.9 MB of
// string. Each is under the per-evaluation memory limit
// (ExprLimits.MemoryLimit); the cost comes from retaining all of them in one
// table.
//
// The multiplier comes from Task.Param.N rather than a literal so the server
// would accept the template: Task.Param.N is unresolved[int] at submission,
// so `"x" * unresolved` costs almost nothing against the submission limits.
func bigLetSequence(n int) []string {
	lets := make([]string, n)
	for i := range lets {
		lets[i] = fmt.Sprintf(`a%d = "x" * (Task.Param.N * 100000)`, i)
	}
	return lets
}

func bigLetMsg(n int) *protocol.AssignMsg {
	return &protocol.AssignMsg{
		EXPR:           true,
		Parameters:     map[string]string{"N": "99"},
		ParameterTypes: map[string]string{"N": "INT"},
		StepScriptLet:  bigLetSequence(n),
	}
}

// tableBytes sums expr.SizeOf over the table, so the tests measure the table
// directly instead of relying on the package's own accounting.
func tableBytes(syms expr.MapSymbols) int64 {
	var total int64
	for _, v := range syms {
		total += expr.SizeOf(v)
	}
	return total
}

// TestApplyTaskLet_RetainedBytesBounded checks that the per-table
// retained-bytes limit (10 MB by default) stops a block of 50 bindings of
// about 9.9 MB each, which would otherwise retain about 495 MB in the
// long-lived worker process. The table must stay under the limit.
func TestApplyTaskLet_RetainedBytesBounded(t *testing.T) {
	msg := bigLetMsg(50)
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	err = fmtres.ApplyTaskLet(msg, syms, nil, nil)
	if err == nil {
		t.Fatal("ApplyTaskLet: want a retained-bytes error, got nil -- 50 x ~9.9 MB was accepted")
	}
	if !strings.Contains(err.Error(), "would retain") {
		t.Errorf("error = %v; want it to name the retained-bytes limit", err)
	}

	if got := tableBytes(syms); got > 10_000_000 {
		t.Errorf("table retains %d bytes, over the 10,000,000-byte limit", got)
	}
	bound := 0
	for i := range 50 {
		if _, ok := syms.Lookup(fmt.Sprintf("a%d", i)); ok {
			bound++
		}
	}
	if bound != 1 {
		t.Errorf("bound %d of 50 oversized bindings; want exactly 1 (the block stops at the limit)", bound)
	}
}

// TestApplyTaskLet_RetainedBytesAccumulatesAcrossBlocks checks that the bound
// applies per table: the step-script block is charged for what the
// step-template block already retained, because the check measures the table
// it writes into. Splitting an oversized block across the two blocks must not
// double the budget.
func TestApplyTaskLet_RetainedBytesAccumulatesAcrossBlocks(t *testing.T) {
	// Param.N, not Task.Param.N: the step-template block is evaluated at
	// ScopeStepTemplate, where task parameters do not exist.
	msg := &protocol.AssignMsg{
		EXPR:              true,
		JobParameters:     map[string]string{"N": "99"},
		JobParameterTypes: map[string]string{"N": "INT"},
		StepTemplateLet:   []string{`a0 = "x" * (Param.N * 100000)`},
		StepScriptLet:     []string{`b0 = "x" * (Param.N * 100000)`},
	}

	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	err = fmtres.ApplyTaskLet(msg, syms, nil, nil)
	if err == nil {
		t.Fatal("ApplyTaskLet: want a retained-bytes error; the script block reused the template block's budget")
	}
	if _, ok := syms.Lookup("a0"); !ok {
		t.Error(`"a0" (the step-template binding, first and within budget) should be bound`)
	}
	if _, ok := syms.Lookup("b0"); ok {
		t.Error(`"b0" should NOT be bound -- the table was already at its retained-bytes limit`)
	}
}

// TestApplyEnvLet_RetainedBytesBounded is the environment counterpart. An
// environment table is held for the whole session, so it gets the same bound
// as the task table.
func TestApplyEnvLet_RetainedBytesBounded(t *testing.T) {
	msg := &protocol.AssignMsg{
		EXPR:              true,
		JobParameters:     map[string]string{"N": "99"},
		JobParameterTypes: map[string]string{"N": "INT"},
	}
	env := &protocol.AssignEnvironment{Name: "E"}
	for i := range 50 {
		env.Let = append(env.Let, fmt.Sprintf(`a%d = "x" * (Param.N * 100000)`, i))
	}

	syms, err := fmtres.EnvSymbols(msg, env, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("EnvSymbols: %v", err)
	}
	if err := fmtres.ApplyEnvLet(env, syms, nil, nil); err == nil {
		t.Fatal("ApplyEnvLet: want a retained-bytes error, got nil")
	}
	if got := tableBytes(syms); got > 10_000_000 {
		t.Errorf("environment table retains %d bytes, over the 10,000,000-byte limit", got)
	}
}

// TestApplyTaskLet_RetainedBytesAllowsOrdinaryBlocks checks that 50 ordinary
// bindings, the most a block may declare, fit within the retained-bytes
// limit.
func TestApplyTaskLet_RetainedBytesAllowsOrdinaryBlocks(t *testing.T) {
	msg := &protocol.AssignMsg{EXPR: true, StepScriptLet: makeLetSequence(50)}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	if err := fmtres.ApplyTaskLet(msg, syms, nil, nil); err != nil {
		t.Fatalf("ApplyTaskLet: %v -- 50 ordinary bindings must fit the retained-bytes budget", err)
	}
	if _, ok := syms.Lookup("a49"); !ok {
		t.Error(`"a49" (the 50th ordinary binding) should be bound`)
	}
}

// TestApplyTaskLet_StepTemplateLetCannotOverwriteSpecSymbols: the
// step-template block is evaluated against a ScopeStepTemplate projection of
// the table (stepTemplateLetScope) but merged into the full table. The shadow
// check must use the full table, or a binding named for a symbol outside the
// projection (Session.*, Task.*) would replace the real symbol and onRun would
// render the binding's value.
//
// A submitted template cannot reach this: §3.6.1's <UserIdentifier> grammar
// rejects a dotted name at submission, and splitLetBinding does not re-check
// wire data. The test guards the table's invariant.
func TestApplyTaskLet_StepTemplateLetCannotOverwriteSpecSymbols(t *testing.T) {
	msg := &protocol.AssignMsg{
		EXPR:           true,
		StepName:       "S",
		Parameters:     map[string]string{"N": "5"},
		ParameterTypes: map[string]string{"N": "INT"},
		StepTemplateLet: []string{
			`Session.WorkingDirectory = "/pwned"`,
			`Task.Param.N = 999`,
			`Step.Name = "nope"`,
		},
		OnRun: &protocol.Action{
			Command: "echo",
			Args:    []string{"{{ Session.WorkingDirectory }}", "{{ Task.Param.N }}"},
		},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	lerr := fmtres.ApplyTaskLet(msg, syms, nil, nil)
	if lerr == nil {
		t.Fatal("ApplyTaskLet: want shadow rejections for all three bindings, got nil")
	}
	for _, name := range []string{"Session.WorkingDirectory", "Task.Param.N", "Step.Name"} {
		if !strings.Contains(lerr.Error(), fmt.Sprintf("%q shadows", name)) {
			t.Errorf("error does not reject %q as shadowing: %v", name, lerr)
		}
	}

	action, aerr := fmtres.ResolveActionExpr(msg.OnRun, syms, nil, nil)
	if aerr != nil {
		t.Fatalf("ResolveActionExpr: %v", aerr)
	}
	// The session directory renders in the host's path flavor (pathFlavor is
	// expr.PathNative), so the expected text is computed the same way.
	wantWorkDir := expr.Path("/work", expr.PathNative).String()
	if len(action.Args) != 2 || action.Args[0] != wantWorkDir || action.Args[1] != "5" {
		t.Errorf("args = %v; want the REAL session directory and task parameter, not the let bindings' values", action.Args)
	}
}

// TestApplyTaskLet_RetainedBytesCountsNonProjectedSymbols is the byte
// accounting counterpart of the test above. The step-template block resolves
// against the ScopeStepTemplate projection, which excludes Task.Param.*,
// Task.RawParam.*, Task.File.*, Session.* and Env.File.*, but its bindings
// land in the full table, so the retained-bytes check must measure the full
// table. Here the table starts at 6 MB of task parameters, all outside the
// projection, so a 9 MB binding must be refused against the 10 MB limit.
func TestApplyTaskLet_RetainedBytesCountsNonProjectedSymbols(t *testing.T) {
	// A 3 MB STRING task parameter, bound as both Task.Param.Big and
	// Task.RawParam.Big, both outside stepTemplateLetScope's projection.
	msg := &protocol.AssignMsg{
		EXPR:            true,
		Parameters:      map[string]string{"Big": strings.Repeat("z", 3_000_000)},
		ParameterTypes:  map[string]string{"Big": "STRING"},
		StepTemplateLet: []string{`a0 = "x" * 9000000`},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	if start := tableBytes(syms); start < 6_000_000 {
		t.Fatalf("fixture is broken: table starts at %d bytes, want at least 6,000,000 outside the projection", start)
	}

	if err := fmtres.ApplyTaskLet(msg, syms, nil, nil); err == nil {
		t.Fatal("ApplyTaskLet: want a retained-bytes error; the step-template block metered the projection, not the merge target")
	}
	if _, ok := syms.Lookup("a0"); ok {
		t.Error(`"a0" should NOT be bound -- 6 MB of task parameters plus 9 MB is over the limit`)
	}
	if got := tableBytes(syms); got > 10_000_000 {
		t.Errorf("table retains %d bytes, over the 10,000,000-byte limit -- the stated per-table invariant does not hold", got)
	}
}
