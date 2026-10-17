// SPDX-License-Identifier: AGPL-3.0-or-later

package product

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// aeWrapperRef is the onRun argument naming the embedded aerender wrapper.
// Every argument after it is aerender's own argv.
const aeWrapperRef = "{{Task.File.aerender_wrapper}}"

// aeOS is one operating system's half of an After Effects preset pair.
type aeOS struct {
	// suffix ends the preset's name.
	suffix string
	// family is the attr.worker.os.family value its steps require.
	family string
	// command is the onRun command, which runs the wrapper.
	command string
	// filename is the wrapper's embedded file name.
	filename string
	// launch is every onRun argument before the wrapper reference: the
	// interpreter's own flags.
	launch []string
}

var (
	aeWindows = aeOS{
		suffix: "windows", family: "windows", command: "powershell", filename: "aerender_wrapper.ps1",
		launch: []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File"},
	}
	aeMacOS = aeOS{suffix: "macos", family: "macos", command: "/bin/sh", filename: "aerender_wrapper.sh"}
)

// aePresetKinds are the After Effects presets shipped as a Windows/macOS pair.
var aePresetKinds = []string{"after-effects-render", "after-effects-queue-render"}

// TestAfterEffectsPresets_VariantsDifferOnlyInGateAndWrapper pins design §5.3:
// the two variants of each After Effects preset differ only in their
// os.family gate and the wrapper that runs aerender. Their parameters, their
// parameter space and the argv aerender receives are identical, so an artist
// picking by render-node OS gets the same render either way, and an edit to
// one variant that is not made to the other fails here.
func TestAfterEffectsPresets_VariantsDifferOnlyInGateAndWrapper(t *testing.T) {
	for _, kind := range aePresetKinds {
		t.Run(kind, func(t *testing.T) {
			win := loadAETemplate(t, kind+"-"+aeWindows.suffix)
			mac := loadAETemplate(t, kind+"-"+aeMacOS.suffix)

			// Everything above the steps: parameterDefinitions, name,
			// extensions, specificationVersion.
			for _, key := range unionKeys(win, mac) {
				if key == "steps" {
					continue
				}
				if !reflect.DeepEqual(win[key], mac[key]) {
					t.Errorf("template %s differs:\n windows: %v\n   macos: %v", key, win[key], mac[key])
				}
			}
			if win["parameterDefinitions"] == nil {
				t.Error("template declares no parameterDefinitions")
			}

			winSteps, macSteps := aeSteps(t, win), aeSteps(t, mac)
			if len(winSteps) != len(macSteps) {
				t.Fatalf("windows has %d steps, macos %d", len(winSteps), len(macSteps))
			}
			for i := range winSteps {
				compareAESteps(t, winSteps[i], macSteps[i])
			}
		})
	}
}

// TestAfterEffectsPresets_WrapperSharedWithinOS pins what each preset's YAML
// comment claims: on one OS, the render and queue presets embed the same
// wrapper byte for byte, so a fix to one cannot silently miss the other.
func TestAfterEffectsPresets_WrapperSharedWithinOS(t *testing.T) {
	for _, host := range []aeOS{aeWindows, aeMacOS} {
		t.Run(host.suffix, func(t *testing.T) {
			var want string
			for _, kind := range aePresetKinds {
				name := kind + "-" + host.suffix
				steps := aeSteps(t, loadAETemplate(t, name))
				for _, step := range steps {
					file := aeWrapperFile(t, step)
					if got := file["filename"]; got != host.filename {
						t.Errorf("%s: wrapper filename = %v, want %s", name, got, host.filename)
					}
					data, ok := file["data"].(string)
					if !ok || data == "" {
						t.Fatalf("%s: wrapper has no data", name)
					}
					if want == "" {
						want = data
						continue
					}
					if data != want {
						t.Errorf("%s embeds a different wrapper from %s-%s\n got: %q\nwant: %q",
							name, aePresetKinds[0], host.suffix, data, want)
					}
				}
			}
		})
	}
}

// compareAESteps asserts that one step of each variant matches in everything
// but the gate and the wrapper.
func compareAESteps(t *testing.T, win, mac map[string]any) {
	t.Helper()
	name := fmt.Sprint(win["name"])
	for _, key := range unionKeys(win, mac) {
		switch key {
		case "hostRequirements", "script":
			continue // compared below
		}
		if !reflect.DeepEqual(win[key], mac[key]) {
			t.Errorf("step %q: %s differs:\n windows: %v\n   macos: %v", name, key, win[key], mac[key])
		}
	}

	// The gates differ only in os.family.
	winHR, winFamily := splitOSFamily(t, name, win)
	macHR, macFamily := splitOSFamily(t, name, mac)
	if !slices.Equal(winFamily, []string{aeWindows.family}) {
		t.Errorf("step %q: windows variant os.family anyOf = %v, want [%s]", name, winFamily, aeWindows.family)
	}
	if !slices.Equal(macFamily, []string{aeMacOS.family}) {
		t.Errorf("step %q: macos variant os.family anyOf = %v, want [%s]", name, macFamily, aeMacOS.family)
	}
	if !reflect.DeepEqual(winHR, macHR) {
		t.Errorf("step %q: hostRequirements differ beyond os.family:\n windows: %v\n   macos: %v", name, winHR, macHR)
	}

	// The script differs only in the wrapper: the same embedded file name,
	// and the same aerender argv after the wrapper argument.
	winArgs := aerenderArgv(t, name, aeWindows, win)
	macArgs := aerenderArgv(t, name, aeMacOS, mac)
	if len(winArgs) == 0 {
		t.Errorf("step %q: no aerender arguments after %s", name, aeWrapperRef)
	}
	if !reflect.DeepEqual(winArgs, macArgs) {
		t.Errorf("step %q: aerender argv differs:\n windows: %v\n   macos: %v", name, winArgs, macArgs)
	}
	winScript, macScript := mapField(win, "script"), mapField(mac, "script")
	// Everything in script.actions but onRun's command and args must match:
	// an onRun.timeout, a cancelation setting or an onEnter/onExit action
	// added to one variant is drift. The command and the args up to the
	// wrapper reference are pinned per OS by aerenderArgv instead.
	winActions := actionsWithoutLaunch(winScript)
	macActions := actionsWithoutLaunch(macScript)
	if !reflect.DeepEqual(winActions, macActions) {
		t.Errorf("step %q: script.actions differ beyond onRun's command and launch args:\n windows: %v\n   macos: %v",
			name, winActions, macActions)
	}
	for _, key := range unionKeys(winScript, macScript) {
		switch key {
		case "embeddedFiles":
			continue // the wrapper, compared by TestAfterEffectsPresets_WrapperSharedWithinOS
		case "actions":
			// Compared above, minus the part that legitimately differs.
			continue
		}
		if !reflect.DeepEqual(winScript[key], macScript[key]) {
			t.Errorf("step %q: script.%s differs", name, key)
		}
	}
}

