// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

func TestMapPath(t *testing.T) {
	tests := []struct {
		name  string
		s     string
		rules []PathMapRule
		dst   PathFormat
		want  string
	}{
		{
			name:  "posix prefix maps and transfers the remainder",
			s:     "/projects/shot01/render.exr",
			rules: []PathMapRule{{PathMapPOSIX, "/projects", "/mnt/projects"}},
			dst:   PathPOSIX,
			want:  "/mnt/projects/shot01/render.exr",
		},
		{
			name:  "windows source matches case-insensitively, remainder re-expressed in posix dst",
			s:     "C:/studio/project/scene.ma",
			rules: []PathMapRule{{PathMapWindows, `C:\studio`, "/mnt/studio"}},
			dst:   PathPOSIX,
			want:  "/mnt/studio/project/scene.ma",
		},
		{
			name:  "windows source matches a different-case drive and directory",
			s:     "c:/STUDIO/project/scene.ma",
			rules: []PathMapRule{{PathMapWindows, `C:\studio`, "/mnt/studio"}},
			dst:   PathPOSIX,
			want:  "/mnt/studio/project/scene.ma",
		},
		{
			name:  "windows source is separator-insensitive (forward slash input)",
			s:     `C:\studio\project`,
			rules: []PathMapRule{{PathMapWindows, "C:/studio", "/mnt/studio"}},
			dst:   PathPOSIX,
			want:  "/mnt/studio/project",
		},
		{
			name:  "uri prefix maps on a path boundary with no normalization",
			s:     "s3://bucket/assets/tex/wood.png",
			rules: []PathMapRule{{PathMapURI, "s3://bucket/assets", "/mnt/assets"}},
			dst:   PathPOSIX,
			want:  "/mnt/assets/tex/wood.png",
		},
		{
			name:  "uri boundary non-match falls through to passthrough",
			s:     "s3://bucket/assets2/tex.png",
			rules: []PathMapRule{{PathMapURI, "s3://bucket/assets", "/mnt/assets"}},
			dst:   PathPOSIX,
			want:  "s3://bucket/assets2/tex.png",
		},
		{
			name: "longest source path wins and processing stops at first match",
			s:    "/a/b/c/file",
			rules: []PathMapRule{
				{PathMapPOSIX, "/a", "/short"},
				{PathMapPOSIX, "/a/b", "/long"},
			},
			dst:  PathPOSIX,
			want: "/long/c/file",
		},
		{
			name:  "component-boundary non-match passes through",
			s:     "/foobar/x",
			rules: []PathMapRule{{PathMapPOSIX, "/foo", "/bar"}},
			dst:   PathPOSIX,
			want:  "/foobar/x",
		},
		{
			name:  "no rule matches: passthrough verbatim",
			s:     "/other/x",
			rules: []PathMapRule{{PathMapPOSIX, "/projects", "/mnt"}},
			dst:   PathPOSIX,
			want:  "/other/x",
		},
		{
			name:  "nil rules: passthrough",
			s:     "/anything",
			rules: nil,
			dst:   PathPOSIX,
			want:  "/anything",
		},
		{
			name:  "empty source path rule is skipped",
			s:     "/x",
			rules: []PathMapRule{{PathMapPOSIX, "", "/dest"}},
			dst:   PathPOSIX,
			want:  "/x",
		},
		{
			// Two equal-length rules, so mapPath's stable sort leaves them in
			// this order. The first rule's own destination "/b" is itself a
			// prefix the SECOND rule's source would match — so an
			// implementation that kept scanning and applying every matching
			// rule (rather than stopping at the first) would additionally
			// rewrite "/b/x" to "/c/x". First-match-wins must stop after
			// the first rule fires.
			name: "processing stops at the first match: a later rule must not re-apply to the result",
			s:    "/a/x",
			rules: []PathMapRule{
				{PathMapPOSIX, "/a", "/b"},
				{PathMapPOSIX, "/b", "/c"},
			},
			dst:  PathPOSIX,
			want: "/b/x",
		},
		{
			// Windows sources match case-insensitively (see the other cases
			// above); POSIX ones do not. A comparator that dropped that
			// distinction (e.g. strings.EqualFold unconditionally) would
			// match "/Projects" against "/projects/x" and this would fail.
			name:  "posix source matching is case-sensitive: a differently-cased source does not match",
			s:     "/projects/x",
			rules: []PathMapRule{{PathMapPOSIX, "/Projects", "/mnt"}},
			dst:   PathPOSIX,
			want:  "/projects/x",
		},
		{
			// dst governs how the DESTINATION and the transferred remainder
			// are re-expressed, independent of the source's own flavor — a
			// POSIX source can map into a Windows destination. Every other
			// case in this table uses dst: PathPOSIX, which would let a bug
			// that ignored dst entirely (e.g. always joining with "/") pass
			// unnoticed.
			name:  "destination and remainder are re-expressed in a windows dst",
			s:     "/projects/shot01/render.exr",
			rules: []PathMapRule{{PathMapPOSIX, "/projects", `Z:\renders`}},
			dst:   PathWindows,
			want:  `Z:\renders\shot01\render.exr`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapPath(tc.s, tc.rules, tc.dst); got != tc.want {
				t.Errorf("mapPath(%q, ...) = %q, want %q", tc.s, got, tc.want)
			}
		})
	}
}

