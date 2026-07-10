// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd/fmtstring"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── EmbeddedFileName ──────────────────────────────────────────────────────────

func TestEmbeddedFileName(t *testing.T) {
	tests := []struct {
		name    string
		file    protocol.EmbeddedFile
		want    string
		wantErr string
	}{
		{
			name: "uses Filename when set",
			file: protocol.EmbeddedFile{Name: "script", Filename: "render.sh"},
			want: "render.sh",
		},
		{
			name: "falls back to Name when Filename empty",
			file: protocol.EmbeddedFile{Name: "render.sh"},
			want: "render.sh",
		},
		{
			name:    "rejects path separators",
			file:    protocol.EmbeddedFile{Name: "evil", Filename: "../escape.txt"},
			wantErr: "must not contain path separators",
		},
		{
			name:    "rejects null bytes",
			file:    protocol.EmbeddedFile{Name: "evil", Filename: "bad\x00.txt"},
			wantErr: "must not contain null bytes",
		},
		{
			name:    "rejects both Name and Filename empty",
			file:    protocol.EmbeddedFile{},
			wantErr: "no name",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fmtres.EmbeddedFileName(tc.file)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("EmbeddedFileName: want error containing %q; got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q; want it to contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("EmbeddedFileName: %v", err)
			}
			if got != tc.want {
				t.Errorf("EmbeddedFileName = %q; want %q", got, tc.want)
			}
		})
	}
}

// ── AddFileVars ───────────────────────────────────────────────────────────────

func TestAddFileVars_SetsJoinedPaths(t *testing.T) {
	const workDir = "/sess/work"
	files := []protocol.EmbeddedFile{
		{Name: "script", Filename: "render.sh"}, // Filename used
		{Name: "data.txt"},                      // Name fallback
	}

	scope := fmtres.TaskScope(nil, nil, workDir, "", false)
	if err := fmtres.AddFileVars(scope, "Task.File", files, workDir); err != nil {
		t.Fatalf("AddFileVars: %v", err)
	}

	wantScript := filepath.Join(workDir, "render.sh")
	if got, ok := scope.Lookup("Task.File.script"); !ok || got != wantScript {
		t.Errorf("Task.File.script = (%q, %v); want (%q, true)", got, ok, wantScript)
	}
	wantData := filepath.Join(workDir, "data.txt")
	if got, ok := scope.Lookup("Task.File.data.txt"); !ok || got != wantData {
		t.Errorf("Task.File.data.txt = (%q, %v); want (%q, true)", got, ok, wantData)
	}
}

func TestAddFileVars_EnvPrefix(t *testing.T) {
	const workDir = "/sess/work"
	files := []protocol.EmbeddedFile{{Name: "activate", Filename: "activate.sh"}}

	scope := fmtres.EnvScope(nil, workDir, "", false)
	if err := fmtres.AddFileVars(scope, "Env.File", files, workDir); err != nil {
		t.Fatalf("AddFileVars: %v", err)
	}

	want := filepath.Join(workDir, "activate.sh")
	if got, ok := scope.Lookup("Env.File.activate"); !ok || got != want {
		t.Errorf("Env.File.activate = (%q, %v); want (%q, true)", got, ok, want)
	}
}

func TestAddFileVars_FilenameErrorSurfaces(t *testing.T) {
	files := []protocol.EmbeddedFile{{Name: "evil", Filename: "../escape.txt"}}
	scope := fmtres.TaskScope(nil, nil, "/w", "", false)

	err := fmtres.AddFileVars(scope, "Task.File", files, "/w")
	if err == nil {
		t.Fatal("AddFileVars: want error for invalid filename; got nil")
	}
	if !strings.Contains(err.Error(), "must not contain path separators") {
		t.Errorf("error = %q; want it to mention path separators", err.Error())
	}
	if !strings.Contains(err.Error(), "evil") {
		t.Errorf("error = %q; want it to name the offending file %q", err.Error(), "evil")
	}
}

// ── ResolveEmbeddedFiles ──────────────────────────────────────────────────────

