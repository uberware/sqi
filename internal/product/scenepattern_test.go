// SPDX-License-Identifier: AGPL-3.0-or-later

package product_test

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/fsutil"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/presettest"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// perFilePreset is a shipped preset that renders one exported scene file per
// frame, naming each frame's file by substituting the frame number into a
// SceneFile pattern such as shot.####.ass.
type perFilePreset struct {
	name string
	// extra holds any other job parameters the preset requires.
	extra map[string]string
	// goodTasks is the task count a valid pattern over Frames "1-2" expands to.
	goodTasks int
}

// perFilePresets lists every preset that carries the shared scene-pattern
// binding; TestPerFilePresets_ListIsComplete fails if one is missing.
var perFilePresets = []perFilePreset{
	{name: "arnold-kick-render", goodTasks: 2},
	{name: "redshift-standalone-render", goodTasks: 2},
	{name: "prman-render", goodTasks: 2},
	// Two frames of the default 2x2 grid: 8 Render tasks and 2 Assemble.
	{name: "arnold-kick-tile-render", extra: map[string]string{"OutputFile": "/mnt/out/beauty.####.exr"}, goodTasks: 10},
	{name: "3delight-render", goodTasks: 2},
	{name: "mantra-render", goodTasks: 2},
}

// badPatternMessage is the fragment every per-file preset's fail() message
// carries.
const badPatternMessage = "must contain exactly one run of #"

// scenePadLine is the first line of the shared binding, by which
// TestPerFilePresets_ListIsComplete recognizes a per-file preset.
const scenePadLine = "pad = Param.SceneFile.name.count('#')"

// presetTemplate returns the template text of the presets/sqi preset name.
func presetTemplate(t *testing.T, name string) string {
	t.Helper()
	tmpl, err := presettest.PresetTemplate(presettest.SourcePresets, name)
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

// parsedPresetTemplate returns the presets/sqi preset name's template, parsed.
func parsedPresetTemplate(t *testing.T, name string) *openjd.JobTemplate {
	t.Helper()
	tmpl, err := openjd.Parse([]byte(presetTemplate(t, name)), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("%s: openjd.Parse: %v", name, err)
	}
	return tmpl
}

// TestPerFilePresets_ListIsComplete asserts perFilePresets lists exactly the
// shipped presets that carry the scene binding.
func TestPerFilePresets_ListIsComplete(t *testing.T) {
	paths, err := fsutil.Glob(filepath.Join("..", "..", "presets", "sqi", "*.yaml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var found []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), scenePadLine) {
			found = append(found, strings.TrimSuffix(filepath.Base(p), ".yaml"))
		}
	}
	var listed []string
	for _, pf := range perFilePresets {
		listed = append(listed, pf.name)
	}
	slices.Sort(found)
	slices.Sort(listed)
	if !slices.Equal(found, listed) {
		t.Errorf("presets carrying the scene binding = %v, perFilePresets lists %v", found, listed)
	}
}

// TestPerFilePresets_RejectABadScenePattern asserts a scene pattern with no run
// of #, or with two runs, is rejected at submission, and a valid one expands.
func TestPerFilePresets_RejectABadScenePattern(t *testing.T) {
	for _, pf := range perFilePresets {
		t.Run(pf.name, func(t *testing.T) {
			tmpl := presetTemplate(t, pf.name)
			submit := func(scene string) (*openjd.SubmitResult, error) {
				params := map[string]string{"SceneFile": scene, "Frames": "1-2"}
				maps.Copy(params, pf.extra)
				st := fake.New()
				farmID, queueID := seedFarmQueue(t, st)
				return openjd.NewSubmitter(st).Submit(t.Context(), tmpl, store.TemplateFormatYAML, openjd.SubmitOptions{
					FarmID: farmID, QueueID: queueID, Parameters: params,
				})
			}
			for _, bad := range []string{"/mnt/s/shot.0001.ass", "/mnt/s/sh##ot.####.ass"} {
				t.Run(bad, func(t *testing.T) {
					_, err := submit(bad)
					if err == nil {
						t.Fatalf("pattern %q was accepted; it must be rejected at submission", bad)
					}
					if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
						t.Fatalf("want a *openjd.SubmitValidationError, got %T: %v", err, err)
					}
					if !strings.Contains(err.Error(), badPatternMessage) {
						t.Errorf("error does not name the pattern rule (%q): %v", badPatternMessage, err)
					}
				})
			}
			t.Run("good pattern", func(t *testing.T) {
				res, err := submit("/mnt/s/shot.####.ass")
				if err != nil {
					t.Fatalf("a valid pattern was rejected: %v", err)
				}
				if len(res.Tasks) != pf.goodTasks {
					t.Errorf("expanded to %d tasks, want %d", len(res.Tasks), pf.goodTasks)
				}
			})
		})
	}
}

