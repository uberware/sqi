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
// It exists because presets/sqi is not homogeneous. The render presets are
// "Rendering" and declare "TASK_CHUNKING"; the ffmpeg presets are
// Transcoding, and the segmented ones deliberately do NOT chunk (their join
// step names every intermediate file from the template, which is only possible
// when slices map one-to-one onto tasks). Per-preset expectations keep the
// pinning without the false generalization.
type presetExpectation struct {
	category   string
	params     []string
	extensions []string
	// noExtensions asserts the template declares no `extensions:` key at all,
	// by byte-scan -- the same technique the scheduler's own EXPR-withholding
	// gate uses. extensions: nil on its own only skips the (zero-iteration)
	// positive-extensions loop below; it does not prove the key is absent, so
	// ffmpeg-transcode's entire reason for existing (a template that is never
	// withheld from a worker whose EXPR limits undercut the server's) would
	// otherwise go unenforced.
	noExtensions bool
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
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS"},
		},
		"maya-scene-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "OutputDir", "Renderer"},
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS"},
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
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS"},
		},
		"blender-view-layer-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "OutputPath", "Scene", "ViewLayer"},
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS"},
		},
		"3dsmax-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "OutputPath"},
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS"},
		},
		"cinema4d-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "OutputPath"},
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS"},
		},
		"cinema4d-take-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "OutputPath", "Take"},
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS"},
		},
		"after-effects-render-windows": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "RenderQueueItem", "FramesPerTask"},
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS"},
		},
		// The whole-queue variant is one task, and its wrapper script is an
		// embedded file, which base OpenJD carries without an extension.
		"after-effects-queue-render-windows": {
			category:     "Rendering",
			params:       []string{"SceneFile"},
			noExtensions: true,
		},
		// The macOS variants differ from the Windows ones only in their gate
		// and wrapper (aftereffectspair_test.go), so their expectations match.
		"after-effects-render-macos": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "RenderQueueItem", "FramesPerTask"},
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS"},
		},
		"after-effects-queue-render-macos": {
			category:     "Rendering",
			params:       []string{"SceneFile"},
			noExtensions: true,
		},
		// The three Mistika presets invoke the renderer as `-s START -e
		// END`, so they need a chunk's bounds rather than the chunk itself.
		// They are not alone in that: the two Maya presets, the two Blender
		// presets, vray-standalone-render, the two Cinema 4D presets,
		// 3dsmax-render and both after-effects-render variants also declare
		// SQI_CHUNK_BOUNDS, thirteen of the twenty-five render presets.
		// Houdini and the two Nuke presets take a frame list directly;
		// husk-usd-render computes its start and count with EXPR; the other
		// standalone renderers render one file per frame and do not chunk,
		// and nor do the whole-queue After Effects presets.
		"mistika-boutique-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames"},
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS"},
		},
		"mistika-vr-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames"},
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS"},
		},
		"mistika-workflows-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames"},
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS"},
		},
		// Standalone renderers. The per-file ones (one exported scene file
		// per frame) declare EXPR for the shared scene-pattern binding
		// (scenepattern_test.go) and no task chunking.
		"arnold-kick-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames"},
			extensions: []string{"EXPR"},
		},
		"vray-standalone-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames"},
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS"},
		},
		"husk-usd-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "Renderer"},
			extensions: []string{"TASK_CHUNKING", "EXPR"},
		},
		"redshift-standalone-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames"},
			extensions: []string{"EXPR"},
		},
		"prman-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames"},
			extensions: []string{"EXPR"},
		},
		"arnold-kick-tile-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "OutputFile", "Width", "Height", "TilesX", "TilesY"},
			extensions: []string{"EXPR"},
		},
		"3delight-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames"},
			extensions: []string{"EXPR"},
		},
		"mantra-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames"},
			extensions: []string{"EXPR"},
		},
		"ffmpeg-transcode": {
			category: "Transcoding",
			params:   []string{"SourceFile", "OutputFile", "VideoCodec", "Quality", "AudioCodec"},
			// No extensions on purpose: this is the one converter that runs on
			// a worker whose expr.* caps undercut the server's.
			extensions:   nil,
			noExtensions: true,
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
			if exp.noExtensions && strings.Contains(p.Template, "extensions:") {
				t.Errorf("template declares %q but must declare no extensions at all", "extensions:")
			}
		})
	}
}
