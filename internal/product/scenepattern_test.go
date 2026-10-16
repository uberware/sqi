// SPDX-License-Identifier: AGPL-3.0-or-later

package product_test

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/product"
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
}

// perFilePresets lists every preset that carries the shared scene-pattern
// binding. A new per-file preset joins this list.
var perFilePresets = []perFilePreset{
	{name: "arnold-kick-render"},
	{name: "redshift-standalone-render"},
}

// badPatternMessage is the fragment every per-file preset's fail() message
// carries.
const badPatternMessage = "must contain exactly one run of #"

func perFileTemplate(t *testing.T, name string) (string, store.Product) {
	t.Helper()
	path := filepath.Join("..", "..", "presets", "sqi", name+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	p, err := product.ParseDefinition(data, product.ValidateOptions{EnforceLimits: true})
	if err != nil {
		t.Fatalf("ParseDefinition %s: %v", path, err)
	}
	return p.Template, p
}

// TestPerFilePresets_RejectABadScenePattern pins the guard that keeps a
// per-file preset from rendering one file N times: a pattern with no frame
// token, or with the # characters split into two runs, is refused at
// SUBMISSION by the phase-2 re-check, never accepted and rendered.
func TestPerFilePresets_RejectABadScenePattern(t *testing.T) {
	for _, pf := range perFilePresets {
		t.Run(pf.name, func(t *testing.T) {
			tmpl, p := perFileTemplate(t, pf.name)
			submit := func(scene string) (*openjd.SubmitResult, error) {
				params := map[string]string{"SceneFile": scene, "Frames": "1-2"}
				maps.Copy(params, pf.extra)
				st := fake.New()
				farmID, queueID := seedFarmQueue(t, st)
				return openjd.NewSubmitter(st).Submit(t.Context(), tmpl, p.Format, openjd.SubmitOptions{
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
				if len(res.Tasks) != 2 {
					t.Errorf("expanded to %d tasks, want 2", len(res.Tasks))
				}
			})
		})
	}
}

// TestPerFilePresets_ShareOneScenePatternBinding pins the scene-pattern let
// lines byte-identical across every per-file preset, so a fix to the rule in
// one preset cannot leave the others behind.
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
	tmpl, _ := perFileTemplate(t, name)
	var doc struct {
		Steps []struct {
			Name   string `yaml:"name"`
			Script struct {
				Let []string `yaml:"let"`
			} `yaml:"script"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal([]byte(tmpl), &doc); err != nil {
		t.Fatalf("decode %s template: %v", name, err)
	}
	binding := ""
	for _, s := range doc.Steps {
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
