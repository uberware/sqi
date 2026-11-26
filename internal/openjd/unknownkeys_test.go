// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// unknownKeyErrors parses and validates src and returns every "unknown field"
// error, whatever level reported it.
func unknownKeyErrors(t *testing.T, src string, f openjd.Format) []openjd.ValidationError {
	t.Helper()
	tmpl, err := openjd.Parse([]byte(src), f)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var out []openjd.ValidationError
	for _, e := range openjd.Validate(tmpl) {
		if strings.HasPrefix(e.Message, "unknown field; the ") {
			out = append(out, e)
		}
	}
	return out
}

// onRunEcho is a valid step script, indented for a step's script: key.
const onRunEcho = `    script:
      actions:
        onRun:
          command: echo
`

func ukMessage(object string) string {
	return "unknown field; the " + object + " schema defines no such key"
}

// typoHeader opens every template in TestFindUnknownKeys_OneTypoPerObject.
const typoHeader = `specificationVersion: jobtemplate-2023-09
name: J
`

func TestFindUnknownKeys_OneTypoPerObject(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		pointer string
		object  string
	}{
		{
			name: "environment actions",
			src: `jobEnvironments:
  - name: E
    script:
      actions:
        onEnter:
          command: echo
        onExt:
          command: echo
steps:
  - name: S
` + onRunEcho,
			pointer: "/jobEnvironments/0/script/actions/onExt",
			object:  "environment actions",
		},
		{
			name: "environment",
			src: `steps:
  - name: S
    stepEnvironments:
      - name: E
        variables:
          A: b
        scrpt:
          actions:
            onEnter:
              command: echo
` + onRunEcho,
			pointer: "/steps/0/stepEnvironments/0/scrpt",
			object:  "environment",
		},
		{
			name: "environment script",
			src: `jobEnvironments:
  - name: E
    script:
      embededFiles: []
      actions:
        onEnter:
          command: echo
steps:
  - name: S
` + onRunEcho,
			pointer: "/jobEnvironments/0/script/embededFiles",
			object:  "environment script",
		},
		{
			name: "host requirements",
			src: `steps:
  - name: S
    hostRequirements:
      attribtes:
        - name: attr.worker.os.family
          anyOf: [linux]
` + onRunEcho,
			pointer: "/steps/0/hostRequirements/attribtes",
			object:  "host requirements",
		},
		{
			name: "amount requirement",
			src: `steps:
  - name: S
    hostRequirements:
      amounts:
        - name: amount.worker.vcpu
          minimum: 4
` + onRunEcho,
			pointer: "/steps/0/hostRequirements/amounts/0/minimum",
			object:  "amount requirement",
		},
		{
			name: "attribute requirement",
			src: `steps:
  - name: S
    hostRequirements:
      attributes:
        - name: attr.worker.os.family
          anyOf: [linux]
          anyof: [macos]
` + onRunEcho,
			pointer: "/steps/0/hostRequirements/attributes/0/anyof",
			object:  "attribute requirement",
		},
		{
			name: "action",
			src: `steps:
  - name: S
    script:
      actions:
        onRun:
          command: echo
          timout: 3600
`,
			pointer: "/steps/0/script/actions/onRun/timout",
			object:  "action",
		},
		{
			name: "cancelation method",
			src: `steps:
  - name: S
    script:
      actions:
        onRun:
          command: echo
          cancelation:
            mode: NOTIFY_THEN_TERMINATE
            notifyPeriod: 30
`,
			pointer: "/steps/0/script/actions/onRun/cancelation/notifyPeriod",
			object:  "cancelation method",
		},
		{
			name: "step actions",
			src: `steps:
  - name: S
    script:
      actions:
        onRun:
          command: echo
        onEnter:
          command: echo
`,
			pointer: "/steps/0/script/actions/onEnter",
			object:  "step actions",
		},
		{
			name: "step script",
			src: `steps:
  - name: S
    script:
      action: {}
      actions:
        onRun:
          command: echo
`,
			pointer: "/steps/0/script/action",
			object:  "step script",
		},
		{
			name: "embedded file",
			src: `steps:
  - name: S
    script:
      embeddedFiles:
        - name: Run
          type: TEXT
          runable: true
          data: echo hi
      actions:
        onRun:
          command: "{{Task.File.Run}}"
`,
			pointer: "/steps/0/script/embeddedFiles/0/runable",
			object:  "embedded file",
		},
		{
			name: "job parameter definition",
			src: `parameterDefinitions:
  - name: P
    type: STRING
    defalt: x
steps:
  - name: S
` + onRunEcho,
			pointer: "/parameterDefinitions/0/defalt",
			object:  "job parameter definition",
		},
		{
			name: "user interface",
			src: `parameterDefinitions:
  - name: P
    type: STRING
    userInterface:
      lable: x
steps:
  - name: S
` + onRunEcho,
			pointer: "/parameterDefinitions/0/userInterface/lable",
			object:  "user interface",
		},
		{
			name: "file filter",
			src: `parameterDefinitions:
  - name: P
    type: PATH
    userInterface:
      control: CHOOSE_INPUT_FILE
      fileFilters:
        - label: Images
          pattern: ["*.exr"]
steps:
  - name: S
` + onRunEcho,
			pointer: "/parameterDefinitions/0/userInterface/fileFilters/0/pattern",
			object:  "file filter",
		},
		{
			name: "nested item constraint",
			src: `extensions: [EXPR]
parameterDefinitions:
  - name: P
    type: LIST[LIST[INT]]
    item: {item: {minValu: 1}}
steps:
  - name: S
` + onRunEcho,
			pointer: "/parameterDefinitions/0/item/item/minValu",
			object:  "item constraint",
		},
		{
			name: "parameter space",
			src: `steps:
  - name: S
    parameterSpace:
      taskParameterDefinitions:
        - name: F
          type: INT
          range: "1-3"
      combo: F
` + onRunEcho,
			pointer: "/steps/0/parameterSpace/combo",
			object:  "parameter space",
		},
		{
			name: "task parameter definition",
			src: `steps:
  - name: S
    parameterSpace:
      taskParameterDefinitions:
        - name: F
          type: INT
          range: "1-3"
          rnage: "1-3"
` + onRunEcho,
			pointer: "/steps/0/parameterSpace/taskParameterDefinitions/0/rnage",
			object:  "task parameter definition",
		},
		{
			name: "chunks",
			src: `extensions: [TASK_CHUNKING]
steps:
  - name: S
    parameterSpace:
      taskParameterDefinitions:
        - name: F
          type: CHUNK[INT]
          range: "1-10"
          chunks:
            defaultTaskCount: 2
            defaultTaskCnt: 3
            rangeConstraint: CONTIGUOUS
` + onRunEcho,
			pointer: "/steps/0/parameterSpace/taskParameterDefinitions/0/chunks/defaultTaskCnt",
			object:  "chunks",
		},
		{
			name: "step dependency",
			src: `steps:
  - name: A
` + onRunEcho + `  - name: B
    dependencies:
      - dependsOn: A
        dependOn: A
` + onRunEcho,
			pointer: "/steps/1/dependencies/0/dependOn",
			object:  "step dependency",
		},
		{
			// §2.2 puts the filters under userInterface, so at the parameter
			// root they are an unknown key.
			name: "file filters at the parameter root",
			src: `parameterDefinitions:
  - name: P
    type: PATH
    userInterface:
      control: CHOOSE_INPUT_FILE
    fileFilters:
      - label: All
        patterns: ["*"]
steps:
  - name: S
` + onRunEcho,
			pointer: "/parameterDefinitions/0/fileFilters",
			object:  "job parameter definition",
		},
		{
			name: "path translation block",
			src: `extensions: [SQI_PATH_TRANSLATION]
SQI_PATH_TRANSLATION:
  deliveries: [swap_in_place]
  delivries: [stage_locally]
steps:
  - name: S
` + onRunEcho,
			pointer: "/SQI_PATH_TRANSLATION/delivries",
			object:  "SQI_PATH_TRANSLATION block",
		},
		{
			name: "path delivery settings",
			src: `extensions: [SQI_PATH_TRANSLATION]
SQI_PATH_TRANSLATION:
  deliveries:
    - swap_in_place
    - command_flags: { pattern: "--remap {src}={dest}", patern: x }
steps:
  - name: S
` + onRunEcho,
			pointer: "/SQI_PATH_TRANSLATION/deliveries/1/command_flags/patern",
			object:  "path delivery settings",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := unknownKeyErrors(t, typoHeader+tc.src, openjd.FormatYAML)
			if len(got) != 1 {
				t.Fatalf("unknown-key errors = %v, want exactly one at %s", got, tc.pointer)
			}
			if got[0].Pointer != tc.pointer {
				t.Errorf("Pointer = %q, want %q", got[0].Pointer, tc.pointer)
			}
			if want := ukMessage(tc.object); got[0].Message != want {
				t.Errorf("Message = %q, want %q", got[0].Message, want)
			}
		})
	}
}