func TestApplyPathMapping_PassthroughWithoutRules(t *testing.T) {
	// No WithPathMapping option: nil rules, so the input passes through as a path.
	// This is the behavior RunExprCase relies on (design doc §6).
	v, err := Eval(`apply_path_mapping('/mnt/share')`, MapSymbols{}, TAny)
	if err != nil {
		t.Fatalf("Eval failed: %v", err)
	}
	if got := v.String(); got != "/mnt/share" {
		t.Errorf("passthrough = %q, want %q", got, "/mnt/share")
	}
	if got := v.Type.String(); got != "path" {
		t.Errorf("result type = %q, want path", got)
	}
}

func TestApplyPathMapping_FunctionForm(t *testing.T) {
	rules := []PathMapRule{{PathMapPOSIX, "/projects", "/mnt/projects"}}
	v, err := Eval(`apply_path_mapping('/projects/shot01/out.exr')`, MapSymbols{}, TAny, WithPathMapping(rules))
	if err != nil {
		t.Fatalf("Eval failed: %v", err)
	}
	if got := v.String(); got != "/mnt/projects/shot01/out.exr" {
		t.Errorf("function form = %q, want %q", got, "/mnt/projects/shot01/out.exr")
	}
}

func TestApplyPathMapping_MethodForm(t *testing.T) {
	rules := []PathMapRule{{PathMapPOSIX, "/projects", "/mnt/projects"}}
	v, err := Eval(`'/projects/shot01/out.exr'.apply_path_mapping()`, MapSymbols{}, TAny, WithPathMapping(rules))
	if err != nil {
		t.Fatalf("Eval failed: %v", err)
	}
	if got := v.String(); got != "/mnt/projects/shot01/out.exr" {
		t.Errorf("method form = %q, want %q", got, "/mnt/projects/shot01/out.exr")
	}
}

func TestApplyPathMapping_WindowsSourceCaseInsensitive(t *testing.T) {
	// The rule's SourcePath is a Go string, so backslashes are literal; the
	// expression input uses forward slashes, which the Windows parser accepts.
	rules := []PathMapRule{{PathMapWindows, `C:\studio`, "/mnt/studio"}}
	v, err := Eval(`apply_path_mapping('c:/STUDIO/project/scene.ma')`, MapSymbols{}, TAny, WithPathMapping(rules))
	if err != nil {
		t.Fatalf("Eval failed: %v", err)
	}
	if got := v.String(); got != "/mnt/studio/project/scene.ma" {
		t.Errorf("windows map = %q, want %q", got, "/mnt/studio/project/scene.ma")
	}
}

