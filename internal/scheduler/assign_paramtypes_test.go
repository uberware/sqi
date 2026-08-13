// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// resolve's relPath argument includes the leading slash (or is empty for a
// bare "loc://name" reference) -- see [openjd.ResolveLocURIs]'s doc comment.
// The fixtures below trim it before joining so the expected "/mnt/name/rel"
// and `C:\name\rel` forms come out with exactly one separator.

// TestResolveLocURIsInParamValue_Scalar pins that the scalar path is unchanged:
// substring replacement, exactly as before sub-project F2.
func TestResolveLocURIsInParamValue_Scalar(t *testing.T) {
	resolve := func(name, rel string) (string, error) {
		return "/mnt/" + name + "/" + strings.TrimPrefix(rel, "/"), nil
	}

	got, err := resolveLocURIsInParamValue(
		"loc://storage/shot.ma", string(openjd.JobParamTypePath), resolve,
	)
	if err != nil {
		t.Fatalf("resolveLocURIsInParamValue: %v", err)
	}
	if want := "/mnt/storage/shot.ma"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestResolveLocURIsInParamValue_ListIsElementWise is the point of this task.
// Splicing a resolved path into canonical JSON by substring replacement
// produces valid-looking output for a POSIX path and CORRUPT output for a
// Windows one, because a backslash is not a legal JSON escape. Resolving
// element-wise and re-encoding is what makes both safe.
func TestResolveLocURIsInParamValue_ListIsElementWise(t *testing.T) {
	tests := []struct {
		name    string
		resolve func(string, string) (string, error)
		want    []string
	}{
		{
			"posix paths",
			func(name, rel string) (string, error) {
				return "/mnt/" + name + "/" + strings.TrimPrefix(rel, "/"), nil
			},
			[]string{"/mnt/storage/a.ma", "/mnt/storage/b.ma"},
		},
		{
			"windows paths, which substring splicing would corrupt",
			func(name, rel string) (string, error) {
				return `C:\shots\` + name + `\` + strings.TrimPrefix(rel, "/"), nil
			},
			[]string{`C:\shots\storage\a.ma`, `C:\shots\storage\b.ma`},
		},
	}

	const in = `["loc://storage/a.ma","loc://storage/b.ma"]`
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveLocURIsInParamValue(
				in, string(openjd.JobParamTypeListPath), tt.resolve,
			)
			if err != nil {
				t.Fatalf("resolveLocURIsInParamValue: %v", err)
			}

			var elems []string
			if err := json.Unmarshal([]byte(got), &elems); err != nil {
				t.Fatalf("result %q is not valid JSON: %v\n"+
					"This is the corruption the task exists to prevent: a resolved "+
					"path spliced into a JSON string literal.", got, err)
			}
			if len(elems) != len(tt.want) {
				t.Fatalf("got %d elements, want %d", len(elems), len(tt.want))
			}
			for i := range elems {
				if elems[i] != tt.want[i] {
					t.Errorf("element %d = %q, want %q", i, elems[i], tt.want[i])
				}
			}
		})
	}
}

// TestResolveLocURIsInParamValue_ListWithoutLocIsUnchanged pins that a list
// carrying no loc:// URI round-trips to the same canonical text, so an ordinary
// list value is not silently re-encoded into a different-but-equivalent form.
func TestResolveLocURIsInParamValue_ListWithoutLocIsUnchanged(t *testing.T) {
	resolve := func(string, string) (string, error) {
		t.Fatal("resolve must not be called when no loc:// URI is present")
		return "", nil
	}
	const in = `["/already/absolute","/also/absolute"]`
	got, err := resolveLocURIsInParamValue(in, string(openjd.JobParamTypeListPath), resolve)
	if err != nil {
		t.Fatalf("resolveLocURIsInParamValue: %v", err)
	}
	if got != in {
		t.Errorf("got %q, want %q unchanged", got, in)
	}
}

// TestResolveLocURIsInParamValue_MalformedListIsAnError pins that a list value
// that does not decode is reported rather than passed through: it reached here
// through BindJobParameters, which validates it, so a malformed one means
// something upstream is wrong and silence would hide it.
func TestResolveLocURIsInParamValue_MalformedListIsAnError(t *testing.T) {
	resolve := func(string, string) (string, error) { return "/x", nil }
	if _, err := resolveLocURIsInParamValue(
		`["a"`, string(openjd.JobParamTypeListPath), resolve,
	); err == nil {
		t.Error("a malformed list value was accepted")
	} else if !strings.Contains(err.Error(), "list") {
		t.Errorf("error %q does not say the value was a list", err)
	}
}

// TestBuildStagingManifest_ListPath pins that a LIST[PATH] with a dataFlow
// direction produces one StageEntry PER ELEMENT. Skipping it entirely — which
// is what a PATH-only check does — means a job declaring its inputs as a list
// stages none of them, silently.
func TestBuildStagingManifest_ListPath(t *testing.T) {
	tmpl := &openjd.JobTemplate{
		ParameterDefinitions: []openjd.JobParameter{
			{
				Name: "Scene", Type: openjd.JobParamTypePath,
				DataFlow: openjd.PathDataFlowIn, ObjectType: openjd.PathObjectTypeFile,
			},
			{
				Name: "Textures", Type: openjd.JobParamTypeListPath,
				DataFlow: openjd.PathDataFlowIn, ObjectType: openjd.PathObjectTypeFile,
			},
			{
				Name: "Ignored", Type: openjd.JobParamTypeListPath,
				DataFlow: openjd.PathDataFlowNone,
			},
			{Name: "Count", Type: openjd.JobParamTypeInt},
		},
	}
	params := map[string]string{
		"Scene":    "/proj/shot.ma",
		"Textures": `["/proj/tex/a.exr","/proj/tex/b.exr"]`,
		"Ignored":  `["/proj/skip.exr"]`,
		"Count":    "7",
	}

	got := buildStagingManifest(tmpl, params)

	want := []struct{ path, dir, objType string }{
		{"/proj/shot.ma", "IN", "FILE"},
		{"/proj/tex/a.exr", "IN", "FILE"},
		{"/proj/tex/b.exr", "IN", "FILE"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Path != w.path || got[i].Direction != w.dir || got[i].ObjectType != w.objType {
			t.Errorf("entry %d = %+v, want path=%q dir=%q objType=%q",
				i, got[i], w.path, w.dir, w.objType)
		}
	}
}

// TestBuildStagingManifest_ListPathEmptyAndMalformed pins the two degenerate
// cases: an empty list contributes nothing, and a malformed value is skipped
// rather than crashing the whole assignment — it was validated at submission,
// so reaching here malformed is a bug elsewhere, and failing the dispatch of
// every other parameter is a worse outcome than staging one fewer file.
func TestBuildStagingManifest_ListPathEmptyAndMalformed(t *testing.T) {
	tmpl := &openjd.JobTemplate{
		ParameterDefinitions: []openjd.JobParameter{
			{Name: "Empty", Type: openjd.JobParamTypeListPath, DataFlow: openjd.PathDataFlowIn},
			{Name: "Bad", Type: openjd.JobParamTypeListPath, DataFlow: openjd.PathDataFlowIn},
		},
	}
	got := buildStagingManifest(tmpl, map[string]string{
		"Empty": "[]",
		"Bad":   `["a"`,
	})
	if len(got) != 0 {
		t.Errorf("got %d entries, want 0: %+v", len(got), got)
	}
}
