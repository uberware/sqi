// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

const lpDeclared = "[SQI_LOG_PROGRESS]"

func lpBlock(body string) string { return "    SQI_LOG_PROGRESS: " + body }

// lpSpace is a one-parameter CHUNK[INT] parameter space.
const lpSpace = "    parameterSpace:\n      taskParameterDefinitions:\n" +
	"        - {name: Frame, type: 'CHUNK[INT]', range: '1-10', chunks: {defaultTaskCount: 2, rangeConstraint: CONTIGUOUS}}\n"

func lpSpaceOf(defs ...string) string {
	return "    parameterSpace:\n      taskParameterDefinitions:\n        - " + strings.Join(defs, "\n        - ") + "\n"
}

func lpParse(t *testing.T, extensions, stepExtra string) (*openjd.JobTemplate, openjd.ValidationErrors) {
	t.Helper()
	ext := extensions
	if strings.Contains(stepExtra, "CHUNK[INT]") && ext != "" {
		ext = strings.TrimSuffix(ext, "]") + ", TASK_CHUNKING]"
	}
	tmpl, err := openjd.Parse([]byte(lcTemplate(ext, "", strings.TrimSuffix(stepExtra, "\n"))), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return tmpl, openjd.Validate(tmpl)
}

func TestLogProgress_Validate(t *testing.T) {
	many := make([]string, 33)
	for i := range many {
		many[i] = fmt.Sprintf("(?P<percent>%d)", i)
	}
	long := "(?P<percent>x)" + strings.Repeat("a", 1025)
	huge := "(?P<percent>a{998}a{1000})"

	tests := []struct {
		name        string
		extensions  string
		stepExtra   string
		wantPointer string // "" means valid
		wantMessage string
	}{
		{
			name: "valid percent and status", extensions: lpDeclared,
			stepExtra: lpBlock("{progress: ['(?P<percent>[0-9]+)%'], status: ['(?P<status>.*)']}"),
		},
		{
			name: "valid frame form, one chunk parameter", extensions: lpDeclared,
			stepExtra: lpBlock("{progress: ['Frame (?P<frame>[0-9]+)']}") + "\n" + lpSpace,
		},
		{
			name:        "block without extension",
			stepExtra:   lpBlock("{progress: ['(?P<percent>x)']}"),
			wantPointer: "/steps/0/SQI_LOG_PROGRESS",
			wantMessage: "requires declaring the SQI_LOG_PROGRESS extension",
		},
		{
			name: "extension without block", extensions: lpDeclared,
			wantPointer: "/extensions",
			wantMessage: "no step carries an SQI_LOG_PROGRESS block",
		},
		{
			name: "neither list", extensions: lpDeclared,
			stepExtra:   lpBlock("{progress: [], status: []}"),
			wantPointer: "/steps/0/SQI_LOG_PROGRESS",
			wantMessage: "at least one of progress or status",
		},
		{
			name: "unknown key", extensions: lpDeclared,
			stepExtra:   lpBlock("{frameParam: Frame, progress: ['(?P<percent>x)']}"),
			wantPointer: "/steps/0/SQI_LOG_PROGRESS",
			wantMessage: "frameParam",
		},
		{
			name: "progress not a list", extensions: lpDeclared,
			stepExtra:   lpBlock("{progress: nope}"),
			wantPointer: "/steps/0/SQI_LOG_PROGRESS/progress",
			wantMessage: "list of strings",
		},
		{
			name: "non-string item", extensions: lpDeclared,
			stepExtra:   lpBlock("{progress: ['(?P<percent>x)', 7]}"),
			wantPointer: "/steps/0/SQI_LOG_PROGRESS/progress/1",
			wantMessage: "must be a string",
		},
		{
			name: "wrong group set", extensions: lpDeclared,
			stepExtra:   lpBlock("{progress: ['(?P<done>1)']}"),
			wantPointer: "/steps/0/SQI_LOG_PROGRESS/progress/0",
			wantMessage: "named groups found",
		},
		{
			name: "frame group in a status pattern", extensions: lpDeclared,
			stepExtra:   lpBlock("{status: ['(?P<frame>1)']}"),
			wantPointer: "/steps/0/SQI_LOG_PROGRESS/status/0",
			wantMessage: "status",
		},
		{
			name: "progress group named twice", extensions: lpDeclared,
			stepExtra:   lpBlock("{progress: ['a (?P<percent>[0-9]+)|b (?P<percent>[0-9]+)']}"),
			wantPointer: "/steps/0/SQI_LOG_PROGRESS/progress/0",
			wantMessage: "more than once",
		},
		{
			name: "status group named twice", extensions: lpDeclared,
			stepExtra:   lpBlock("{status: ['a (?P<status>x)|b (?P<status>y)']}"),
			wantPointer: "/steps/0/SQI_LOG_PROGRESS/status/0",
			wantMessage: "more than once",
		},
		{
			name: "within-frame pattern alone", extensions: lpDeclared,
			stepExtra:   lpBlock("{progress: ['(?P<framepercent>1)']}") + "\n" + lpSpace,
			wantPointer: "/steps/0/SQI_LOG_PROGRESS/progress/0",
			wantMessage: "within-frame",
		},
		{
			name: "within-frame pattern with a frame pattern", extensions: lpDeclared,
			stepExtra: lpBlock("{progress: ['(?P<framepercent>1)', 'F (?P<frame>1)']}") + "\n" + lpSpace,
		},
		{
			name: "frame form without a frame parameter", extensions: lpDeclared,
			stepExtra:   lpBlock("{progress: ['(?P<frame>1)']}"),
			wantPointer: "/steps/0/SQI_LOG_PROGRESS",
			wantMessage: "INT or CHUNK[INT] task parameter",
		},
		{
			name: "two frame parameters, no frameParameter", extensions: lpDeclared,
			stepExtra: lpBlock("{progress: ['(?P<frame>1)']}") + "\n" +
				lpSpaceOf("{name: A, type: INT, range: '1-3'}", "{name: B, type: INT, range: '1-3'}"),
			wantPointer: "/steps/0/SQI_LOG_PROGRESS",
			wantMessage: "A, B",
		},
		{
			name: "two frame parameters, one named", extensions: lpDeclared,
			stepExtra: lpBlock("{frameParameter: B, progress: ['(?P<frame>1)']}") + "\n" +
				lpSpaceOf("{name: A, type: INT, range: '1-3'}", "{name: B, type: INT, range: '1-3'}"),
		},
		{
			name: "frameParameter names a missing parameter", extensions: lpDeclared,
			stepExtra:   lpBlock("{frameParameter: Nope, progress: ['(?P<frame>1)']}") + "\n" + lpSpace,
			wantPointer: "/steps/0/SQI_LOG_PROGRESS/frameParameter",
			wantMessage: "Nope",
		},
		{
			name: "frameParameter names a STRING parameter", extensions: lpDeclared,
			stepExtra: lpBlock("{frameParameter: S, progress: ['(?P<frame>1)']}") + "\n" +
				lpSpaceOf("{name: S, type: STRING, range: [a, b]}"),
			wantPointer: "/steps/0/SQI_LOG_PROGRESS/frameParameter",
			wantMessage: "INT or CHUNK[INT]",
		},
		{
			name: "33 patterns", extensions: lpDeclared,
			stepExtra:   lpBlock("{progress: " + lcQuoted(many) + "}"),
			wantPointer: "/steps/0/SQI_LOG_PROGRESS/progress",
			wantMessage: "at most 32",
		},
		{
			name: "32 patterns", extensions: lpDeclared,
			stepExtra: lpBlock("{progress: " + lcQuoted(many[:32]) + "}"),
		},
		{
			name: "1025-byte pattern", extensions: lpDeclared,
			stepExtra:   lpBlock("{progress: " + lcQuoted([]string{long}) + "}"),
			wantPointer: "/steps/0/SQI_LOG_PROGRESS/progress/0",
			wantMessage: "at most 1024 bytes",
		},
		{
			name: "over-size program", extensions: lpDeclared,
			stepExtra:   lpBlock("{progress: " + lcQuoted([]string{strings.Replace(huge, "a{998}a{1000}", "(a{900}){900}", 1)}) + "}"),
			wantPointer: "/steps/0/SQI_LOG_PROGRESS/progress/0",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := lpParse(t, tc.extensions, tc.stepExtra)
			if tc.wantPointer == "" {
				if len(errs) != 0 {
					t.Fatalf("want valid, got %v", errs)
				}
				return
			}
			if !lcHas(errs, tc.wantPointer, tc.wantMessage) {
				t.Fatalf("want error at %s containing %q, got %v", tc.wantPointer, tc.wantMessage, errs)
			}
		})
	}
}

