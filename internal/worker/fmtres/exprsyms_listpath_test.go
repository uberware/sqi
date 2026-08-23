// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres_test

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// hostPath and hostPathList render a POSIX-written expectation in the flavor
// THIS host produces, because these tests assert on concrete path values built
// at phase 3.
//
// internal/worker/fmtres fixes expr.PathFormat to PathNative (exprsyms.go's
// pathFlavor), which is not an accident to be normalized away here:
// Expression-Language section 1.2.1 says host contexts (SESSION and TASK
// scopes) take the host operating system's path semantics, and phase 3 IS a
// host context — it runs on the worker, against a real session directory.
// Phase 2, at TEMPLATE scope, correctly keeps POSIX so a server-side template
// expands identically whatever submitted it.
//
// So on a Windows worker `/mnt/render/shot.ma` really does come back as
// `\mnt\render\shot.ma`, and that is correct. Hardcoding the POSIX
// spelling only ever passed because CI runs these on Linux. On POSIX both helpers are
// the identity transform, so the assertions there are byte-for-byte what they
// were before.
func hostPath(posix string) string { return filepath.FromSlash(posix) }

// hostPathList renders the String() form of a list[path]: JSON-style quoting,
// ", " separated — so a Windows separator appears escaped, exactly as the
// value's own String() escapes it.
func hostPathList(posix ...string) string {
	quoted := make([]string, len(posix))
	for i, p := range posix {
		quoted[i] = strconv.Quote(hostPath(p))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// TestTaskSymbols_ListPathIsMapped pins RFC 0007's requirement that
// Param.<name> for a LIST[PATH] returns "a list[path] type value with path
// mapping applied".
//
// Before sub-project F2 the mapping branch matched only a SCALAR path, on the
// stated grounds that a concrete LIST[PATH] could never exist — true when
// written, and made false by F1, which taught expr.ValueFromText to decode
// lists. The value then took the unmapped branch silently.
func TestTaskSymbols_ListPathIsMapped(t *testing.T) {
	msg := &protocol.AssignMsg{
		JobName: "J", StepName: "S",
		JobParameters: map[string]string{
			"Scene":    "/projects/shot.ma",
			"Textures": `["/projects/tex/a.exr","/projects/tex/b.exr"]`,
		},
		JobParameterTypes: map[string]string{
			"Scene": "PATH", "Textures": "LIST[PATH]",
		},
		PathMap: []protocol.PathMapRule{{
			SourcePathFormat: "POSIX",
			SourcePath:       "/projects",
			DestinationPath:  "/mnt/render",
		}},
	}

	syms, err := fmtres.TaskSymbols(msg, "/work", "/work/path_mapping.json", true, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	// The scalar case, unchanged — proves the fixture's rule actually matches.
	scene, ok := syms.Lookup("Param.Scene")
	if !ok {
		t.Fatal("Param.Scene not bound")
	}
	if got, want := scene.String(), hostPath("/mnt/render/shot.ma"); got != want {
		t.Errorf("Param.Scene = %q, want %q", got, want)
	}

	tex, ok := syms.Lookup("Param.Textures")
	if !ok {
		t.Fatal("Param.Textures not bound")
	}
	want := hostPathList("/mnt/render/tex/a.exr", "/mnt/render/tex/b.exr")
	if got := tex.String(); got != want {
		t.Errorf("Param.Textures = %s, want %s\n"+
			"RFC 0007: Param.<name> for a LIST[PATH] returns a list[path] "+
			"with path mapping applied, per element.", got, want)
	}
}

// TestTaskSymbols_ListPathRawIsUnmapped is the other half of section 1.2.2:
// RawParam carries the ORIGINAL unmapped value, as list[string].
func TestTaskSymbols_ListPathRawIsUnmapped(t *testing.T) {
	msg := &protocol.AssignMsg{
		JobName: "J", StepName: "S",
		JobParameters:     map[string]string{"Textures": `["/projects/tex/a.exr"]`},
		JobParameterTypes: map[string]string{"Textures": "LIST[PATH]"},
		PathMap: []protocol.PathMapRule{{
			SourcePathFormat: "POSIX",
			SourcePath:       "/projects",
			DestinationPath:  "/mnt/render",
		}},
	}

	syms, err := fmtres.TaskSymbols(msg, "/work", "/work/path_mapping.json", true, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	raw, ok := syms.Lookup("RawParam.Textures")
	if !ok {
		t.Fatal("RawParam.Textures not bound")
	}
	if want := `["/projects/tex/a.exr"]`; raw.String() != want {
		t.Errorf("RawParam.Textures = %s, want %s unmapped", raw.String(), want)
	}
}

// TestTaskSymbols_ListPathNoRules pins the passthrough case: with no rules the
// engine normalizes but does not rewrite, so the elements come back as given.
func TestTaskSymbols_ListPathNoRules(t *testing.T) {
	msg := &protocol.AssignMsg{
		JobName: "J", StepName: "S",
		JobParameters:     map[string]string{"Textures": `["/projects/a.exr"]`},
		JobParameterTypes: map[string]string{"Textures": "LIST[PATH]"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	tex, ok := syms.Lookup("Param.Textures")
	if !ok {
		t.Fatal("Param.Textures not bound")
	}
	if want := hostPathList("/projects/a.exr"); tex.String() != want {
		t.Errorf("Param.Textures = %s, want %s", tex.String(), want)
	}
}
