// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// lcTemplate renders a one-step template. extensions is the YAML flow list for
// the extensions key ("" omits the key), top is spliced in at the top level,
// and stepExtra is spliced into the step (already indented four spaces).
func lcTemplate(extensions, top, stepExtra string) string {
	var b strings.Builder
	b.WriteString("specificationVersion: jobtemplate-2023-09\nname: LogChecksJob\n")
	if extensions != "" {
		fmt.Fprintf(&b, "extensions: %s\n", extensions)
	}
	if top != "" {
		b.WriteString(top)
		b.WriteString("\n")
	}
	b.WriteString("steps:\n  - name: Render\n")
	if stepExtra != "" {
		b.WriteString(stepExtra)
		b.WriteString("\n")
	}
	b.WriteString("    script:\n      actions:\n        onRun:\n          command: echo\n")
	return b.String()
}

func lcQuoted(patterns []string) string {
	q := make([]string, len(patterns))
	for i, p := range patterns {
		q[i] = "'" + strings.ReplaceAll(p, "'", "''") + "'"
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func lcHas(errs openjd.ValidationErrors, pointer, substr string) bool {
	for _, e := range errs {
		if e.Pointer == pointer && strings.Contains(e.Message, substr) {
			return true
		}
	}
	return false
}

func TestLogChecks_Validate(t *testing.T) {
	const declared = "[SQI_LOG_CHECKS]"
	block := func(body string) string { return "    SQI_LOG_CHECKS: " + body }

	many := make([]string, 33)
	for i := range many {
		many[i] = fmt.Sprintf("p%d", i)
	}
	atLimit := many[:32]

	tests := []struct {
		name        string
		extensions  string
		top         string
		stepExtra   string
		wantOK      bool
		wantPointer string
		wantMessage string
	}{
		{
			name:       "valid both lists",
			extensions: declared,
			stepExtra:  block("{failOn: ['ERROR: .*'], requireLine: ['SQI_RENDER: OK']}"),
			wantOK:     true,
		},
		{
			name:       "exactly 32 patterns is allowed",
			extensions: declared,
			stepExtra:  block("{failOn: " + lcQuoted(atLimit) + "}"),
			wantOK:     true,
		},
		{
			name:        "block without extension",
			stepExtra:   block("{failOn: [x]}"),
			wantPointer: "/steps/0/SQI_LOG_CHECKS",
			wantMessage: "requires declaring the SQI_LOG_CHECKS extension",
		},
		{
			name:        "extension without block",
			extensions:  declared,
			wantPointer: "/extensions",
			wantMessage: "no step carries an SQI_LOG_CHECKS block",
		},
		{
			name:        "both lists empty",
			extensions:  declared,
			stepExtra:   block("{failOn: [], requireLine: []}"),
			wantPointer: "/steps/0/SQI_LOG_CHECKS",
			wantMessage: "at least one of failOn or requireLine",
		},
		{
			name:        "null block is an empty block",
			extensions:  declared,
			stepExtra:   "    SQI_LOG_CHECKS:",
			wantPointer: "/steps/0/SQI_LOG_CHECKS",
			wantMessage: "at least one of failOn or requireLine",
		},
		{
			name:        "non-mapping block",
			extensions:  declared,
			stepExtra:   "    SQI_LOG_CHECKS: 5",
			wantPointer: "/steps/0/SQI_LOG_CHECKS",
			wantMessage: "must be a mapping with failOn and/or requireLine",
		},
		{
			name:        "bad regex",
			extensions:  declared,
			stepExtra:   block("{failOn: ['(']}"),
			wantPointer: "/steps/0/SQI_LOG_CHECKS/failOn/0",
			wantMessage: "invalid regular expression",
		},
		{
			name:        "too many",
			extensions:  declared,
			stepExtra:   block("{failOn: " + lcQuoted(many) + "}"),
			wantPointer: "/steps/0/SQI_LOG_CHECKS/failOn",
			wantMessage: "at most 32",
		},
		{
			name:        "too long",
			extensions:  declared,
			stepExtra:   block("{requireLine: ['" + strings.Repeat("a", 1025) + "']}"),
			wantPointer: "/steps/0/SQI_LOG_CHECKS/requireLine/0",
			wantMessage: "at most 1024 bytes",
		},
		{
			name:       "exactly 1024 bytes is allowed",
			extensions: declared,
			stepExtra:  block("{requireLine: ['" + strings.Repeat("a", 1024) + "']}"),
			wantOK:     true,
		},
		{
			// 905 bytes, under the byte cap, but ~226,000 compiled instructions:
			// matching one line would take seconds.
			name:        "oversized compiled program",
			extensions:  declared,
			stepExtra:   block("{failOn: [x, '" + strings.Repeat(".{0,999}", 113) + "x']}"),
			wantPointer: "/steps/0/SQI_LOG_CHECKS/failOn/1",
			wantMessage: "instructions; at most 2000 are allowed",
		},
		{
			name:       "compiled program exactly at the limit is allowed",
			extensions: declared,
			stepExtra:  block("{requireLine: ['a{998}a{1000}']}"),
			wantOK:     true,
		},
		{
			name:        "compiled program one over the limit",
			extensions:  declared,
			stepExtra:   block("{requireLine: ['a{999}a{1000}']}"),
			wantPointer: "/steps/0/SQI_LOG_CHECKS/requireLine/0",
			wantMessage: "pattern compiles to 2001 instructions; at most 2000 are allowed",
		},
		{
			name:        "empty pattern",
			extensions:  declared,
			stepExtra:   block("{failOn: ['']}"),
			wantPointer: "/steps/0/SQI_LOG_CHECKS/failOn/0",
			wantMessage: "must not be empty",
		},
		{
			name:        "unknown field",
			extensions:  declared,
			stepExtra:   block("{failOn: [x], fialOn: [y]}"),
			wantPointer: "/steps/0/SQI_LOG_CHECKS",
			wantMessage: `unknown field "fialOn"`,
		},
		{
			name:        "list value is a scalar",
			extensions:  declared,
			stepExtra:   block("{failOn: 'x'}"),
			wantPointer: "/steps/0/SQI_LOG_CHECKS/failOn",
			wantMessage: "must be a list of strings",
		},
		{
			name:        "list element is not a string",
			extensions:  declared,
			stepExtra:   block("{failOn: [{a: b}]}"),
			wantPointer: "/steps/0/SQI_LOG_CHECKS/failOn/0",
			wantMessage: "must be a string",
		},
		{
			name:        "misspelled step key",
			extensions:  declared,
			stepExtra:   "    SQI_LOG_CHECK:\n      failOn: [x]\n" + block("{failOn: [x]}"),
			wantPointer: "/steps/0/SQI_LOG_CHECK",
			wantMessage: "unknown sqi step block",
		},
		{
			name:       "non-SQI unknown step key is still dropped",
			extensions: declared,
			stepExtra:  "    fooBar: 1\n" + block("{failOn: [x]}"),
			wantOK:     true,
		},
		{
			name:        "top level, declared",
			extensions:  declared,
			top:         "SQI_LOG_CHECKS:\n  failOn: [x]",
			stepExtra:   block("{failOn: [x]}"),
			wantPointer: "/SQI_LOG_CHECKS",
			wantMessage: "is a step-level block",
		},
		{
			name:        "top level, undeclared",
			top:         "SQI_LOG_CHECKS:\n  failOn: [x]",
			wantPointer: "/SQI_LOG_CHECKS",
			wantMessage: "is a step-level block",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, err := openjd.Parse([]byte(lcTemplate(tc.extensions, tc.top, tc.stepExtra)), openjd.FormatYAML)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			errs := openjd.Validate(tmpl)
			if tc.wantOK {
				if len(errs) != 0 {
					t.Fatalf("want valid, got %v", errs)
				}
				return
			}
			if !lcHas(errs, tc.wantPointer, tc.wantMessage) {
				t.Fatalf("want error at %s containing %q; got %v", tc.wantPointer, tc.wantMessage, errs)
			}
		})
	}
}

// A SQI_LOG_CHECKS block anywhere but directly on a step would be dropped by the
// decoder, so the step would run with its checks off; validation rejects it at
// the pointer where it was found.
func TestLogChecks_MisplacedBlock(t *testing.T) {
	const header = "specificationVersion: jobtemplate-2023-09\nname: J\nextensions: [SQI_LOG_CHECKS]\n"
	const checks = "SQI_LOG_CHECKS: {failOn: [x]}"
	tests := []struct {
		name        string
		body        string
		wantPointer string // "" means the template is valid
	}{
		{
			name: "step level is the proper slot",
			body: "steps:\n  - name: S\n    " + checks + "\n" +
				"    script:\n      actions:\n        onRun:\n          command: echo\n",
		},
		{
			name: "under script",
			body: "steps:\n  - name: S\n    " + checks + "\n" +
				"    script:\n      " + checks + "\n      actions:\n        onRun:\n          command: echo\n",
			wantPointer: "/steps/0/script/SQI_LOG_CHECKS",
		},
		{
			name: "under actions.onRun",
			body: "steps:\n  - name: S\n    " + checks + "\n" +
				"    script:\n      actions:\n        onRun:\n          command: echo\n          " + checks + "\n",
			wantPointer: "/steps/0/script/actions/onRun/SQI_LOG_CHECKS",
		},
		{
			name: "under a step environment",
			body: "steps:\n  - name: S\n    " + checks + "\n" +
				"    stepEnvironments:\n      - name: E\n        " + checks + "\n" +
				"        variables: {A: b}\n" +
				"    script:\n      actions:\n        onRun:\n          command: echo\n",
			wantPointer: "/steps/0/stepEnvironments/0/SQI_LOG_CHECKS",
		},
		{
			name: "under a job environment",
			body: "jobEnvironments:\n  - name: E\n    " + checks + "\n    variables: {A: b}\n" +
				"steps:\n  - name: S\n    " + checks + "\n" +
				"    script:\n      actions:\n        onRun:\n          command: echo\n",
			wantPointer: "/jobEnvironments/0/SQI_LOG_CHECKS",
		},
		{
			name: "an environment variable named SQI_LOG_CHECKS is not a block",
			body: "steps:\n  - name: S\n    " + checks + "\n" +
				"    stepEnvironments:\n      - name: E\n        variables: {SQI_LOG_CHECKS: '1'}\n" +
				"    script:\n      actions:\n        onRun:\n          command: echo\n",
		},
		{
			name: "under a second step's script, the first step's block is valid",
			body: "steps:\n  - name: S\n    " + checks + "\n" +
				"    script:\n      actions:\n        onRun:\n          command: echo\n" +
				"  - name: T\n    script:\n      actions:\n        onRun:\n          command: echo\n" +
				"      " + checks + "\n",
			wantPointer: "/steps/1/script/SQI_LOG_CHECKS",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, err := openjd.Parse([]byte(header+tc.body), openjd.FormatYAML)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			errs := openjd.Validate(tmpl)
			if tc.wantPointer == "" {
				if len(errs) != 0 {
					t.Fatalf("want valid, got %v", errs)
				}
				return
			}
			if !lcHas(errs, tc.wantPointer, "SQI_LOG_CHECKS is a step-level block and is ignored here") {
				t.Fatalf("want a misplaced-block error at %s; got %v", tc.wantPointer, errs)
			}
		})
	}
}