// actionsWithoutLaunch returns a script's actions with onRun's command and
// args removed, leaving the rest of onRun and every other action.
func actionsWithoutLaunch(script map[string]any) map[string]any {
	actions := maps.Clone(mapField(script, "actions"))
	onRun := maps.Clone(asMap(actions["onRun"]))
	delete(onRun, "command")
	delete(onRun, "args")
	actions["onRun"] = onRun
	return actions
}

// splitOSFamily returns a step's hostRequirements with the os.family
// requirement removed, and that requirement's anyOf values.
func splitOSFamily(t *testing.T, name string, step map[string]any) (map[string]any, []string) {
	t.Helper()
	hr := mapField(step, "hostRequirements")
	attrs := asList(hr["attributes"])
	var family []string
	kept := make([]any, 0, len(attrs))
	for _, a := range attrs {
		attr := asMap(a)
		if attr["name"] != "attr.worker.os.family" {
			kept = append(kept, a)
			continue
		}
		if family != nil {
			t.Errorf("step %q: more than one os.family requirement", name)
		}
		family = []string{}
		values := asList(attr["anyOf"])
		for _, v := range values {
			family = append(family, fmt.Sprint(v))
		}
	}
	if family == nil {
		t.Errorf("step %q: no attr.worker.os.family requirement", name)
	}
	out := maps.Clone(hr)
	out["attributes"] = kept
	return out, family
}

// aerenderArgv returns the onRun arguments after the wrapper argument, having
// checked the command and that the wrapper argument is present.
func aerenderArgv(t *testing.T, name string, v aeOS, step map[string]any) []any {
	t.Helper()
	onRun := mapField(mapField(mapField(step, "script"), "actions"), "onRun")
	if got := onRun["command"]; got != v.command {
		t.Errorf("step %q: %s variant command = %v, want %s", name, v.suffix, got, v.command)
	}
	args := asList(onRun["args"])
	i := slices.Index(args, any(aeWrapperRef))
	if i < 0 {
		t.Fatalf("step %q: %s variant args do not pass %s: %v", name, v.suffix, aeWrapperRef, args)
	}
	var launch []string
	for _, a := range args[:i] {
		launch = append(launch, fmt.Sprint(a))
	}
	if !slices.Equal(launch, v.launch) {
		t.Errorf("step %q: %s variant args before %s = %v, want %v", name, v.suffix, aeWrapperRef, launch, v.launch)
	}
	return args[i+1:]
}

// aeWrapperFile returns a step's one embedded file, which must be the wrapper.
func aeWrapperFile(t *testing.T, step map[string]any) map[string]any {
	t.Helper()
	files := asList(mapField(step, "script")["embeddedFiles"])
	if len(files) != 1 {
		t.Fatalf("step %v: %d embedded files, want only the aerender wrapper", step["name"], len(files))
	}
	file := asMap(files[0])
	if file["name"] != "aerender_wrapper" {
		t.Fatalf("step %v: embedded file %v, want aerender_wrapper", step["name"], file["name"])
	}
	return file
}

// loadAETemplate decodes one presets/sqi preset's template.
func loadAETemplate(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "presets", "sqi", name+".yaml"))
	if err != nil {
		t.Fatalf("read preset: %v", err)
	}
	var def struct {
		Template map[string]any `yaml:"template"`
	}
	if err := yaml.Unmarshal(data, &def); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	if def.Template == nil {
		t.Fatalf("%s has no template", name)
	}
	return def.Template
}

// aeSteps returns a template's steps.
func aeSteps(t *testing.T, tmpl map[string]any) []map[string]any {
	t.Helper()
	raw := asList(tmpl["steps"])
	if len(raw) == 0 {
		t.Fatal("template declares no steps")
	}
	out := make([]map[string]any, len(raw))
	for i, s := range raw {
		step, ok := s.(map[string]any)
		if !ok {
			t.Fatalf("step %d is not a mapping", i)
		}
		out[i] = step
	}
	return out
}

// mapField returns m[key] as a mapping, or an empty one.
func mapField(m map[string]any, key string) map[string]any {
	return asMap(m[key])
}

// asMap returns v as a mapping, or an empty one when it is not a mapping.
func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok && m != nil {
		return m
	}
	return map[string]any{}
}

// asList returns v as a sequence, or nil when it is not a sequence.
func asList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

// unionKeys returns the keys of a and b, sorted.
func unionKeys(a, b map[string]any) []string {
	var keys []string
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}