func TestResolveEmbeddedFiles_ResolvesData(t *testing.T) {
	scope := fmtstring.MapScope{"Task.Param.Frame": "42", "Task.File.script": "/w/script.sh"}
	files := []protocol.EmbeddedFile{
		{Name: "a", Filename: "a.txt", Data: "frame={{Task.Param.Frame}}", Runnable: true, EndOfLine: "LF"},
		{Name: "b", Data: "path={{Task.File.script}}"},
	}

	got, err := fmtres.ResolveEmbeddedFiles(files, scope)
	if err != nil {
		t.Fatalf("ResolveEmbeddedFiles: %v", err)
	}
	if got[0].Data != "frame=42" {
		t.Errorf("files[0].Data = %q; want %q", got[0].Data, "frame=42")
	}
	// Other fields must be preserved.
	if !got[0].Runnable || got[0].EndOfLine != "LF" || got[0].Filename != "a.txt" {
		t.Errorf("files[0] metadata not preserved: %+v", got[0])
	}
	if got[1].Data != "path=/w/script.sh" {
		t.Errorf("files[1].Data = %q; want %q", got[1].Data, "path=/w/script.sh")
	}
	// Input must not be mutated.
	if files[0].Data != "frame={{Task.Param.Frame}}" {
		t.Errorf("input mutated: %q", files[0].Data)
	}
}

func TestResolveEmbeddedFiles_NilReturnsNil(t *testing.T) {
	got, err := fmtres.ResolveEmbeddedFiles(nil, fmtstring.MapScope{})
	if err != nil {
		t.Fatalf("ResolveEmbeddedFiles(nil): %v", err)
	}
	if got != nil {
		t.Errorf("ResolveEmbeddedFiles(nil) = %v; want nil", got)
	}
}

func TestResolveEmbeddedFiles_UnknownVariableErrors(t *testing.T) {
	scope := fmtstring.MapScope{}
	files := []protocol.EmbeddedFile{{Name: "bad", Data: "{{Task.Param.Nope}}"}}

	_, err := fmtres.ResolveEmbeddedFiles(files, scope)
	if err == nil {
		t.Fatal("ResolveEmbeddedFiles: want error for unknown variable; got nil")
	}
	if !strings.Contains(err.Error(), "Task.Param.Nope") {
		t.Errorf("error = %q; want it to name the reference", err.Error())
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Errorf("error = %q; want it to name the offending file %q", err.Error(), "bad")
	}
}

// ── Scope construction ────────────────────────────────────────────────────────

