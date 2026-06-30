// SPDX-License-Identifier: AGPL-3.0-or-later

package pathmap_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
		{SourcePath: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
		{SourcePath: "s3://bucket/assets", DestinationPath: "/mnt/s3/assets"},
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
// source path.
func TestParse_emptyDestinationPath(t *testing.T) {
	tests := []struct {
		name    string
		rules   []protocol.PathMapRule
		wantSrc string // expected to appear in the error message
	}{
		{
			name: "single empty destination",
			rules: []protocol.PathMapRule{
				{SourcePath: "/nfs/projects", DestinationPath: ""},
			},
			wantSrc: "/nfs/projects",
		},
		{
			name: "second rule empty destination",
			rules: []protocol.PathMapRule{
				{SourcePath: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
				{SourcePath: "s3://bucket/assets", DestinationPath: ""},
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

// TestParse_validatesSourcePathNotFormat verifies that validation keys on
// SourcePath (the new field), not SourcePathFormat: a rule that carries only a
// format enum but no SourcePath is a no-op and must not error, while a rule with
// a SourcePath but no DestinationPath must error and name the SourcePath.
func TestParse_validatesSourcePathNotFormat(t *testing.T) {
	t.Run("format set but empty source path is accepted", func(t *testing.T) {
		// A format enum without a source path carries no mapping; must not error.
		rules := []protocol.PathMapRule{
			{SourcePathFormat: "POSIX", SourcePath: "", DestinationPath: ""},
		}
		if _, err := pathmap.Parse(rules); err != nil {
			t.Fatalf("Parse: unexpected error for format-only rule: %v", err)
		}
	})

	t.Run("source path without destination errors naming the source", func(t *testing.T) {
		rules := []protocol.PathMapRule{
			{SourcePathFormat: "POSIX", SourcePath: "/nfs/projects", DestinationPath: ""},
		}
		_, err := pathmap.Parse(rules)
		if err == nil {
			t.Fatal("Parse: expected error for source path with empty destination; got nil")
		}
		if !strings.Contains(err.Error(), "/nfs/projects") {
			t.Errorf("error %q does not name the offending source path %q", err.Error(), "/nfs/projects")
		}
		if strings.Contains(err.Error(), "POSIX") {
			t.Errorf("error %q should not reference the format enum %q", err.Error(), "POSIX")
		}
	})
}

// TestApply_keysOnSourcePathNotFormat verifies that substitution replaces the
// SourcePath value and ignores SourcePathFormat entirely (the format enum must
// never be substituted into the command string).
func TestApply_keysOnSourcePathNotFormat(t *testing.T) {
	rules := []protocol.PathMapRule{
		{SourcePathFormat: "POSIX", SourcePath: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
	}
	l, err := pathmap.Parse(rules)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := l.Apply("/nfs/projects/shot.ma")
	if got != "/mnt/nas/projects/shot.ma" {
		t.Errorf("Apply = %q; want %q", got, "/mnt/nas/projects/shot.ma")
	}
	// The format enum "POSIX" must not be substituted.
	if got := l.Apply("POSIX-literal"); got != "POSIX-literal" {
		t.Errorf("Apply(%q) = %q; want it unchanged (format enum must not match)", "POSIX-literal", got)
	}
}

// ── Apply ─────────────────────────────────────────────────────────────────────

// TestParse_bothFieldsEmpty verifies that a rule with both SourcePath
// and DestinationPath empty is accepted by Parse (the empty DestinationPath is
// not treated as an error because the source is also empty, meaning the rule
// carries no usable mapping) and that Apply skips it without modifying the
// input string.
func TestParse_bothFieldsEmpty(t *testing.T) {
	rules := []protocol.PathMapRule{
		{SourcePath: "", DestinationPath: ""},
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
// substitution.
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
				{SourcePath: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
			},
			input: "/nfs/projects/shot001/render.ma",
			want:  "/mnt/nas/projects/shot001/render.ma",
		},
		{
			name: "location appearing multiple times in one command",
			rules: []protocol.PathMapRule{
				{SourcePath: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
			},
			input: "/nfs/projects/shot001/in.ma -output /nfs/projects/shot001/out/",
			want:  "/mnt/nas/projects/shot001/in.ma -output /mnt/nas/projects/shot001/out/",
		},
		{
			name: "location with no mapping — string unchanged",
			rules: []protocol.PathMapRule{
				{SourcePath: "/nfs/renders", DestinationPath: "/mnt/nas/renders"},
			},
			input: "/nfs/projects/shot001/render.ma",
			want:  "/nfs/projects/shot001/render.ma",
		},
		{
			name: "multiple rules applied in order",
			rules: []protocol.PathMapRule{
				{SourcePath: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
				{SourcePath: "/nfs/renders", DestinationPath: "/mnt/nas/renders"},
			},
			input: "/nfs/projects/shot.ma -o /nfs/renders/out/shot.exr",
			want:  "/mnt/nas/projects/shot.ma -o /mnt/nas/renders/out/shot.exr",
		},
		{
			name: "S3 URI source path",
			rules: []protocol.PathMapRule{
				{SourcePath: "s3://bucket/assets", DestinationPath: "/mnt/s3/assets"},
			},
			input: "s3://bucket/assets/scene.ma",
			want:  "/mnt/s3/assets/scene.ma",
		},
		{
			name: "empty source path format — skipped, no substitution",
			rules: []protocol.PathMapRule{
				{SourcePath: "", DestinationPath: "/mnt/nas/projects"},
			},
			input: "some command /nfs/projects/shot.ma",
			want:  "some command /nfs/projects/shot.ma",
		},
		{
			name: "empty input string",
			rules: []protocol.PathMapRule{
				{SourcePath: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
			},
			input: "",
			want:  "",
		},
		{
			name: "rules applied sequentially — earlier output can be matched by later rules",
			rules: []protocol.PathMapRule{
				{SourcePath: "/a", DestinationPath: "/b"},
				{SourcePath: "/b", DestinationPath: "/c"},
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
		{SourcePath: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
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
		{SourcePath: "/nfs/tools", DestinationPath: "/opt/dcc"},
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
		{SourcePath: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
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
		{SourcePath: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
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
// correct content and that an empty rules slice produces no file.
func TestWritePathMappingFile(t *testing.T) {
	t.Run("writes conformant pathmapping-1.0 envelope", func(t *testing.T) {
		dir := t.TempDir()
		rules := []protocol.PathMapRule{
			{SourcePathFormat: "POSIX", SourcePath: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
			{SourcePathFormat: "WINDOWS", SourcePath: `C:\assets`, DestinationPath: "/mnt/s3/assets"},
		}
		if err := pathmap.WritePathMappingFile(dir, rules); err != nil {
			t.Fatalf("WritePathMappingFile: %v", err)
		}
		path := filepath.Join(dir, pathmap.PathMappingFileName)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read file: %v", err)
		}
		// Decode and verify the OpenJD pathmapping-1.0 envelope: a top-level
		// object with "version" and "path_mapping_rules".
		type entry struct {
			SourcePathFormat string `json:"source_path_format"`
			SourcePath       string `json:"source_path"`
			DestinationPath  string `json:"destination_path"`
		}
		type doc struct {
			Version          string  `json:"version"`
			PathMappingRules []entry `json:"path_mapping_rules"`
		}
		var got doc
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v — raw: %s", err, data)
		}
		if got.Version != pathmap.PathMappingVersion {
			t.Errorf("version = %q; want %q", got.Version, pathmap.PathMappingVersion)
		}
		if got.Version != "pathmapping-1.0" {
			t.Errorf("version = %q; want literal %q", got.Version, "pathmapping-1.0")
		}
		if len(got.PathMappingRules) != 2 {
			t.Fatalf("rules len = %d; want 2", len(got.PathMappingRules))
		}
		want := []entry{
			{SourcePathFormat: "POSIX", SourcePath: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
			{SourcePathFormat: "WINDOWS", SourcePath: `C:\assets`, DestinationPath: "/mnt/s3/assets"},
		}
		for i, w := range want {
			g := got.PathMappingRules[i]
			if g != w {
				t.Errorf("rule[%d] = %+v; want %+v", i, g, w)
			}
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
			{SourcePath: "/source", DestinationPath: "/dest"},
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
			{SourcePath: "/src", DestinationPath: "/dst"},
		}
		err := pathmap.WritePathMappingFile("/nonexistent/no/such/dir", rules)
		if err == nil {
			t.Error("expected error writing to nonexistent directory; got nil")
		}
	})
}

// ── CommandFlags ──────────────────────────────────────────────────────────────

func TestLookup_CommandFlags(t *testing.T) {
	l, err := pathmap.NewLookup([]protocol.PathMapRule{
		{SourcePath: "/projects", DestinationPath: "/mnt/cloud"},
		{SourcePath: "/assets", DestinationPath: "/mnt/assets"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := l.CommandFlags("--remap {src}={dest}")
	want := []string{"--remap /projects=/mnt/cloud", "--remap /assets=/mnt/assets"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CommandFlags = %v, want %v", got, want)
	}
}

func TestLookup_CommandFlags_SkipsEmptySource(t *testing.T) {
	l, err := pathmap.NewLookup([]protocol.PathMapRule{{SourcePath: "", DestinationPath: "/x"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := l.CommandFlags("{src}={dest}"); len(got) != 0 {
		t.Errorf("CommandFlags = %v, want empty", got)
	}
}

// ── EnvValue ──────────────────────────────────────────────────────────────────

func TestLookup_EnvValue(t *testing.T) {
	l, err := pathmap.NewLookup([]protocol.PathMapRule{
		{SourcePath: "/projects", DestinationPath: "/mnt/cloud"},
		{SourcePath: "/assets", DestinationPath: "/mnt/assets"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "/projects=/mnt/cloud" + string(os.PathListSeparator) + "/assets=/mnt/assets"
	if got := l.EnvValue(); got != want {
		t.Errorf("EnvValue = %q, want %q", got, want)
	}
}