func lpStepsJSON(steps int, block string) []byte {
	var b strings.Builder
	b.WriteString(`{"specificationVersion":"jobtemplate-2023-09","name":"J","extensions":["SQI_LOG_PROGRESS"],"steps":[`)
	for i := range steps {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"name":"S%d","SQI_LOG_PROGRESS":%s,"script":{"actions":{"onRun":{"command":"echo"}}}}`, i, block)
	}
	b.WriteString("]}")
	return []byte(b.String())
}

func TestValidateLogProgress_TemplateBudget(t *testing.T) {
	pat := `"(?P<percent>x)a{998}a{997}"`
	list := "[" + strings.Repeat(pat+",", 31) + pat + "]"
	block := `{"progress":` + list + `,"status":` + strings.ReplaceAll(list, "(?P<percent>x)", "(?P<status>x)") + `}`
	tmpl, err := openjd.Parse(lpStepsJSON(1, block), openjd.FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	if errs := openjd.Validate(tmpl); len(errs) != 0 {
		t.Fatalf("one full step: want valid, got %v", errs)
	}
	tmpl, err = openjd.Parse(lpStepsJSON(3, block), openjd.FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	var got openjd.ValidationErrors
	for _, e := range openjd.Validate(tmpl) {
		if strings.Contains(e.Message, "compiled instructions in total") {
			got = append(got, e)
		}
	}
	if len(got) != 1 || got[0].Pointer != "/steps/1/SQI_LOG_PROGRESS/progress/0" || !strings.Contains(got[0].Message, "SQI_LOG_PROGRESS") {
		t.Fatalf("want one SQI_LOG_PROGRESS budget error at /steps/1/SQI_LOG_PROGRESS/progress/0, got %v", got)
	}
}

func TestValidateLogProgress_BudgetSeparateFromLogChecks(t *testing.T) {
	pat := `"a{998}a{1000}"`
	list := "[" + strings.Repeat(pat+",", 31) + pat + "]"
	ppat := `"(?P<percent>x)a{998}a{997}"`
	plist := "[" + strings.Repeat(ppat+",", 31) + ppat + "]"
	raw := `{"specificationVersion":"jobtemplate-2023-09","name":"J","extensions":["SQI_LOG_CHECKS","SQI_LOG_PROGRESS"],"steps":[` +
		`{"name":"S","SQI_LOG_CHECKS":{"failOn":` + list + `,"requireLine":` + list + `},` +
		`"SQI_LOG_PROGRESS":{"progress":` + plist + `,"status":` + strings.ReplaceAll(plist, "(?P<percent>x)", "(?P<status>x)") + `},` +
		`"script":{"actions":{"onRun":{"command":"echo"}}}}]}`
	tmpl, err := openjd.Parse([]byte(raw), openjd.FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	if errs := openjd.Validate(tmpl); len(errs) != 0 {
		t.Fatalf("both full blocks on one step: want valid, got %v", errs)
	}
}

func TestLogProgress_MisplacedBlockNamesExtension(t *testing.T) {
	body := "specificationVersion: jobtemplate-2023-09\nname: J\nextensions: [SQI_LOG_PROGRESS]\nsteps:\n  - name: S\n" +
		lpBlock("{progress: ['(?P<percent>x)']}") + "\n" +
		"    script:\n      SQI_LOG_PROGRESS: {progress: ['(?P<percent>x)']}\n      actions:\n        onRun:\n          command: echo\n"
	tmpl, err := openjd.Parse([]byte(body), openjd.FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	errs := openjd.Validate(tmpl)
	if !lcHas(errs, "/steps/0/script/SQI_LOG_PROGRESS", "SQI_LOG_PROGRESS is a step-level block") {
		t.Fatalf("got %v", errs)
	}
	n := 0
	for _, e := range errs {
		if strings.Contains(e.Message, "step-level block") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("misplaced block reported %d times, want once", n)
	}
}

func TestLogProgressFrameParameter(t *testing.T) {
	tests := []struct {
		name  string
		block string
		space string
		want  string
	}{
		{"omitted, one chunk parameter", "{progress: ['(?P<frame>1)']}", lpSpace, "Frame"},
		{
			"named", "{frameParameter: B, progress: ['(?P<frame>1)']}",
			lpSpaceOf("{name: A, type: INT, range: '1-3'}", "{name: B, type: INT, range: '1-3'}"), "B",
		},
		{"no frame pattern", "{progress: ['(?P<percent>1)']}", lpSpace, ""},
		{"frame pattern after a percent one", "{progress: ['(?P<percent>1)', '(?P<framedone>1)']}", lpSpace, "Frame"},
		{"no block", "", lpSpace, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			extra := tc.space
			ext := ""
			if tc.block != "" {
				extra = lpBlock(tc.block) + "\n" + tc.space
				ext = lpDeclared
			}
			tmpl, errs := lpParse(t, ext, extra)
			if tc.block != "" && len(errs) != 0 {
				t.Fatalf("want valid, got %v", errs)
			}
			if got := tmpl.Steps[0].LogProgressFrameParameter(); got != tc.want {
				t.Errorf("LogProgressFrameParameter() = %q, want %q", got, tc.want)
			}
		})
	}
}
