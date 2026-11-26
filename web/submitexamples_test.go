// SPDX-License-Identifier: AGPL-3.0-or-later

package web_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// exampleLiteral matches one entry of EXAMPLES in src/pages/submitExamples.ts:
// a key, bare or quoted, and its template literal.
var exampleLiteral = regexp.MustCompile("(?m)^  '?([a-z-]+)'?: `([^`]*)`,$")

// exampleLabel matches one entry of EXAMPLE_LABELS: a key and its quoted label.
var exampleLabel = regexp.MustCompile(`(?m)^  '?([a-z-]+)'?: (?:'[^']*'|"[^"]*"),$`)

// TestSubmitExamples_Validate checks that every template the Submit page's
// "Load example" menu offers passes the template validation the server runs at
// submission, so the menu never hands an artist a job that cannot be submitted.
func TestSubmitExamples_Validate(t *testing.T) {
	src, err := os.ReadFile("src/pages/submitExamples.ts")
	if err != nil {
		t.Fatal(err)
	}
	// The menu labels are a Record<ExampleKey, string>, so the compiler keeps
	// them one per example; matching the two key lists shows the extraction
	// found every example.
	var labels, examples []string
	for _, m := range exampleLabel.FindAllSubmatch(src, -1) {
		labels = append(labels, string(m[1]))
	}
	matches := exampleLiteral.FindAllSubmatch(src, -1)
	for _, m := range matches {
		examples = append(examples, string(m[1]))
	}
	if len(examples) == 0 || !slices.Equal(labels, examples) {
		t.Fatalf("examples %v, labels %v; want the same non-empty keys in the same order. "+
			"Has the file's layout changed?", examples, labels)
	}
	for _, m := range matches {
		name, body := string(m[1]), string(m[2])
		t.Run(name, func(t *testing.T) {
			if strings.Contains(body, "${") || strings.Contains(body, `\`) {
				t.Fatal("the example uses a template-literal escape or substitution; the test reads it verbatim")
			}
			tmpl, err := openjd.Parse([]byte(body), openjd.FormatYAML)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if errs := openjd.Validate(tmpl); len(errs) != 0 {
				t.Errorf("Validate: %v", errs)
			}
		})
	}
}