// TestFindUnknownKeys_AllSpecKeysAccepted uses every key the walk allows, so a
// key missing from the table shows up here as a valid template rejected.
func TestFindUnknownKeys_AllSpecKeysAccepted(t *testing.T) {
	src := `specificationVersion: jobtemplate-2023-09
name: J
extensions: [EXPR, TASK_CHUNKING]
parameterDefinitions:
  - name: S
    type: STRING
    description: d
    default: a
    allowedValues: [a, b]
    minLength: 1
    maxLength: 5
    userInterface:
      control: DROPDOWN_LIST
      label: L
      groupLabel: G
  - name: P
    type: PATH
    objectType: FILE
    dataFlow: IN
    userInterface:
      control: CHOOSE_INPUT_FILE
      fileFilters:
        - label: Images
          patterns: ["*.exr"]
      fileFilterDefault:
        label: Images
        patterns: ["*.exr"]
  - name: F
    type: FLOAT
    minValue: 0
    maxValue: 10
    userInterface:
      control: SPIN_BOX
      decimals: 2
      singleStepDelta: 0.5
  - name: L
    type: LIST[LIST[INT]]
    minLength: 1
    maxLength: 4
    default: [[1]]
    item:
      minLength: 1
      maxLength: 3
      item:
        allowedValues: [1, 2]
        minValue: 1
        maxValue: 2
jobEnvironments:
  - name: E
    description: d
    variables:
      A: b
    script:
      let:
        - x = 1
      embeddedFiles:
        - name: EF
          type: TEXT
          filename: e.sh
          runnable: true
          data: echo
      actions:
        onEnter:
          command: echo
        onExit:
          command: echo
steps:
  - name: A
    script:
      actions:
        onRun:
          command: echo
  - name: B
    description: d
    dependencies:
      - dependsOn: A
    hostRequirements:
      amounts:
        - name: amount.worker.vcpu
          min: 1
          max: 8
      attributes:
        - name: attr.worker.os.family
          anyOf: [linux]
        - name: attr.custom
          allOf: [a]
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: CHUNK[INT]
          range: "1-10"
          chunks:
            defaultTaskCount: 2
            targetRuntimeSeconds: 60
            rangeConstraint: CONTIGUOUS
        - name: Cam
          type: STRING
          range: [a, b]
      combination: "Frame * Cam"
    script:
      let:
        - y = 2
      embeddedFiles:
        - name: Run
          type: TEXT
          data: echo
      actions:
        onRun:
          command: echo
          args: [a]
          timeout: 60
          cancelation:
            mode: NOTIFY_THEN_TERMINATE
            notifyPeriodInSeconds: 30
`
	if got := unknownKeyErrors(t, src, openjd.FormatYAML); len(got) != 0 {
		t.Errorf("unknown-key errors = %v, want none", got)
	}
}