// TestApplyPathMapping_PassthroughNormalizes pins what "passthrough" means: no
// RULE rewrote the text, but the result is still re-parsed by boundedPath in the
// evaluation's flavor, so a non-normal input does not come back verbatim. The
// vendored fixture cases below depend on this (expr2.3.2--apply-path-mapping's
// output_windows expects an unmatched POSIX-shaped input back with backslashes),
// which is why it is pinned separately rather than left implicit.
func TestApplyPathMapping_PassthroughNormalizes(t *testing.T) {
	tests := []struct {
		name   string
		src    string
		format PathFormat
		want   string
	}{
		{"redundant and trailing separators collapse", `apply_path_mapping('/a//b/')`, PathPOSIX, "/a/b"},
		{"a posix-shaped input is re-expressed in a windows evaluation", `apply_path_mapping('/a/b')`, PathWindows, `\a\b`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny, WithPathFormat(tc.format))
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// TestMapPath_ReturnsRawTextOnTheURIAndPassthroughArms pins the half of mapPath's
// contract that its doc comment has to disclaim: only applyFileRule rebuilds its
// result in dst. A URI rule and the no-match passthrough hand back raw text, so
// dst is ignored on those two arms and it is the apply_path_mapping wrapper's
// boundedPath call — not mapPath — that makes the composite "a path in dst" true.
func TestMapPath_ReturnsRawTextOnTheURIAndPassthroughArms(t *testing.T) {
	uri := mapPath("s3://b/x", []PathMapRule{{PathMapURI, "s3://b", `E:\dst`}}, PathWindows)
	if want := `E:\dst/x`; uri != want {
		t.Errorf("uri arm = %q, want %q (raw, separator untouched)", uri, want)
	}
	if through := mapPath("/a//b/", nil, PathWindows); through != "/a//b/" {
		t.Errorf("passthrough arm = %q, want %q (raw, unnormalized)", through, "/a//b/")
	}
}

// TestApplyPathMapping_ResultChainsButAPathReceiverIsRefused pins the two claims
// doc.go makes about the function's signature — string in, path out — meeting the
// section 1.2.4 rule that a method RECEIVER is not coerced.
func TestApplyPathMapping_ResultChainsButAPathReceiverIsRefused(t *testing.T) {
	t.Run("the path result chains into a path property", func(t *testing.T) {
		v, err := Eval(`apply_path_mapping('/projects/shot01/out.exr').stem`, MapSymbols{}, TAny)
		if err != nil {
			t.Fatalf("Eval failed: %v", err)
		}
		if got := v.String(); got != "out" {
			t.Errorf("stem = %q, want %q", got, "out")
		}
	})
	t.Run("a path receiver is not coerced to the string parameter", func(t *testing.T) {
		_, err := Eval(`path('/projects/a').apply_path_mapping()`, MapSymbols{}, TAny)
		if err == nil {
			t.Fatal("a path receiver was accepted; section 1.2.4 forbids coercing one")
		}
		const want = `no signature of "apply_path_mapping" accepts (path)`
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err.Error(), want)
		}
	})
}

