// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/presettest"
	"github.com/uberware/sqi/internal/store"
)

// Helpers shared by the host-script preset tests (Unreal, VRED, KeyShot, Modo, Fusion).

// presetTemplates caches presettest.PresetTemplate by preset name: it reads and
// validates the whole preset, and capturePreset runs once per subtest.
var presetTemplates sync.Map

// capturePreset expands one shipped preset under params through the production
// pipeline (presettest.Capture, which resolves phase 3 against the worker's
// default EXPR limits) and returns the snapshot.
func capturePreset(t *testing.T, preset string, params map[string]string) (presettest.Snapshot, error) {
	t.Helper()
	cached, ok := presetTemplates.Load(preset)
	if !ok {
		tmpl, err := presettest.PresetTemplate(presettest.SourcePresets, preset)
		if err != nil {
			t.Fatalf("PresetTemplate %s: %v", preset, err)
		}
		cached, _ = presetTemplates.LoadOrStore(preset, tmpl)
	}
	tmpl, ok := cached.(string)
	if !ok {
		t.Fatalf("presetTemplates holds a %T for %s", cached, preset)
	}
	return presettest.Capture(t.Context(), tmpl, presettest.Options{Params: params, Format: store.TemplateFormatYAML})
}

// presetCase returns the preset's fixture case called name.
func presetCase(t *testing.T, preset, name string) presettest.Case {
	t.Helper()
	cases, err := presettest.LoadCases(filepath.Join(presetCaseDir, preset+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s.yaml has no %s case", preset, name)
	return presettest.Case{}
}

// mustMarshal is json.Marshal that fails the test on an error.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// decodeStrict decodes a host script's params.json strictly: an unknown key is
// an error, so a key the script does not read cannot slip in unnoticed.
func decodeStrict[T any](t *testing.T, data string) T {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	var p T
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("params.json is not the JSON object the script expects: %v\n%s", err, data)
	}
	return p
}

// assertPresetsShareFile checks that every preset in presets embeds the file
// called name byte for byte as the first does, so a fix to one cannot miss the
// others.
func assertPresetsShareFile(t *testing.T, presets []string, name string) {
	t.Helper()
	var first string
	for i, preset := range presets {
		tmpl, err := presettest.PresetTemplate(presettest.SourcePresets, preset)
		if err != nil {
			t.Fatal(err)
		}
		data := embeddedFileData(t, tmpl, name)
		if i == 0 {
			first = data
			continue
		}
		if data != first {
			t.Errorf("%s's %s file differs from %s's", preset, name, presets[0])
		}
	}
}

// embeddedFileData returns the data of the first step's embedded file called
// name, read from the parsed template so a test always runs the shipped bytes.
func embeddedFileData(t *testing.T, tmpl, name string) string {
	t.Helper()
	parsed, err := openjd.Parse([]byte(tmpl), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("openjd.Parse: %v", err)
	}
	for _, f := range parsed.Steps[0].Script.EmbeddedFiles {
		if f.Name == name {
			return f.Data
		}
	}
	t.Fatalf("template declares no embedded file named %s", name)
	return ""
}