func TestFindUnknownKeys_VariablesAreData(t *testing.T) {
	src := `specificationVersion: jobtemplate-2023-09
name: J
jobEnvironments:
  - name: E
    variables:
      ANYTHING_AT_ALL: x
      onExt: y
steps:
  - name: S
` + onRunEcho
	if got := unknownKeyErrors(t, src, openjd.FormatYAML); len(got) != 0 {
		t.Errorf("unknown-key errors = %v, want none", got)
	}
}

func TestFindUnknownKeys_MisplacedStepBlockReportedOnce(t *testing.T) {
	src := `specificationVersion: jobtemplate-2023-09
name: J
steps:
  - name: S
    script:
      SQI_LOG_CHECKS:
        failOn: [ERROR]
      actions:
        onRun:
          command: echo
`
	var at []openjd.ValidationError
	for _, e := range openjd.Validate(mustParse(t, src)) {
		if e.Pointer == "/steps/0/script/SQI_LOG_CHECKS" {
			at = append(at, e)
		}
	}
	if len(at) != 1 || !strings.Contains(at[0].Message, "is a step-level block") {
		t.Errorf("errors at the block = %v, want exactly the misplaced-block error", at)
	}
}

func TestFindUnknownKeys_JSONMatchesYAML(t *testing.T) {
	src := `{"specificationVersion":"jobtemplate-2023-09","name":"J",
"jobEnvironments":[{"name":"E","script":{"actions":{"onEnter":{"command":"echo"},"onExt":{"command":"echo"}}}}],
"steps":[{"name":"S","script":{"actions":{"onRun":{"command":"echo"}}}}]}`
	got := unknownKeyErrors(t, src, openjd.FormatJSON)
	if len(got) != 1 || got[0].Pointer != "/jobEnvironments/0/script/actions/onExt" ||
		got[0].Message != ukMessage("environment actions") {
		t.Errorf("unknown-key errors = %v, want one at /jobEnvironments/0/script/actions/onExt", got)
	}
}

