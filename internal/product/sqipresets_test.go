// SPDX-License-Identifier: AGPL-3.0-or-later

package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/fsutil"
)

// presetExpectation is what one shipped preset under presets/sqi must satisfy:
// its catalog category, the convention-named parameters it must define, and the
// OpenJD extensions it must declare.
//
// It exists because presets/sqi is no longer homogeneous. Until EXPR
// sub-project I the directory held six render presets and this test asserted
// "Rendering" and "TASK_CHUNKING" of every file in it; the ffmpeg presets are
// Transcoding, and the segmented ones deliberately do NOT chunk (their join
// step names every intermediate file from the template, which is only possible
// when slices map one-to-one onto tasks). Per-preset expectations keep the
// pinning without the false generalization.
type presetExpectation struct {
	category   string
	params     []string
	extensions []string
}

// The reference presets (presets/sqi/) are authored in this repo and published
// to the preset library; this test pins them to the real schema.
//
// EVERY FILE IN THE DIRECTORY MUST HAVE AN ENTRY. The count check below is not
// bookkeeping -- it is what makes adding a preset a paired change, so a new
// preset cannot ship without someone stating what it is supposed to be.
func TestSQIReferencePresets(t *testing.T) {
	want := map[string]presetExpectation{
		"maya-layer-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "OutputDir", "Renderer", "RenderLayer"},
			extensions: []string{"TASK_CHUNKING"},
		},
		"maya-scene-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "OutputDir", "Renderer"},
			extensions: []string{"TASK_CHUNKING"},
		},
		"houdini-rop-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "RopPath"},
			extensions: []string{"TASK_CHUNKING"},
		},
		"nuke-write-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "WriteNode"},
			extensions: []string{"TASK_CHUNKING"},
		},
		"nuke-script-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames"},
			extensions: []string{"TASK_CHUNKING"},
		},
		"blender-batch-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "OutputPath"},
			extensions: []string{"TASK_CHUNKING"},
		},
		"ffmpeg-transcode": {
			category: "Transcoding",
			params:   []string{"SourceFile", "OutputFile", "VideoCodec", "Quality", "AudioCodec"},
			// No extensions on purpose: this is the one converter that runs on
			// a worker whose expr.* caps undercut the server's.
			extensions: nil,
		},
		"ffmpeg-sequence-encode": {
			category:   "Transcoding",
			params:     []string{"SourcePattern", "StartFrame", "EndFrame", "FrameRate", "OutputDir"},
			extensions: []string{"EXPR"},
		},
		"ffmpeg-segment-transcode-bash": {
			category:   "Transcoding",
			params:     []string{"SourceFile", "OutputFile", "DurationSeconds", "SegmentSeconds"},
			extensions: []string{"EXPR"},
		},
		"ffmpeg-segment-transcode-powershell": {
			category:   "Transcoding",
			params:     []string{"SourceFile", "OutputFile", "DurationSeconds", "SegmentSeconds"},
			extensions: []string{"EXPR"},
		},
		"ffmpeg-segment-transcode-expr": {
			category:   "Transcoding",
			params:     []string{"SourceFile", "OutputFile", "DurationSeconds", "SegmentSeconds"},
			extensions: []string{"EXPR"},
		},
	}
	paths, err := fsutil.Glob(filepath.Join("..", "..", "presets", "sqi", "*.yaml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(paths) != len(want) {
		t.Fatalf("expected %d presets, found %d: %v -- every preset needs a want entry", len(want), len(paths), paths)
	}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".yaml")
		t.Run(name, func(t *testing.T) {
			exp, ok := want[name]
			if !ok {
				t.Fatalf("preset %q has no expectation entry", name)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			p, err := ParseDefinition(data, ValidateOptions{EnforceLimits: true})
			if err != nil {
				t.Fatalf("ParseDefinition: %v", err)
			}
			if p.Name != name {
				t.Errorf("name %q does not match file %q", p.Name, name)
			}
			if p.Category != exp.category {
				t.Errorf("category = %q, want %q", p.Category, exp.category)
			}
			if err := ValidateTemplate(p.Template, p.Format, ValidateOptions{EnforceLimits: true}); err != nil {
				t.Fatalf("ValidateTemplate: %v", err)
			}
			for _, param := range exp.params {
				if !strings.Contains(p.Template, "name: "+param) {
					t.Errorf("template missing convention parameter %q", param)
				}
			}
			for _, ext := range exp.extensions {
				if !strings.Contains(p.Template, ext) {
					t.Errorf("template does not declare %q", ext)
				}
			}
		})
	}
}