// TestLogChecks_MisplacedBlockDeepNesting pins that the walk copes with deep
// nesting (it is iterative) and caps how many misplaced blocks it reports.
func TestLogChecks_MisplacedBlockDeepNesting(t *testing.T) {
	const depth = 5000
	var b strings.Builder
	b.WriteString(`{"specificationVersion":"jobtemplate-2023-09","name":"J","extensions":["SQI_LOG_CHECKS"],` +
		`"steps":[{"name":"S","SQI_LOG_CHECKS":{"failOn":["x"]},` +
		`"script":{"actions":{"onRun":{"command":"echo"}}},"extra":`)
	for range depth {
		b.WriteString(`{"SQI_LOG_CHECKS":1,"n":`)
	}
	b.WriteString("0")
	b.WriteString(strings.Repeat("}", depth))
	b.WriteString("}]}")
	tmpl, err := openjd.Parse([]byte(b.String()), openjd.FormatJSON)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var got int
	for _, e := range openjd.Validate(tmpl) {
		if strings.Contains(e.Message, "is a step-level block and is ignored here") {
			got++
		}
	}
	if got != 16 {
		t.Errorf("reported %d misplaced blocks, want the cap of 16", got)
	}
}

func TestLogChecks_TopLevelErrorIsNotTheGenericUnknownField(t *testing.T) {
	tmpl, err := openjd.Parse([]byte(lcTemplate("[SQI_LOG_CHECKS]", "SQI_LOG_CHECKS:\n  failOn: [x]", "")), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, e := range openjd.Validate(tmpl) {
		if e.Pointer == "/SQI_LOG_CHECKS" && strings.Contains(e.Message, "unknown field") {
			t.Fatalf("got the generic unknown-field message: %v", e)
		}
	}
}

func TestLogChecks_ParsedValues(t *testing.T) {
	y := lcTemplate("[SQI_LOG_CHECKS]", "",
		"    SQI_LOG_CHECKS:\n      failOn: ['ERROR', 'FATAL']\n      requireLine: ['SQI_RENDER: OK']")
	tmpl, err := openjd.Parse([]byte(y), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if errs := openjd.Validate(tmpl); len(errs) != 0 {
		t.Fatalf("Validate: %v", errs)
	}
	lc := tmpl.Steps[0].LogChecks
	if lc == nil {
		t.Fatal("LogChecks is nil")
	}
	if want := []string{"ERROR", "FATAL"}; !reflect.DeepEqual(lc.FailOn, want) {
		t.Errorf("FailOn = %v, want %v", lc.FailOn, want)
	}
	if want := []string{"SQI_RENDER: OK"}; !reflect.DeepEqual(lc.RequireLine, want) {
		t.Errorf("RequireLine = %v, want %v", lc.RequireLine, want)
	}
	if want := []string{"SQI_LOG_CHECKS"}; !reflect.DeepEqual(tmpl.Steps[0].SQIKeys, want) {
		t.Errorf("SQIKeys = %v, want %v", tmpl.Steps[0].SQIKeys, want)
	}
}

func TestLogChecks_AbsentBlockIsNil(t *testing.T) {
	tmpl, err := openjd.Parse([]byte(lcTemplate("", "", "")), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if tmpl.Steps[0].LogChecks != nil {
		t.Errorf("LogChecks = %+v, want nil", tmpl.Steps[0].LogChecks)
	}
	if len(tmpl.Steps[0].SQIKeys) != 0 {
		t.Errorf("SQIKeys = %v, want none", tmpl.Steps[0].SQIKeys)
	}
}

func TestLogChecks_SQIKeysAreSorted(t *testing.T) {
	y := lcTemplate("", "", "    SQI_ZED: 1\n    SQI_ALPHA: 2\n    plain: 3")
	tmpl, err := openjd.Parse([]byte(y), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := []string{"SQI_ALPHA", "SQI_ZED"}; !reflect.DeepEqual(tmpl.Steps[0].SQIKeys, want) {
		t.Errorf("SQIKeys = %v, want %v", tmpl.Steps[0].SQIKeys, want)
	}
}

func TestLogChecks_JSONFormat(t *testing.T) {
	j := `{"specificationVersion":"jobtemplate-2023-09","name":"J","extensions":["SQI_LOG_CHECKS"],
"steps":[{"name":"S","SQI_LOG_CHECKS":{"requireLine":["ok"]},
"script":{"actions":{"onRun":{"command":"echo"}}}}]}`
	tmpl, err := openjd.Parse([]byte(j), openjd.FormatJSON)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if errs := openjd.Validate(tmpl); len(errs) != 0 {
		t.Fatalf("Validate: %v", errs)
	}
	if lc := tmpl.Steps[0].LogChecks; lc == nil || !reflect.DeepEqual(lc.RequireLine, []string{"ok"}) {
		t.Errorf("LogChecks = %+v", lc)
	}
}