func TestFindUnknownKeys_PointerEscaping(t *testing.T) {
	tests := []struct {
		name, key, pointer string
	}{
		{"slash and tilde", `"a/b~c"`, "/steps/0/script/actions/onRun/a~1b~0c"},
		{"non-string key", "1", "/steps/0/script/actions/onRun/1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := `specificationVersion: jobtemplate-2023-09
name: J
steps:
  - name: S
    script:
      actions:
        onRun:
          command: echo
          ` + tc.key + `: x
`
			got := unknownKeyErrors(t, src, openjd.FormatYAML)
			if len(got) != 1 || got[0].Pointer != tc.pointer {
				t.Errorf("unknown-key errors = %v, want one at %s", got, tc.pointer)
			}
		})
	}
}

func TestFindUnknownKeys_TopAndStepLevelMessages(t *testing.T) {
	src := `specificationVersion: jobtemplate-2023-09
name: J
unknownField: x
steps:
  - name: S
    stepEnviroments: []
` + onRunEcho
	got := unknownKeyErrors(t, src, openjd.FormatYAML)
	want := map[string]string{
		"/unknownField":            "unknown field; the job template schema defines no such key",
		"/steps/0/stepEnviroments": "unknown field; the step template schema defines no such key",
	}
	if len(got) != len(want) {
		t.Fatalf("unknown-key errors = %v, want %v", got, want)
	}
	for _, e := range got {
		if want[e.Pointer] != e.Message {
			t.Errorf("error %v, want message %q at that pointer", e, want[e.Pointer])
		}
	}
}

// TestFindUnknownKeys_ReportsAtMostSixteen pins the cap: one unknown key is
// enough to reject a template, so a template stuffed with them reports only the
// first sixteen by pointer rather than one error per copy.
func TestFindUnknownKeys_ReportsAtMostSixteen(t *testing.T) {
	var b strings.Builder
	b.WriteString(`specificationVersion: jobtemplate-2023-09
name: J
steps:
  - name: S
    script:
      actions:
        onRun:
          command: echo
`)
	for i := range 40 {
		fmt.Fprintf(&b, "          k%02d: x\n", i)
	}
	got := unknownKeyErrors(t, b.String(), openjd.FormatYAML)
	if len(got) != 16 {
		t.Fatalf("unknown-key errors = %d, want 16", len(got))
	}
	if first, last := got[0].Pointer, got[15].Pointer; first != "/steps/0/script/actions/onRun/k00" ||
		last != "/steps/0/script/actions/onRun/k15" {
		t.Errorf("reported %s .. %s, want the first sixteen by pointer (k00 .. k15)", first, last)
	}
}

// TestValidate_TopAndStepUnknownKeysCapped pins the shared cap on the top
// level and the steps' own keys: two top-level typos and one in each of forty
// steps report sixteen errors, the top level's first and then the steps' in
// order.
func TestValidate_TopAndStepUnknownKeysCapped(t *testing.T) {
	var b strings.Builder
	b.WriteString("specificationVersion: jobtemplate-2023-09\nname: J\ntopA: x\ntopB: x\nsteps:\n")
	for i := range 40 {
		fmt.Fprintf(&b, "  - name: S%02d\n    stepEnviroments: []\n%s", i, onRunEcho)
	}
	got := unknownKeyErrors(t, b.String(), openjd.FormatYAML)
	if len(got) != 16 {
		t.Fatalf("unknown-key errors = %d, want 16: %v", len(got), got)
	}
	if got[0].Pointer != "/topA" || got[1].Pointer != "/topB" || got[2].Pointer != "/steps/0/stepEnviroments" ||
		got[15].Pointer != "/steps/13/stepEnviroments" {
		t.Errorf("reported %s, %s, %s .. %s; want /topA, /topB, then steps 0 .. 13",
			got[0].Pointer, got[1].Pointer, got[2].Pointer, got[15].Pointer)
	}
}

// TestValidate_ChunksOnlyOnChunkInt: the unknown-key walk accepts chunks, a
// TASK_CHUNKING key, on every task parameter, but only CHUNK[INT] applies it.
// On another type it is an error, since ignoring it would run the step one task
// per value.
func TestValidate_ChunksOnlyOnChunkInt(t *testing.T) {
	src := `specificationVersion: jobtemplate-2023-09
name: J
steps:
  - name: S
    parameterSpace:
      taskParameterDefinitions:
        - name: F
          type: INT
          range: "1-10"
          chunks:
            defaultTaskCount: 2
            rangeConstraint: CONTIGUOUS
` + onRunEcho
	assertValidationContains(t, mustParse(t, src),
		"/steps/0/parameterSpace/taskParameterDefinitions/0/chunks: chunks is valid only on CHUNK[INT]")
}