func TestTaskScope_ResolvesAllVariables(t *testing.T) {
	scope := fmtres.TaskScope(
		map[string]string{"JobP": "jval"},
		map[string]string{"Frame": "42"},
		"/work/dir",
		"",
		false,
	)

	tests := []struct {
		name string
		want string
	}{
		{"Param.JobP", "jval"},
		{"RawParam.JobP", "jval"},
		{"Task.Param.Frame", "42"},
		{"Task.RawParam.Frame", "42"},
		{"Session.WorkingDirectory", "/work/dir"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := scope.Lookup(tc.name)
			if !ok {
				t.Fatalf("Lookup(%q) ok=false; want true", tc.name)
			}
			if got != tc.want {
				t.Errorf("Lookup(%q) = %q; want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestEnvScope_ResolvesParamAndWorkingDirectory(t *testing.T) {
	scope := fmtres.EnvScope(map[string]string{"JobP": "jval"}, "/work/dir", "", false)

	for _, name := range []string{"Param.JobP", "RawParam.JobP", "Session.WorkingDirectory"} {
		if _, ok := scope.Lookup(name); !ok {
			t.Errorf("Lookup(%q) ok=false; want true", name)
		}
	}
}

func TestEnvScope_DoesNotResolveTaskParam(t *testing.T) {
	// Environments are session-scoped, so Task.Param.* must NOT be available.
	scope := fmtres.EnvScope(map[string]string{"JobP": "jval"}, "/work/dir", "", false)

	if _, ok := scope.Lookup("Task.Param.Frame"); ok {
		t.Error("Lookup(\"Task.Param.Frame\") ok=true; want false (env scope must not expose task params)")
	}
	if _, ok := scope.Lookup("Task.RawParam.Frame"); ok {
		t.Error("Lookup(\"Task.RawParam.Frame\") ok=true; want false")
	}
}

func TestTaskScope_IsExtensibleMap(t *testing.T) {
	// The returned scope must be a map that callers can extend with future keys
	// (e.g. Task.File.*) without a special API.
	scope := fmtres.TaskScope(nil, nil, "/w", "", false)
	scope["Task.File.script"] = "/w/script.sh"
	if got, ok := scope.Lookup("Task.File.script"); !ok || got != "/w/script.sh" {
		t.Errorf("extended key Lookup = (%q, %v); want (\"/w/script.sh\", true)", got, ok)
	}
}

// ── Session path-mapping variables ────────────────────────────────────────────

func TestScopes_ExposePathMappingVariables(t *testing.T) {
	const file = "/sess/path_mapping.json"

	tests := []struct {
		name       string
		build      func() interface{ Lookup(string) (string, bool) }
		wantHas    string
		wantFile   string // expected value when present
		wantPresnt bool   // whether PathMappingRulesFile must be present
	}{
		{
			name: "TaskScope with rules",
			build: func() interface{ Lookup(string) (string, bool) } {
				return fmtres.TaskScope(nil, nil, "/sess", file, true)
			},
			wantHas: "true", wantFile: file, wantPresnt: true,
		},
		{
			name: "TaskScope without rules",
			build: func() interface{ Lookup(string) (string, bool) } {
				return fmtres.TaskScope(nil, nil, "/sess", "", false)
			},
			wantHas: "false", wantPresnt: false,
		},
		{
			name: "EnvScope with rules",
			build: func() interface{ Lookup(string) (string, bool) } {
				return fmtres.EnvScope(nil, "/sess", file, true)
			},
			wantHas: "true", wantFile: file, wantPresnt: true,
		},
		{
			name: "EnvScope without rules",
			build: func() interface{ Lookup(string) (string, bool) } {
				return fmtres.EnvScope(nil, "/sess", "", false)
			},
			wantHas: "false", wantPresnt: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scope := tc.build()

			has, ok := scope.Lookup("Session.HasPathMappingRules")
			if !ok {
				t.Fatal("Session.HasPathMappingRules must always be present")
			}
			if has != tc.wantHas {
				t.Errorf("Session.HasPathMappingRules = %q; want %q", has, tc.wantHas)
			}

			f, ok := scope.Lookup("Session.PathMappingRulesFile")
			if ok != tc.wantPresnt {
				t.Errorf("Session.PathMappingRulesFile present = %v; want %v (value %q)", ok, tc.wantPresnt, f)
			}
			if tc.wantPresnt && f != tc.wantFile {
				t.Errorf("Session.PathMappingRulesFile = %q; want %q", f, tc.wantFile)
			}
		})
	}
}

// ── ResolveAction ─────────────────────────────────────────────────────────────

func TestResolveAction_ResolvesCommandAndArgs(t *testing.T) {
	scope := fmtres.TaskScope(
		map[string]string{"Out": "/out"},
		map[string]string{"Frame": "7"},
		"/sess",
		"",
		false,
	)
	in := &protocol.Action{
		Command:        "render-{{Task.Param.Frame}}",
		Args:           []string{"--out", "{{Param.Out}}", "--cwd", "{{Session.WorkingDirectory}}"},
		TimeoutSeconds: 30,
	}

	got, err := fmtres.ResolveAction(in, scope)
	if err != nil {
		t.Fatalf("ResolveAction: %v", err)
	}
	if got.Command != "render-7" {
		t.Errorf("Command = %q; want %q", got.Command, "render-7")
	}
	wantArgs := []string{"--out", "/out", "--cwd", "/sess"}
	if len(got.Args) != len(wantArgs) {
		t.Fatalf("Args = %v; want %v", got.Args, wantArgs)
	}
	for i := range wantArgs {
		if got.Args[i] != wantArgs[i] {
			t.Errorf("Args[%d] = %q; want %q", i, got.Args[i], wantArgs[i])
		}
	}
	// Non-resolved fields must be preserved.
	if got.TimeoutSeconds != 30 {
		t.Errorf("TimeoutSeconds = %d; want 30", got.TimeoutSeconds)
	}
	// Original must not be mutated.
	if in.Command != "render-{{Task.Param.Frame}}" {
		t.Errorf("input Command mutated to %q", in.Command)
	}
	// Original Args must not be mutated.
	wantOrigArgs := []string{"--out", "{{Param.Out}}", "--cwd", "{{Session.WorkingDirectory}}"}
	for i, orig := range wantOrigArgs {
		if in.Args[i] != orig {
			t.Errorf("input Args[%d] mutated to %q; want %q", i, in.Args[i], orig)
		}
	}
}

func TestResolveAction_NilReturnsNil(t *testing.T) {
	got, err := fmtres.ResolveAction(nil, fmtres.TaskScope(nil, nil, "/w", "", false))
	if err != nil {
		t.Fatalf("ResolveAction(nil): %v", err)
	}
	if got != nil {
		t.Errorf("ResolveAction(nil) = %+v; want nil", got)
	}
}

func TestResolveAction_UnknownVariableErrorsNamingIt(t *testing.T) {
	scope := fmtres.TaskScope(nil, nil, "/w", "", false)
	in := &protocol.Action{Command: "echo", Args: []string{"{{Task.Param.Missing}}"}}

	_, err := fmtres.ResolveAction(in, scope)
	if err == nil {
		t.Fatal("ResolveAction: want error for unknown variable; got nil")
	}
	if !strings.Contains(err.Error(), "Task.Param.Missing") {
		t.Errorf("error = %q; want it to name %q", err.Error(), "Task.Param.Missing")
	}
}

func TestResolveAction_UnknownCommandVariableErrors(t *testing.T) {
	scope := fmtres.TaskScope(nil, nil, "/w", "", false)
	in := &protocol.Action{Command: "{{Param.Nope}}"}

	_, err := fmtres.ResolveAction(in, scope)
	if err == nil {
		t.Fatal("ResolveAction: want error for unknown command variable; got nil")
	}
	if !strings.Contains(err.Error(), "Param.Nope") {
		t.Errorf("error = %q; want it to name %q", err.Error(), "Param.Nope")
	}
}

// ── ResolveVars ───────────────────────────────────────────────────────────────

func TestResolveVars_ResolvesValues(t *testing.T) {
	scope := fmtres.EnvScope(map[string]string{"Root": "/data"}, "/sess", "", false)
	in := map[string]string{
		"OUTPUT_DIR": "{{Param.Root}}/out",
		"SESSION":    "{{Session.WorkingDirectory}}",
		"PLAIN":      "literal",
	}

	got, err := fmtres.ResolveVars(in, scope)
	if err != nil {
		t.Fatalf("ResolveVars: %v", err)
	}
	want := map[string]string{
		"OUTPUT_DIR": "/data/out",
		"SESSION":    "/sess",
		"PLAIN":      "literal",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("ResolveVars[%q] = %q; want %q", k, got[k], v)
		}
	}
	// Input must not be mutated.
	if in["OUTPUT_DIR"] != "{{Param.Root}}/out" {
		t.Errorf("input map mutated: %q", in["OUTPUT_DIR"])
	}
}

func TestResolveVars_NilReturnsNil(t *testing.T) {
	got, err := fmtres.ResolveVars(nil, fmtres.EnvScope(nil, "/w", "", false))
	if err != nil {
		t.Fatalf("ResolveVars(nil): %v", err)
	}
	if got != nil {
		t.Errorf("ResolveVars(nil) = %v; want nil", got)
	}
}

func TestResolveVars_UnknownVariableErrorsNamingKeyAndRef(t *testing.T) {
	scope := fmtres.EnvScope(nil, "/w", "", false)
	in := map[string]string{"BAD": "{{Task.Param.Frame}}"}

	_, err := fmtres.ResolveVars(in, scope)
	if err == nil {
		t.Fatal("ResolveVars: want error for unknown variable; got nil")
	}
	// Error must name both the offending variable reference and the env key.
	if !strings.Contains(err.Error(), "Task.Param.Frame") {
		t.Errorf("error = %q; want it to name the reference %q", err.Error(), "Task.Param.Frame")
	}
	if !strings.Contains(err.Error(), "BAD") {
		t.Errorf("error = %q; want it to name the key %q", err.Error(), "BAD")
	}
}

// ── SQI_CHUNK_BOUNDS end-to-end resolution ────────────────────────────────────

// TestTaskScope_ResolvesChunkBoundsVars proves the SQI_CHUNK_BOUNDS derived
// task-parameter keys ("<name>.Start" / "<name>.End", produced server-side by
// openjd.DeriveChunkBounds and unit-tested there) resolve correctly through
// the ordinary worker-side Task.Param.* scope and format-string substitution
// — i.e. no worker-side change is required, matching the claim in
// docs/openjd-extensions/sqi-chunk-bounds.md.
func TestTaskScope_ResolvesChunkBoundsVars(t *testing.T) {
	// Mirrors the task-params shape openjd.DeriveChunkBounds produces for a
	// CHUNK[INT] parameter named "Frame": the combined range plus derived
	// ".Start" / ".End" keys, all as ordinary task parameters.
	taskParams := map[string]string{
		"Frame":       "1-10",
		"Frame.Start": "1",
		"Frame.End":   "10",
	}
	scope := fmtres.TaskScope(map[string]string{}, taskParams, t.TempDir(), "", false)

	got, err := fmtstring.Resolve("-s {{Task.Param.Frame.Start}} -e {{Task.Param.Frame.End}}", scope)
	if err != nil {
		t.Fatalf("fmtstring.Resolve: %v", err)
	}
	if want := "-s 1 -e 10"; got != want {
		t.Errorf("Resolve = %q; want %q", got, want)
	}
}
