// SPDX-License-Identifier: AGPL-3.0-or-later

package product

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/fsutil"
	"github.com/uberware/sqi/internal/openjd"
)

// presetExpectation is what one shipped preset under presets/sqi must satisfy:
// its catalog category, the convention-named parameters it must define, and the
// OpenJD extensions it must declare.
//
// Expectations are per preset because presets/sqi is not homogeneous: the
// render presets are Rendering and the ffmpeg presets Transcoding, some render
// presets do not chunk, and the segmented ffmpeg presets must not,
// because their join step names every intermediate file from the template,
// which works only when slices map one-to-one onto tasks.
type presetExpectation struct {
	category   string
	params     []string
	extensions []string
	// noExtensions asserts the template contains no `extensions:` key, by
	// byte-scan, as the scheduler's EXPR-withholding gate does. A nil
	// extensions field only skips the positive check below, so it cannot prove
	// the key is absent.
	noExtensions bool
	// osFamily, when set, is the attr.worker.os.family anyOf every step must
	// require, in order. A Windows-only preset sets it so a Linux worker
	// carrying a hand-set tag cannot lease work it cannot run.
	osFamily []string
	// taskParams, when set, are the task parameters every step must declare,
	// as name:TYPE in order. The params check above reads the template's
	// text, so it cannot tell a task parameter from a job parameter whose
	// name starts the same way.
	taskParams []string
}