// TestPerFilePresets_ShareOneScenePatternBinding asserts the scene-pattern let
// lines are byte-identical across every per-file preset.
func TestPerFilePresets_ShareOneScenePatternBinding(t *testing.T) {
	want := sceneBinding(t, perFilePresets[0].name)
	for _, pf := range perFilePresets {
		t.Run(pf.name, func(t *testing.T) {
			if got := sceneBinding(t, pf.name); got != want {
				t.Errorf("scene binding differs from %s's:\n got: %s\nwant: %s", perFilePresets[0].name, got, want)
			}
		})
	}
}

// sceneBinding returns the preset's "pad =" and "scene =" let lines, joined,
// failing if any step that carries one lacks the other, or if no step does.
func sceneBinding(t *testing.T, name string) string {
	t.Helper()
	tmpl := parsedPresetTemplate(t, name)
	binding := ""
	for _, s := range tmpl.Steps {
		if s.Script == nil {
			continue
		}
		var pad, scene string
		for _, l := range s.Script.Let {
			switch {
			case strings.HasPrefix(l, "pad ="):
				pad = l
			case strings.HasPrefix(l, "scene ="):
				scene = l
			}
		}
		if pad == "" && scene == "" {
			continue
		}
		if pad == "" || scene == "" {
			t.Fatalf("%s step %q carries only half of the scene binding", name, s.Name)
		}
		got := pad + "\n" + scene
		if binding != "" && got != binding {
			t.Fatalf("%s carries two different scene bindings", name)
		}
		binding = got
	}
	if binding == "" {
		t.Fatalf("%s has no step carrying the scene binding", name)
	}
	return binding
}

// TestPerFilePresets_BadPatternMessageNamesNoRenderer asserts the shared fail()
// message uses a renderer-neutral example and carries windowsFolderNote.
func TestPerFilePresets_BadPatternMessageNamesNoRenderer(t *testing.T) {
	binding := sceneBinding(t, perFilePresets[0].name)
	for _, want := range []string{"e.g. shot.####.<ext>", windowsFolderNote} {
		if !strings.Contains(binding, want) {
			t.Errorf("scene binding's fail() message lacks %q:\n%s", want, binding)
		}
	}
}

// windowsFolderNote is the clause both pattern messages carry. The template's
// POSIX path engine reads a backslash Windows path as one file name, so a # in
// a folder counts against the pattern.
const windowsFolderNote = "in a Windows path, folders may not contain #"

// TestArnoldKickTileRender_RejectsABadOutputPattern asserts an Output File
// Pattern with no # is rejected at submission, and both steps carry one
// identical out binding whose message includes windowsFolderNote.
func TestArnoldKickTileRender_RejectsABadOutputPattern(t *testing.T) {
	const name = "arnold-kick-tile-render"
	tmpl := presetTemplate(t, name)
	st := fake.New()
	farmID, queueID := seedFarmQueue(t, st)
	_, err := openjd.NewSubmitter(st).Submit(t.Context(), tmpl, store.TemplateFormatYAML, openjd.SubmitOptions{
		FarmID: farmID, QueueID: queueID, Parameters: map[string]string{
			"SceneFile": "/mnt/s/shot.####.ass", "OutputFile": "/mnt/out/beauty.exr", "Frames": "1",
		},
	})
	if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
		t.Fatalf("an Output File Pattern with no # was not rejected at submission: %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "Output File Pattern must contain exactly one run of #") {
		t.Errorf("error does not name the output pattern rule: %v", err)
	}

	parsed, err := openjd.Parse([]byte(tmpl), openjd.FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	var outs []string
	for _, s := range parsed.Steps {
		if s.Script == nil {
			continue
		}
		for _, l := range s.Script.Let {
			if strings.HasPrefix(l, "out =") {
				outs = append(outs, l)
			}
		}
	}
	if len(outs) != 2 || outs[0] != outs[1] {
		t.Fatalf("want one identical out binding in each of the two steps, got %q", outs)
	}
	if !strings.Contains(outs[0], windowsFolderNote) {
		t.Errorf("the out binding's fail() message lacks %q", windowsFolderNote)
	}
}
