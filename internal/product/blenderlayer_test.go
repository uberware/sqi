// SPDX-License-Identifier: AGPL-3.0-or-later

// An external test package: it reads the preset through presettest, and
// presettest imports product.
package product_test

import (
	"slices"
	"strings"
	"testing"
)

// TestBlenderViewLayerRender_NamesAreNeverInPythonSource pins the property
// that makes blender-view-layer-render safe: the view layer's and the scene's
// names reach Blender as their own arguments after `--`, and the Python
// expression that checks and selects them is a fixed string with no template
// substitution. A name holding a quote or a backslash could otherwise break the
// expression, or run as code. The expression reads the names back out of
// sys.argv instead.
//
// It parses the shipped YAML rather than asserting on a captured argv, because
// the captured argv shows only one pair of names; the property is about the
// template.
func TestBlenderViewLayerRender_NamesAreNeverInPythonSource(t *testing.T) {
	tmpl := parsedPresetTemplate(t, "blender-view-layer-render")
	if len(tmpl.Steps) != 1 || tmpl.Steps[0].Script == nil {
		t.Fatalf("preset declares %d steps, want 1 with a script", len(tmpl.Steps))
	}
	onRun := tmpl.Steps[0].Script.Actions.OnRun
	args := onRun.Args
	if onRun.Command != "blender" {
		t.Errorf("command = %q, want blender", onRun.Command)
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
		t.Errorf("the Python expression does not read the names from sys.argv: %q", source)
	}
	if strings.ContainsAny(source, "\r\n") {
		t.Errorf("the Python expression contains a line break, which OpenJD forbids in a literal arg: %q", source)
	}

	// The expression reads the layer then the scene: sys.argv after `--`, in order.
	want := []string{"--", "{{Param.ViewLayer}}", "{{Param.Scene}}"}
	if tail := args[max(0, len(args)-len(want)):]; !slices.Equal(tail, want) {
		t.Errorf("last three args = %v, want %v", tail, want)
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