// The reference presets (presets/sqi/) are authored in this repo and published
// to the preset library; this test pins them to the real schema.
//
// Every file in the directory must have an entry. The count check below makes
// adding a preset a paired change that states what the preset must satisfy.
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
			extensions: []string{"TASK_CHUNKING", "EXPR"},
			osFamily:   []string{"windows"},
		},
		"cinema4d-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "OutputPath"},
			extensions: []string{"TASK_CHUNKING", "EXPR"},
		},
		"cinema4d-take-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "OutputPath", "Take"},
			extensions: []string{"TASK_CHUNKING", "EXPR"},
		},
		// The two After Effects presets run aerender directly on Windows and
		// macOS workers. aerender can exit 0 when a render fails, so both declare
		// SQI_LOG_CHECKS. The whole-queue preset is one task and needs nothing
		// else; the per-item one passes a chunk's start and end.
		"after-effects-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "RenderQueueItem", "FramesPerTask"},
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS", "SQI_LOG_CHECKS"},
			osFamily:   []string{"windows", "macos"},
		},
		"after-effects-queue-render": {
			category:   "Rendering",
			params:     []string{"SceneFile"},
			extensions: []string{"SQI_LOG_CHECKS"},
			osFamily:   []string{"windows", "macos"},
		},
		// The three Mistika presets invoke the renderer as `-s START -e
		// END`, so they need a chunk's bounds rather than the chunk itself
		// and declare SQI_CHUNK_BOUNDS. So do the two Maya presets, the two
		// Blender presets, vray-standalone-render and after-effects-render.
		// Houdini and the two Nuke presets take a frame list
		// directly; husk-usd-render computes its start and count with EXPR,
		// and the two Cinema 4D presets and 3dsmax-render their start and end,
		// because they also need EXPR to leave out a blank output path; the
		// other standalone renderers render one file per frame and do not
		// chunk, and neither do the whole-queue After Effects presets.
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
		"hython-script": {
			category:   "General",
			params:     []string{"SceneFile", "Script", "Frames", "FramesPerTask"},
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS"},
		},
		"usdrecord-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "OutputFile", "Frames", "FramesPerTask", "Renderer", "Camera", "ImageWidth"},
			extensions: []string{"TASK_CHUNKING", "EXPR"},
		},
		"oiiotool-process": {
			category:   "Image Processing",
			params:     []string{"InputFile", "OutputFile", "Frames", "FramesPerTask", "Operations"},
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
			extensions: []string{"EXPR", "SQI_LOG_CHECKS"},
		},
		"mantra-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames"},
			extensions: []string{"EXPR"},
		},
		// DCC presets with SQI_LOG_CHECKS. katana-render passes a chunk's start
		// and end to -t, so it also declares SQI_CHUNK_BOUNDS.
		"katana-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "RenderNode", "FramesPerTask"},
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS", "SQI_LOG_CHECKS"},
		},
		// fusion-render passes a chunk's start and end to -start/-end.
		"fusion-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "FramesPerTask"},
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS", "SQI_LOG_CHECKS"},
		},
		// fusion-saver-render is a host-script preset: EXPR builds the
		// params.json its embedded render.lua reads, which names the one Saver
		// to render and the chunk's first and last frame. It declares no
		// SQI_CHUNK_BOUNDS, which is an error under EXPR.
		"fusion-saver-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "Saver", "FramesPerTask"},
			extensions: []string{"TASK_CHUNKING", "EXPR", "SQI_LOG_CHECKS"},
			taskParams: []string{"Frame:CHUNK[INT]"},
		},
		// The two silhouette presets declare no SQI_LOG_CHECKS: no failure line
		// is documented for sfxcmd. silhouette-render-all has no Session or
		// OutputNode, so it is a whole-scene product.
		"silhouette-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "Session", "OutputNode", "FramesPerTask"},
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS"},
		},
		"silhouette-render-all": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "FramesPerTask"},
			extensions: []string{"TASK_CHUNKING", "SQI_CHUNK_BOUNDS"},
		},
		// The two VRED presets are host-script presets: EXPR builds the
		// params.json their embedded render.py reads. They declare no
		// SQI_CHUNK_BOUNDS, which is an error under EXPR. vred-render has no View,
		// so it is a whole-scene product.
		"vred-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "FramesPerTask", "OutputFile"},
			extensions: []string{"TASK_CHUNKING", "EXPR", "SQI_LOG_CHECKS"},
		},
		"vred-viewpoint-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "View", "FramesPerTask", "OutputFile"},
			extensions: []string{"TASK_CHUNKING", "EXPR", "SQI_LOG_CHECKS"},
		},
		// The two KeyShot presets are host-script presets like VRED's, but
		// declare no SQI_CHUNK_BOUNDS: the script loops over any list of
		// frames (NONCONTIGUOUS) and reads no bounds. keyshot-render has no
		// Studio, so it is a whole-scene product.
		"keyshot-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "FramesPerTask", "OutputFile", "Width", "Height", "LicenseFeature"},
			extensions: []string{"TASK_CHUNKING", "EXPR", "SQI_LOG_CHECKS"},
		},
		"keyshot-studio-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Studio", "Frames", "FramesPerTask", "OutputFile", "Width", "Height", "LicenseFeature"},
			extensions: []string{"TASK_CHUNKING", "EXPR", "SQI_LOG_CHECKS"},
		},
		// The two Modo presets are host-script presets with CONTIGUOUS chunks
		// like VRED's. modo-render has no PassGroup, so it is a
		// whole-scene product.
		"modo-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "FramesPerTask"},
			extensions: []string{"TASK_CHUNKING", "EXPR", "SQI_LOG_CHECKS"},
		},
		"modo-pass-group-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "Frames", "PassGroup", "FramesPerTask"},
			extensions: []string{"TASK_CHUNKING", "EXPR", "SQI_LOG_CHECKS"},
		},
		// The two Unreal presets are host-script presets with one task and no
		// frame parameters, so no TASK_CHUNKING. The job preset's target
		// parameters are LevelSequence, Map and MovieRenderConfig; the queue
		// preset declares none of them, so it is a whole-scene product.
		"unreal-mrq-job-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "LevelSequence", "Map", "MovieRenderConfig", "OutputDirectory"},
			extensions: []string{"EXPR", "SQI_LOG_CHECKS"},
			osFamily:   []string{"windows"},
		},
		"unreal-mrq-queue-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "QueueAsset", "OutputDirectory"},
			extensions: []string{"EXPR", "SQI_LOG_CHECKS"},
			osFamily:   []string{"windows"},
		},
		// The shot preset is the job preset with one task per shot: a Shots
		// list job parameter and a Shot task parameter ranging over it.
		"unreal-mrq-shot-render": {
			category:   "Rendering",
			params:     []string{"SceneFile", "LevelSequence", "Map", "MovieRenderConfig", "Shots", "OutputDirectory"},
			extensions: []string{"EXPR", "SQI_LOG_CHECKS"},
			osFamily:   []string{"windows"},
			taskParams: []string{"Shot:STRING"},
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
			if exp.osFamily == nil && exp.taskParams == nil {
				return
			}
			tmpl, err := openjd.Parse([]byte(p.Template), openjd.FormatYAML)
			if err != nil {
				t.Fatalf("openjd.Parse: %v", err)
			}
			if len(tmpl.Steps) == 0 {
				t.Fatal("template declares no steps")
			}
			if exp.osFamily != nil {
				assertStepsOSFamily(t, tmpl, exp.osFamily)
			}
			if exp.taskParams != nil {
				assertStepsTaskParams(t, tmpl, exp.taskParams)
			}
		})
	}
}

// assertStepsTaskParams fails unless every step of tmpl declares exactly the
// task parameters want, each written name:TYPE, in order.
func assertStepsTaskParams(t *testing.T, tmpl *openjd.JobTemplate, want []string) {
	t.Helper()
	for _, step := range tmpl.Steps {
		var got []string
		if step.ParameterSpace != nil {
			for _, def := range step.ParameterSpace.TaskParameterDefinitions {
				got = append(got, def.Name+":"+string(def.Type))
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("step %q: task parameters = %v, want %v", step.Name, got, want)
		}
	}
}

// assertStepsOSFamily fails unless every step of tmpl requires
// attr.worker.os.family anyOf exactly want.
func assertStepsOSFamily(t *testing.T, tmpl *openjd.JobTemplate, want []string) {
	t.Helper()
	for _, step := range tmpl.Steps {
		var got []string
		if step.HostRequirements != nil {
			for _, attr := range step.HostRequirements.Attributes {
				if attr.Name == "attr.worker.os.family" {
					got = attr.AnyOf
				}
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("step %q: attr.worker.os.family anyOf = %v, want %v", step.Name, got, want)
		}
	}
}
