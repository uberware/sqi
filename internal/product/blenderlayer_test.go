// SPDX-License-Identifier: AGPL-3.0-or-later

// An external test package: it locates the preset through presettest, and
// presettest imports product.
package product_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/presettest"
)

// TestBlenderViewLayerRender_LayerNameIsNeverInPythonSource pins the property
// that makes blender-view-layer-render safe: the view layer's name reaches
// Blender as its own argument after `--`, and the Python expression that
// selects it is a fixed string with no template substitution. A name holding a
// quote or a backslash could otherwise break the expression, or run as code.
// The expression reads the name back out of sys.argv instead.
//
// It parses the shipped YAML rather than asserting on a captured argv, because
// the captured argv shows only one layer name; the property is about the
// template.
func TestBlenderViewLayerRender_LayerNameIsNeverInPythonSource(t *testing.T) {
	root, err := presettest.RepoRoot()
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "presets", "sqi", "blender-view-layer-render.yaml"))
	if err != nil {
		t.Fatalf("read preset: %v", err)
	}
	var def struct {
		Template struct {
			Steps []struct {
				Script struct {
					Actions struct {
						OnRun struct {
							Command string   `yaml:"command"`
							Args    []string `yaml:"args"`
						} `yaml:"onRun"`
					} `yaml:"actions"`
				} `yaml:"script"`
			} `yaml:"steps"`
		} `yaml:"template"`
	}
	if err := yaml.Unmarshal(data, &def); err != nil {
		t.Fatalf("parse preset: %v", err)
	}
	if len(def.Template.Steps) != 1 {
		t.Fatalf("preset declares %d steps, want 1", len(def.Template.Steps))
	}
	args := def.Template.Steps[0].Script.Actions.OnRun.Args
	if got := def.Template.Steps[0].Script.Actions.OnRun.Command; got != "blender" {
		t.Errorf("command = %q, want blender", got)
	}

	expr := slices.Index(args, "--python-expr")
	if expr < 0 || expr+1 >= len(args) {
		t.Fatalf("args %v carry no --python-expr value", args)
	}
	source := args[expr+1]
	if strings.Contains(source, "{{") {
		t.Errorf("the Python expression contains template substitution: %q", source)
	}
	if !strings.Contains(source, "sys.argv") {
		t.Errorf("the Python expression does not read the layer name from sys.argv: %q", source)
	}
	if strings.ContainsAny(source, "\r\n") {
		t.Errorf("the Python expression contains a line break, which OpenJD forbids in a literal arg: %q", source)
	}

	if len(args) < 2 || args[len(args)-2] != "--" || args[len(args)-1] != "{{Param.ViewLayer}}" {
		t.Errorf("last two args = %v, want [-- {{Param.ViewLayer}}]", args[max(0, len(args)-2):])
	}
	// `--` ends Blender's own argument parsing, so everything Blender must act
	// on, -a above all, has to precede it, and the layer selection has to run
	// before -a renders.
	dashes := slices.Index(args, "--")
	render := slices.Index(args, "-a")
	if render < 0 || dashes < 0 || render > dashes {
		t.Errorf("-a at %d, -- at %d: -a must precede --", render, dashes)
	}
	if expr > render {
		t.Errorf("--python-expr at %d follows -a at %d: layers would be selected after the render", expr, render)
	}
	if code := slices.Index(args, "--python-exit-code"); code < 0 || code > expr {
		t.Errorf("--python-exit-code at %d must precede --python-expr at %d", code, expr)
	}
}
