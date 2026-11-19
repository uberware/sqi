// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// rangeListTemplate is an EXPR template with one task parameter of type typ
// whose range list is rangeYAML.
func rangeListTemplate(typ, rangeYAML string) string {
	return fmt.Sprintf(`
specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: RangeListItems
parameterDefinitions:
  - {name: Extra, type: "LIST[INT]", default: [7, 8]}
  - {name: Inc, type: BOOL, default: false}
  - {name: N, type: INT, default: 4}
steps:
  - name: Step1
    parameterSpace:
      taskParameterDefinitions:
        - name: X
          type: %s
          range: %s
    script:
      actions:
        onRun:
          command: echo
`, typ, rangeYAML)
}

// resolveRangeList validates the template, binds its parameters (defaults
// overridden by values) and resolves the step's parameter space, returning the
// resolved range list.
func resolveRangeList(t *testing.T, yaml string, values map[string]string) ([]string, error) {
	t.Helper()
	tmpl := mustParse(t, yaml)
	if errs := openjd.Validate(tmpl); len(errs) != 0 {
		return nil, errs
	}
	params, errs := openjd.BindJobParameters(tmpl.ParameterDefinitions, values)
	if len(errs) != 0 {
		t.Fatalf("BindJobParameters() = %v", errs)
	}
	step := &tmpl.Steps[0]
	ps, errs := openjd.ResolveParameterSpaceParams(tmpl, step, step.ParameterSpace, params)
	if len(errs) != 0 {
		return nil, errs
	}
	return ps.TaskParameterDefinitions[0].RangeList, nil
}

// TestRangeListItems_LoneExpressionIsAListItem pins Expression Language
// §1.3.12 (as §1.3.2's list items): an element of a range list that is exactly
// "{{<expr>}}" has the target type T? | list[T], so a null result drops the
// element and a list result is flattened into one element per member. For
// FLOAT the target also admits int, kept as written. An element with text
// around its expression resolves to one string, as without EXPR.
func TestRangeListItems_LoneExpressionIsAListItem(t *testing.T) {
	tests := []struct {
		name   string
		typ    string
		rng    string
		values map[string]string
		want   []string
	}{
		{"a list parameter flattens, a null drops", "INT", `[1, "{{ Param.Extra }}", "{{ 100 if Param.Inc else null }}"]`, nil, []string{"1", "7", "8"}},
		{"the conditional element is kept when true", "INT", `[1, "{{ Param.Extra }}", "{{ 100 if Param.Inc else null }}"]`, map[string]string{"Inc": "true"}, []string{"1", "7", "8", "100"}},
		{"text around the expression is one string", "INT", `["{{ Param.N }}0", 5]`, nil, []string{"40", "5"}},
		{"FLOAT keeps an int as written and converts list members", "FLOAT", `["{{ 2 }}", "{{ [1, 2.5] }}"]`, nil, []string{"2", "1.0", "2.5"}},
		{"STRING flattens and drops", "STRING", `["a", "{{ ['b', 'c'] }}", "{{ null }}"]`, nil, []string{"a", "b", "c"}},
		{"CHUNK[INT] is an INT list item", "INT", `["{{ range(2) }}"]`, nil, []string{"0", "1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveRangeList(t, rangeListTemplate(tc.typ, tc.rng), tc.values)
			if err != nil {
				t.Fatalf("resolve = %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("resolved range = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRangeListItems_Errors(t *testing.T) {
	tests := []struct {
		name string
		typ  string
		rng  string
		want string
	}{
		{"a list of the wrong type", "INT", `[1, "{{ ['a'] }}"]`, "/range/1"},
		{"a list emptied by null", "INT", `["{{ null }}"]`, "range"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveRangeList(t, rangeListTemplate(tc.typ, tc.rng), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("resolve error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// TestValidate_RangeElementReferencesMustBeDeclared pins Template Schemas
// §3.4.1.1 and §7.3 for a template without EXPR: a <TaskParameterStringValue>
// element of a range is a format string, and a format string may only
// reference variables defined at its location.
func TestValidate_RangeElementReferencesMustBeDeclared(t *testing.T) {
	tests := []struct {
		name, typ, rng, want string
	}{
		{"INT list element", "INT", `[1, "{{Param.Missing}}"]`, "/range/1"},
		{"FLOAT list element", "FLOAT", `[1.5, "{{Param.Missing}}"]`, "/range/1"},
		{"range expression", "INT", `"1-{{Param.Missing}}"`, "/range"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustParse(t, fmt.Sprintf(`
specificationVersion: jobtemplate-2023-09
name: Undeclared
parameterDefinitions:
  - {name: Value, type: INT, default: 10}
steps:
  - name: Step1
    parameterSpace:
      taskParameterDefinitions:
        - {name: X, type: %s, range: %s}
    script:
      actions:
        onRun:
          command: echo
`, tc.typ, tc.rng))
			assertValidationContains(t, tmpl, tc.want)
			assertValidationContains(t, tmpl, "Param.Missing")
		})
	}
	ok := mustParse(t, `
specificationVersion: jobtemplate-2023-09
name: Declared
parameterDefinitions:
  - {name: Value, type: INT, default: 10}
steps:
  - name: Step1
    parameterSpace:
      taskParameterDefinitions:
        - {name: X, type: INT, range: [1, "{{Param.Value}}", "{{RawParam.Value}}"]}
    script:
      actions:
        onRun:
          command: echo
`)
	if errs := openjd.Validate(ok); len(errs) != 0 {
		t.Errorf("Validate() = %v, want no errors for declared references", errs)
	}
}

// TestRangeListItems_FlatteningIsBoundedByTheTaskLimit pins that flattening
// stops as soon as the resolved list passes maxTasksPerStep, before the rest
// of the list is built: each list item is cheap within the expression limits,
// but together they can name far more values than any step may have tasks.
func TestRangeListItems_FlatteningIsBoundedByTheTaskLimit(t *testing.T) {
	entries := make([]string, 101)
	for i := range entries {
		entries[i] = `"{{ range(9990) }}"`
	}
	rng := "[" + strings.Join(entries, ", ") + "]"
	_, err := resolveRangeList(t, rangeListTemplate("INT", rng), nil)
	if err == nil || !strings.Contains(err.Error(), "more than 1000000 values") {
		t.Fatalf("resolve error = %v, want the task limit reported", err)
	}
}
