// SPDX-License-Identifier: AGPL-3.0-or-later

package web_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// exampleLiteral matches one entry of EXAMPLES in src/pages/submitExamples.ts:
// a key, bare or quoted, and its template literal.
var exampleLiteral = regexp.MustCompile("(?m)^  '?([a-z-]+)'?: `([^`]*)`,$")

// TestSubmitExamples_Validate checks that every template the Submit page's
// "Load example" menu offers passes the template validation the server runs at
// submission, so the menu never hands an artist a job that cannot be submitted.
func TestSubmitExamples_Validate(t *testing.T) {
	src, err := os.ReadFile("src/pages/submitExamples.ts")
	if err != nil {
		t.Fatal(err)
	}
	keys := regexp.MustCompile(`export type ExampleKey = (.*)`).FindSubmatch(src)
	if keys == nil {
		t.Fatal("no ExampleKey type in submitExamples.ts")
	}
	want := strings.Count(string(keys[1]), "'") / 2
	matches := exampleLiteral.FindAllSubmatch(src, -1)
	if len(matches) != want {
		t.Fatalf("found %d examples, ExampleKey names %d; has the file's layout changed?", len(matches), want)
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