// TestApplyPathMapping_VendoredFixtureExpectations transcribes the OpenJD
// conformance suite's own hand-authored expectations for this function. The
// three fixtures are
//
//	third_party/openjd-specifications/conformance-tests/2023-09/EXPR/jobs/
//	    expr2.3.2--apply-path-mapping.test.yaml
//	    expr2.3.2--uri-source-path-mapping-posix.test.yaml
//	    expr2.3.2--uri-source-path-mapping-windows.test.yaml
//
// and every want below is copied from their expected.output / output_posix /
// output_windows blocks, with the "LABEL:" prefix the fixture's python print()
// adds stripped. They are SPECIFICATION-AUTHORED ground truth, not invented
// expectations — which matters because apply_path_mapping has no oracle
// coverage (see doc.go), so this table is the closest thing the function has to
// an external reference.
//
// They are transcribed rather than executed because test/conformance scores only
// EXPR/job_templates and env_templates (test/conformance/classify.go), not
// EXPR/jobs, which needs a real session and a real python subprocess. Left in
// the fixture tree they are inert; here they run.
//
// Each fixture's pathMapping block becomes the rules, and its runOn/output block
// the format: output_posix under PathPOSIX, output_windows under PathWindows, and
// a flavor-independent output block under both.
func TestApplyPathMapping_VendoredFixtureExpectations(t *testing.T) {
	// expr2.3.2--apply-path-mapping.test.yaml
	fileRules := []PathMapRule{
		{PathMapPOSIX, "/mnt/shared", "/local/cache"},
		{PathMapWindows, `D:\assets`, "/data/assets"},
	}
	// expr2.3.2--uri-source-path-mapping-posix.test.yaml
	uriPOSIXRules := []PathMapRule{
		{PathMapURI, "s3://my-bucket/assets", "/local/assets"},
		{PathMapURI, "https://cdn.example.com/models", "/cache/models"},
		{PathMapPOSIX, "/mnt/shared", "/local/shared"},
	}
	// expr2.3.2--uri-source-path-mapping-windows.test.yaml
	uriWindowsRules := []PathMapRule{
		{PathMapURI, "s3://my-bucket/assets", `E:\local\assets`},
		{PathMapURI, "https://cdn.example.com/models", `E:\cache\models`},
		{PathMapWindows, `D:\shared`, `E:\local\shared`},
	}

	tests := []struct {
		name   string
		rules  []PathMapRule
		in     string
		format PathFormat
		want   string
	}{
		// --- expr2.3.2--apply-path-mapping.test.yaml, expected.output_posix ---
		{"file/posix POSIX_MAPPED", fileRules, "/mnt/shared/project/scene.exr", PathPOSIX, "/local/cache/project/scene.exr"},
		{"file/posix POSIX_UNMAPPED", fileRules, "/other/path/file.txt", PathPOSIX, "/other/path/file.txt"},
		{"file/posix WIN_MAPPED", fileRules, `D:\assets\textures\wood.png`, PathPOSIX, "/data/assets/textures/wood.png"},
		//nolint:misspell // "\other" is a fixture path segment, not the word "there"
		{"file/posix WIN_UNMAPPED", fileRules, `E:\other\file.txt`, PathPOSIX, `E:\other\file.txt`},
		// --- expr2.3.2--apply-path-mapping.test.yaml, expected.output_windows ---
		{"file/windows POSIX_MAPPED", fileRules, "/mnt/shared/project/scene.exr", PathWindows, `\local\cache\project\scene.exr`},
		//nolint:misspell // "\other" is a fixture path segment, not the word "there"
		{"file/windows POSIX_UNMAPPED", fileRules, "/other/path/file.txt", PathWindows, `\other\path\file.txt`},
		{"file/windows WIN_MAPPED", fileRules, `D:\assets\textures\wood.png`, PathWindows, `\data\assets\textures\wood.png`},
		//nolint:misspell // "\other" is a fixture path segment, not the word "there"
		{"file/windows WIN_UNMAPPED", fileRules, `E:\other\file.txt`, PathWindows, `E:\other\file.txt`},

		// --- uri-source-path-mapping-posix.test.yaml, expected.output (flavor-independent) ---
		{"uri-posix/posix S3_NOMATCH_BUCKET", uriPOSIXRules, "s3://other-bucket/assets/file.obj", PathPOSIX, "s3://other-bucket/assets/file.obj"},
		{"uri-posix/windows S3_NOMATCH_BUCKET", uriPOSIXRules, "s3://other-bucket/assets/file.obj", PathWindows, "s3://other-bucket/assets/file.obj"},
		{"uri-posix/posix S3_NOMATCH_PREFIX", uriPOSIXRules, "s3://my-bucket/assets2/file.obj", PathPOSIX, "s3://my-bucket/assets2/file.obj"},
		{"uri-posix/windows S3_NOMATCH_PREFIX", uriPOSIXRules, "s3://my-bucket/assets2/file.obj", PathWindows, "s3://my-bucket/assets2/file.obj"},
		{"uri-posix/posix HTTPS_NOMATCH", uriPOSIXRules, "https://other.com/models/scene.obj", PathPOSIX, "https://other.com/models/scene.obj"},
		{"uri-posix/windows HTTPS_NOMATCH", uriPOSIXRules, "https://other.com/models/scene.obj", PathWindows, "https://other.com/models/scene.obj"},
		{"uri-posix/posix UNMAPPED", uriPOSIXRules, "fsx://vol/data/file.bin", PathPOSIX, "fsx://vol/data/file.bin"},
		{"uri-posix/windows UNMAPPED", uriPOSIXRules, "fsx://vol/data/file.bin", PathWindows, "fsx://vol/data/file.bin"},
		// --- uri-source-path-mapping-posix.test.yaml, expected.output_posix ---
		{"uri-posix/posix S3_MATCH", uriPOSIXRules, "s3://my-bucket/assets/teapot.obj", PathPOSIX, "/local/assets/teapot.obj"},
		{"uri-posix/posix S3_EXACT", uriPOSIXRules, "s3://my-bucket/assets", PathPOSIX, "/local/assets"},
		{"uri-posix/posix S3_NESTED", uriPOSIXRules, "s3://my-bucket/assets/sub/dir/file.exr", PathPOSIX, "/local/assets/sub/dir/file.exr"},
		{"uri-posix/posix HTTPS_MATCH", uriPOSIXRules, "https://cdn.example.com/models/scene.obj", PathPOSIX, "/cache/models/scene.obj"},
		{"uri-posix/posix POSIX_STILL_WORKS", uriPOSIXRules, "/mnt/shared/project/file.exr", PathPOSIX, "/local/shared/project/file.exr"},
		// --- uri-source-path-mapping-posix.test.yaml, expected.output_windows ---
		{"uri-posix/windows S3_MATCH", uriPOSIXRules, "s3://my-bucket/assets/teapot.obj", PathWindows, `\local\assets\teapot.obj`},
		{"uri-posix/windows S3_EXACT", uriPOSIXRules, "s3://my-bucket/assets", PathWindows, `\local\assets`},
		{"uri-posix/windows S3_NESTED", uriPOSIXRules, "s3://my-bucket/assets/sub/dir/file.exr", PathWindows, `\local\assets\sub\dir\file.exr`},
		{"uri-posix/windows HTTPS_MATCH", uriPOSIXRules, "https://cdn.example.com/models/scene.obj", PathWindows, `\cache\models\scene.obj`},
		{"uri-posix/windows POSIX_STILL_WORKS", uriPOSIXRules, "/mnt/shared/project/file.exr", PathWindows, `\local\shared\project\file.exr`},

		// --- uri-source-path-mapping-windows.test.yaml, expected.output (runOn: [windows]) ---
		{"uri-windows S3_MATCH", uriWindowsRules, "s3://my-bucket/assets/teapot.obj", PathWindows, `E:\local\assets\teapot.obj`},
		{"uri-windows S3_EXACT", uriWindowsRules, "s3://my-bucket/assets", PathWindows, `E:\local\assets`},
		{"uri-windows S3_NESTED", uriWindowsRules, "s3://my-bucket/assets/sub/dir/file.exr", PathWindows, `E:\local\assets\sub\dir\file.exr`},
		{"uri-windows S3_NOMATCH_BUCKET", uriWindowsRules, "s3://other-bucket/assets/file.obj", PathWindows, "s3://other-bucket/assets/file.obj"},
		{"uri-windows S3_NOMATCH_PREFIX", uriWindowsRules, "s3://my-bucket/assets2/file.obj", PathWindows, "s3://my-bucket/assets2/file.obj"},
		{"uri-windows HTTPS_MATCH", uriWindowsRules, "https://cdn.example.com/models/scene.obj", PathWindows, `E:\cache\models\scene.obj`},
		{"uri-windows HTTPS_NOMATCH", uriWindowsRules, "https://other.com/models/scene.obj", PathWindows, "https://other.com/models/scene.obj"},
		{"uri-windows WINDOWS_STILL_WORKS", uriWindowsRules, `D:\shared\project\file.exr`, PathWindows, `E:\local\shared\project\file.exr`},
		{"uri-windows UNMAPPED", uriWindowsRules, "fsx://vol/data/file.bin", PathWindows, "fsx://vol/data/file.bin"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The input is bound as a symbol rather than written as a string
			// literal so that a fixture's backslashes reach the evaluator
			// exactly as the fixture's python raw string delivers them, with no
			// second layer of EXPR escaping to get wrong in transcription.
			syms := MapSymbols{"Param.S": String(tc.in)}
			v, err := Eval(`apply_path_mapping(Param.S)`, syms, TAny,
				WithPathMapping(tc.rules), WithPathFormat(tc.format))
			if err != nil {
				t.Fatalf("Eval for input %q failed: %v", tc.in, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("apply_path_mapping(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestApplyPathMapping_UnresolvedArgumentPropagates(t *testing.T) {
	// An unresolved argument short-circuits before the function runs (call.go),
	// so the result is unresolved[path], never a mapped value.
	syms := MapSymbols{"Param.U": Unresolved(TString)}
	rules := []PathMapRule{{PathMapPOSIX, "/projects", "/mnt/projects"}}
	v, err := Eval(`apply_path_mapping(Param.U)`, syms, TAny, WithPathMapping(rules))
	if err != nil {
		t.Fatalf("Eval failed: %v", err)
	}
	if got := v.Type.String(); got != "unresolved[path]" {
		t.Errorf("unresolved arg gave type %q, want unresolved[path]", got)
	}
}
