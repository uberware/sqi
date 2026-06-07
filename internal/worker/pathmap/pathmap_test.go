// SPDX-License-Identifier: AGPL-3.0-only

package pathmap_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/worker/pathmap"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Parse / validation ────────────────────────────────────────────────────────

// TestParse_empty verifies that an empty rule slice is accepted and produces
// an empty Lookup.
func TestParse_empty(t *testing.T) {
	l, err := pathmap.Parse(nil)
	if err != nil {
		t.Fatalf("Parse(nil): unexpected error: %v", err)
	}
	if l == nil {
		t.Fatal("Parse(nil): returned nil Lookup")
	}
	if l.Len() != 0 {
		t.Errorf("Len() = %d; want 0", l.Len())
	}
}

// TestParse_valid verifies that a fully-populated rule slice parses without
// error.
func TestParse_valid(t *testing.T) {
	rules := []protocol.PathMapRule{
		{SourcePathFormat: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
		{SourcePathFormat: "s3://bucket/assets", DestinationPath: "/mnt/s3/assets"},
	}
	l, err := pathmap.Parse(rules)
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if l.Len() != 2 {
		t.Errorf("Len() = %d; want 2", l.Len())
	}
}

// TestParse_emptyDestinationPath verifies that a rule with an empty
// DestinationPath causes Parse to return an error identifying the offending
// source path (task 62).
func TestParse_emptyDestinationPath(t *testing.T) {
	tests := []struct {
		name    string
		rules   []protocol.PathMapRule
		wantSrc string // expected to appear in the error message
	}{
		{
			name: "single empty destination",
			rules: []protocol.PathMapRule{
				{SourcePathFormat: "/nfs/projects", DestinationPath: ""},
			},
			wantSrc: "/nfs/projects",
		},
		{
			name: "second rule empty destination",
			rules: []protocol.PathMapRule{
				{SourcePathFormat: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
				{SourcePathFormat: "s3://bucket/assets", DestinationPath: ""},
			},
			wantSrc: "s3://bucket/assets",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, err := pathmap.Parse(tc.rules)
			if err == nil {
				t.Fatal("Parse: expected error for empty DestinationPath; got nil")
			}
			if l != nil {
				t.Error("Parse: returned non-nil Lookup alongside error")
			}
			if tc.wantSrc != "" {
				if !strings.Contains(err.Error(), tc.wantSrc) {
					t.Errorf("error %q does not mention the offending source path %q", err.Error(), tc.wantSrc)
				}
			}
		})
	}
}

// ── Apply ─────────────────────────────────────────────────────────────────────

// TestParse_bothFieldsEmpty verifies that a rule with both SourcePathFormat
// and DestinationPath empty is accepted by Parse (the empty DestinationPath is
// not treated as an error because the source is also empty, meaning the rule
// carries no usable mapping) and that Apply skips it without modifying the
// input string.
func TestParse_bothFieldsEmpty(t *testing.T) {
	rules := []protocol.PathMapRule{
		{SourcePathFormat: "", DestinationPath: ""},
	}
	l, err := pathmap.Parse(rules)
	if err != nil {
		t.Fatalf("Parse: unexpected error for both-empty rule: %v", err)
	}
	// Apply should skip the empty-source rule and leave the string unchanged.
	input := "/nfs/projects/shot.ma"
	got := l.Apply(input)
	if got != input {
		t.Errorf("Apply(%q) = %q; want unchanged %q", input, got, input)
	}
}

// TestApply is the primary table-driven test for resolved-mode path
// substitution (task 63).
func TestApply(t *testing.T) {
	tests := []struct {
		name  string
		rules []protocol.PathMapRule
		input string
		want  string
	}{
		{
			name:  "no rules — input unchanged",
			rules: nil,
			input: "/nfs/projects/shot001/render.ma",
			want:  "/nfs/projects/shot001/render.ma",
		},
		{
			name: "simple prefix substitution",
			rules: []protocol.PathMapRule{
				{SourcePathFormat: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
			},
			input: "/nfs/projects/shot001/render.ma",
			want:  "/mnt/nas/projects/shot001/render.ma",
		},
		{
			name: "location appearing multiple times in one command",
			rules: []protocol.PathMapRule{
				{SourcePathFormat: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
			},
			input: "/nfs/projects/shot001/in.ma -output /nfs/projects/shot001/out/",
			want:  "/mnt/nas/projects/shot001/in.ma -output /mnt/nas/projects/shot001/out/",
		},
		{
			name: "location with no mapping — string unchanged",
			rules: []protocol.PathMapRule{
				{SourcePathFormat: "/nfs/renders", DestinationPath: "/mnt/nas/renders"},
			},
			input: "/nfs/projects/shot001/render.ma",
			want:  "/nfs/projects/shot001/render.ma",
		},
		{
			name: "multiple rules applied in order",
			rules: []protocol.PathMapRule{
				{SourcePathFormat: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
				{SourcePathFormat: "/nfs/renders", DestinationPath: "/mnt/nas/renders"},
			},
			input: "/nfs/projects/shot.ma -o /nfs/renders/out/shot.exr",
			want:  "/mnt/nas/projects/shot.ma -o /mnt/nas/renders/out/shot.exr",
		},
		{
			name: "S3 URI source path",
			rules: []protocol.PathMapRule{
				{SourcePathFormat: "s3://bucket/assets", DestinationPath: "/mnt/s3/assets"},
			},
			input: "s3://bucket/assets/scene.ma",
			want:  "/mnt/s3/assets/scene.ma",
		},
		{
			name: "empty source path format — skipped, no substitution",
			rules: []protocol.PathMapRule{
				{SourcePathFormat: "", DestinationPath: "/mnt/nas/projects"},
			},
			input: "some command /nfs/projects/shot.ma",
			want:  "some command /nfs/projects/shot.ma",
		},
		{
			name: "empty input string",
			rules: []protocol.PathMapRule{
				{SourcePathFormat: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
			},
			input: "",
			want:  "",
		},
		{
			name: "rules applied sequentially — earlier output can be matched by later rules",
			rules: []protocol.PathMapRule{
				{SourcePathFormat: "/a", DestinationPath: "/b"},
				{SourcePathFormat: "/b", DestinationPath: "/c"},
			},
			// Rules are applied in declaration order via strings.ReplaceAll.
			// /a → /b after rule 1; /b → /c after rule 2.  Callers should use
			// non-overlapping source paths to avoid unintended chaining.
			input: "/a/file.ma",
			want:  "/c/file.ma",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, err := pathmap.Parse(tc.rules)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			got := l.Apply(tc.input)
			if got != tc.want {
				t.Errorf("Apply(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestApply_nilLookup verifies that calling Apply on a nil *Lookup is safe.
func TestApply_nilLookup(t *testing.T) {
	var l *pathmap.Lookup
	got := l.Apply("/nfs/projects/shot.ma")
	want := "/nfs/projects/shot.ma"
	if got != want {
		t.Errorf("nil Lookup.Apply() = %q; want %q", got, want)
	}
}

// ── ApplyToAction ─────────────────────────────────────────────────────────────

// TestApplyToAction verifies that path substitution is applied to both
// Command and Args, and that the original action is not modified.
func TestApplyToAction(t *testing.T) {
	rules := []protocol.PathMapRule{
		{SourcePathFormat: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
	}
	l, err := pathmap.Parse(rules)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	orig := &protocol.Action{
		Command:        "/usr/bin/render",
		Args:           []string{"/nfs/projects/shot.ma", "-o", "/nfs/projects/out/"},
		TimeoutSeconds: 300,
	}
	got := l.ApplyToAction(orig)

	// Command unchanged (no source path prefix in it).
	if got.Command != "/usr/bin/render" {
		t.Errorf("Command = %q; want %q", got.Command, "/usr/bin/render")
	}

	// Args substituted.
	wantArgs := []string{"/mnt/nas/projects/shot.ma", "-o", "/mnt/nas/projects/out/"}
	if len(got.Args) != len(wantArgs) {
		t.Fatalf("Args len = %d; want %d", len(got.Args), len(wantArgs))
	}
	for i, w := range wantArgs {
		if got.Args[i] != w {
			t.Errorf("Args[%d] = %q; want %q", i, got.Args[i], w)
		}
	}

	// TimeoutSeconds preserved.
	if got.TimeoutSeconds != 300 {
		t.Errorf("TimeoutSeconds = %d; want 300", got.TimeoutSeconds)
	}

	// Original action not mutated.
	if orig.Args[0] != "/nfs/projects/shot.ma" {
		t.Errorf("original Args[0] was mutated to %q", orig.Args[0])
	}
}

// TestApplyToAction_commandSubstitution verifies that the Command field is
// also subject to substitution when it contains a source path.
func TestApplyToAction_commandSubstitution(t *testing.T) {
	rules := []protocol.PathMapRule{
		{SourcePathFormat: "/nfs/tools", DestinationPath: "/opt/dcc"},
	}
	l, err := pathmap.Parse(rules)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	orig := &protocol.Action{
		Command: "/nfs/tools/maya/bin/render",
		Args:    []string{"-scene", "/nfs/tools/scenes/shot.ma"},
	}
	got := l.ApplyToAction(orig)
	if got.Command != "/opt/dcc/maya/bin/render" {
		t.Errorf("Command = %q; want %q", got.Command, "/opt/dcc/maya/bin/render")
	}
	if got.Args[1] != "/opt/dcc/scenes/shot.ma" {
		t.Errorf("Args[1] = %q; want %q", got.Args[1], "/opt/dcc/scenes/shot.ma")
	}
}

// TestApplyToAction_nilAction verifies that nil action returns nil.
func TestApplyToAction_nilAction(t *testing.T) {
	rules := []protocol.PathMapRule{
		{SourcePathFormat: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
	}
	l, err := pathmap.Parse(rules)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := l.ApplyToAction(nil); got != nil {
		t.Errorf("ApplyToAction(nil) = %v; want nil", got)
	}
}

// TestApplyToAction_emptyRules verifies that ApplyToAction with no rules
// returns the original action pointer (no copy needed).
func TestApplyToAction_emptyRules(t *testing.T) {
	l, err := pathmap.Parse(nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	orig := &protocol.Action{Command: "/usr/bin/render", Args: []string{"/nfs/scene.ma"}}
	got := l.ApplyToAction(orig)
	if got != orig {
		t.Error("ApplyToAction with empty Lookup should return the original action pointer")
	}
}

// TestApplyToAction_noArgs verifies that an action with no Args is handled
// without panic.
func TestApplyToAction_noArgs(t *testing.T) {
	rules := []protocol.PathMapRule{
		{SourcePathFormat: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
	}
	l, err := pathmap.Parse(rules)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	orig := &protocol.Action{Command: "/nfs/projects/render"}
	got := l.ApplyToAction(orig)
	if got.Command != "/mnt/nas/projects/render" {
		t.Errorf("Command = %q; want %q", got.Command, "/mnt/nas/projects/render")
	}
	if len(got.Args) != 0 {
		t.Errorf("Args = %v; want []", got.Args)
	}
}

// ── WritePathMappingFile ──────────────────────────────────────────────────────

// TestWritePathMappingFile verifies that the JSON file is created with the
// correct content and that an empty rules slice produces no file (task 61).
func TestWritePathMappingFile(t *testing.T) {
	t.Run("writes file with correct content", func(t *testing.T) {
		dir := t.TempDir()
		rules := []protocol.PathMapRule{
			{SourcePathFormat: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
			{SourcePathFormat: "s3://bucket/assets", DestinationPath: "/mnt/s3/assets"},
		}
		if err := pathmap.WritePathMappingFile(dir, rules); err != nil {
			t.Fatalf("WritePathMappingFile: %v", err)
		}
		path := filepath.Join(dir, pathmap.PathMappingFileName)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read file: %v", err)
		}
		// Decode and verify the JSON structure.
		type entry struct {
			SourcePathFormat string `json:"source_path_format"`
			DestinationPath  string `json:"destination_path"`
		}
		var got []entry
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v — raw: %s", err, data)
		}
		if len(got) != 2 {
			t.Fatalf("len = %d; want 2", len(got))
		}
		if got[0].SourcePathFormat != "/nfs/projects" {
			t.Errorf("[0].source_path_format = %q; want %q", got[0].SourcePathFormat, "/nfs/projects")
		}
		if got[0].DestinationPath != "/mnt/nas/projects" {
			t.Errorf("[0].destination_path = %q; want %q", got[0].DestinationPath, "/mnt/nas/projects")
		}
		if got[1].SourcePathFormat != "s3://bucket/assets" {
			t.Errorf("[1].source_path_format = %q; want %q", got[1].SourcePathFormat, "s3://bucket/assets")
		}
		if got[1].DestinationPath != "/mnt/s3/assets" {
			t.Errorf("[1].destination_path = %q; want %q", got[1].DestinationPath, "/mnt/s3/assets")
		}
	})

	t.Run("empty rules produces no file", func(t *testing.T) {
		dir := t.TempDir()
		if err := pathmap.WritePathMappingFile(dir, nil); err != nil {
			t.Fatalf("WritePathMappingFile(nil): %v", err)
		}
		path := filepath.Join(dir, pathmap.PathMappingFileName)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Error("expected no file to be written for empty rules; file exists")
		}
	})

	t.Run("single rule", func(t *testing.T) {
		dir := t.TempDir()
		rules := []protocol.PathMapRule{
			{SourcePathFormat: "/source", DestinationPath: "/dest"},
		}
		if err := pathmap.WritePathMappingFile(dir, rules); err != nil {
			t.Fatalf("WritePathMappingFile: %v", err)
		}
		path := filepath.Join(dir, pathmap.PathMappingFileName)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("file not created: %v", err)
		}
	})

	t.Run("invalid directory returns error", func(t *testing.T) {
		rules := []protocol.PathMapRule{
			{SourcePathFormat: "/src", DestinationPath: "/dst"},
		}
		err := pathmap.WritePathMappingFile("/nonexistent/no/such/dir", rules)
		if err == nil {
			t.Error("expected error writing to nonexistent directory; got nil")
		}
	})
}
