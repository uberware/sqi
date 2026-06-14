// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Fuzz targets for the OpenJD parser.
//
// Running:
//
//	go test -fuzz=FuzzParseJSON ./internal/openjd/
//	go test -fuzz=FuzzParseYAML ./internal/openjd/
//
// The corpus seeds cover minimal valid templates, empty input, truncated input,
// and a few adversarial shapes to give the fuzzer a useful starting population.
// Go's fuzzer will mutate these seeds on its own; the seeds themselves are not
// intended to be exhaustive.
//
// Correctness invariants asserted on every corpus entry:
//  1. Parse must not panic.
//  2. If Parse succeeds, Validate must not panic.
//  3. If Parse returns an error, Validate is not called (parse errors are expected).
//  4. If Validate succeeds (no errors returned), the template must have a
//     non-empty name and at least one step — the minimal requirements the
//     validator enforces.

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// ── JSON fuzz target ──────────────────────────────────────────────────────────

func FuzzParseJSON(f *testing.F) {
	// Seed: minimal valid JSON template.
	f.Add([]byte(`{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "FuzzJob",
  "steps": [
    {
      "name": "Step1",
      "script": {
        "actions": {
          "onRun": { "command": "echo", "args": ["hi"] }
        }
      }
    }
  ]
}`))

	// Seed: template with a parameter space.
	f.Add([]byte(`{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "FrameRender",
  "parameterDefinitions": [
    {"name": "Frame", "type": "INT", "minValue": "1", "maxValue": "10"}
  ],
  "steps": [
    {
      "name": "Render",
      "script": {
        "actions": {
          "onRun": {"command": "render", "args": ["--frame", "{{Param.Frame}}"]}
        }
      },
      "parameterSpace": {
        "taskParameterDefinitions": [
          {"name": "Frame", "type": "INT", "range": ["1:10"]}
        ]
      }
    }
  ]
}`))

	// Seed: multi-step template with dependency.
	f.Add([]byte(`{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "TwoStep",
  "steps": [
    {
      "name": "Render",
      "script": {"actions": {"onRun": {"command": "render"}}}
    },
    {
      "name": "Composite",
      "dependencies": [{"dependsOn": "Render"}],
      "script": {"actions": {"onRun": {"command": "comp"}}}
    }
  ]
}`))

	// Seed: empty JSON object (must not panic, will fail validation).
	f.Add([]byte(`{}`))

	// Seed: empty input.
	f.Add([]byte{})

	// Seed: pure array (malformed top-level type).
	f.Add([]byte(`[]`))

	// Seed: truncated valid template.
	f.Add([]byte(`{"specificationVersion": "jobtemplate-2023-09", "name": "T"`))

	f.Fuzz(func(t *testing.T, data []byte) {
		tmpl, err := openjd.Parse(data, openjd.FormatJSON)
		if err != nil {
			// Parse errors are expected for arbitrary input; no further checks.
			return
		}

		// Parse succeeded — Validate must not panic and must return a coherent result.
		errs := openjd.Validate(tmpl)

		if len(errs) == 0 {
			// A fully valid template must satisfy the minimal invariants the
			// validator requires.
			if tmpl.Name == "" {
				t.Error("valid template has empty name")
			}
			if len(tmpl.Steps) == 0 {
				t.Error("valid template has no steps")
			}
		}
	})
}

// ── YAML fuzz target ──────────────────────────────────────────────────────────

func FuzzParseYAML(f *testing.F) {
	// Seed: minimal valid YAML template.
	f.Add([]byte(`specificationVersion: jobtemplate-2023-09
name: FuzzJobYAML
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
          args: ["hi"]
`))

	// Seed: template with job environment.
	f.Add([]byte(`specificationVersion: jobtemplate-2023-09
name: EnvJob
jobEnvironments:
  - name: Setup
    script:
      actions:
        onEnter:
          command: setup.sh
        onExit:
          command: teardown.sh
steps:
  - name: Work
    script:
      actions:
        onRun:
          command: work.sh
`))

	// Seed: template with path mapping rules.
	f.Add([]byte(`specificationVersion: jobtemplate-2023-09
name: PathMapJob
steps:
  - name: Render
    script:
      actions:
        onRun:
          command: render
          args: ["{{RootPath.nas_shows}}/scene.mb"]
`))

	// Seed: empty YAML.
	f.Add([]byte{})

	// Seed: scalar value at top level.
	f.Add([]byte(`"just a string"`))

	// Seed: truncated YAML.
	f.Add([]byte("specificationVersion: jobtemplate-2023-09\nname: T\nsteps:\n  - na"))

	f.Fuzz(func(t *testing.T, data []byte) {
		tmpl, err := openjd.Parse(data, openjd.FormatYAML)
		if err != nil {
			return
		}

		errs := openjd.Validate(tmpl)

		if len(errs) == 0 {
			if tmpl.Name == "" {
				t.Error("valid template has empty name")
			}
			if len(tmpl.Steps) == 0 {
				t.Error("valid template has no steps")
			}
		}
	})
}
