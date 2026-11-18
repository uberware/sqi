// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// Template Schemas §3.3: an amount's or attribute's name is a format string,
// subject to the capability-name constraints after it is resolved, and no two
// amounts (or attributes) may share a name after resolution. Every
// host-requirement format string is resolved at Job creation, against the
// bound job parameters.

func hostReqTemplate(ext, hostReqs string) string {
	return fmt.Sprintf(`
specificationVersion: jobtemplate-2023-09
%s
name: HostReqFormatStrings
parameterDefinitions:
  - {name: AmountName, type: STRING, default: amount.custom}
  - {name: AttrName, type: STRING, default: attr.custom}
  - {name: Min, type: INT, default: 2}
  - {name: Value, type: STRING, default: blue}
steps:
  - name: Step1
    hostRequirements:
%s
    script:
      actions:
        onRun:
          command: echo
`, ext, hostReqs)
}

func TestValidate_CapabilityNamesAreFormatStrings(t *testing.T) {
	tests := []struct {
		name, ext, hr string
	}{
		{"amount name", "", "      amounts: [{name: \"{{Param.AmountName}}\", min: 1}]"},
		{"attribute name", "", "      attributes: [{name: \"{{Param.AttrName}}\", anyOf: [x]}]"},
		{"partial names", "", "      amounts: [{name: \"amount.{{Param.Value}}\", min: 1}]\n      attributes: [{name: \"attr.{{Param.Value}}\", anyOf: [x]}]"},
		{"two names that only resolve later", "", "      amounts: [{name: \"{{Param.AmountName}}\", min: 1}, {name: \"{{Param.AmountName}}\", min: 1}]"},
		{"an EXPR expression", "extensions: [EXPR]", "      amounts: [{name: \"{{ 'amount.' + Param.Value }}\", min: 1}]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustParse(t, hostReqTemplate(tc.ext, tc.hr))
			if errs := openjd.Validate(tmpl); len(errs) != 0 {
				t.Fatalf("Validate() = %v, want no errors", errs)
			}
		})
	}
}

func TestValidate_CapabilityNameFormatStringErrors(t *testing.T) {
	tests := []struct {
		name, ext, hr, want string
	}{
		{"an undeclared parameter", "", "      amounts: [{name: \"{{Param.Missing}}\", min: 1}]", "/steps/0/hostRequirements/amounts/0/name"},
		{"an out-of-scope reference", "", "      attributes: [{name: \"{{Session.WorkingDirectory}}\", anyOf: [x]}]", "/steps/0/hostRequirements/attributes/0/name"},
		{"an EXPR unknown symbol", "extensions: [EXPR]", "      amounts: [{name: \"{{ Param.Missing }}\", min: 1}]", "/steps/0/hostRequirements/amounts/0/name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustParse(t, hostReqTemplate(tc.ext, tc.hr))
			assertValidationContains(t, tmpl, tc.want)
		})
	}
}

func submitHostReqTemplate(t *testing.T, ext, hr string, params map[string]string) (*openjd.SubmitResult, error) {
	t.Helper()
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	sub := openjd.NewSubmitter(st)
	return sub.Submit(t.Context(), hostReqTemplate(ext, hr), store.TemplateFormatYAML, openjd.SubmitOptions{
		FarmID: farmID, QueueID: queueID, Parameters: params,
	})
}

// TestSubmit_HostRequirementFormatStringsAreResolved pins that what reaches
// the scheduler is the RESOLVED host requirement: names, amount bounds and
// attribute values alike. A bound left as "{{Param.Min}}" could never parse,
// so the step could never be scheduled.
func TestSubmit_HostRequirementFormatStringsAreResolved(t *testing.T) {
	for _, ext := range []string{"", "extensions: [EXPR]"} {
		t.Run(fmt.Sprintf("ext=%q", ext), func(t *testing.T) {
			res, err := submitHostReqTemplate(t, ext,
				"      amounts: [{name: \"{{Param.AmountName}}\", min: \"{{Param.Min}}\"}]\n"+
					"      attributes: [{name: \"{{Param.AttrName}}\", anyOf: [\"{{Param.Value}}\"]}]", nil)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			hr := res.Steps[0].HostRequirements
			if hr == nil || len(hr.Amounts) != 1 || len(hr.Attributes) != 1 {
				t.Fatalf("stored host requirements = %+v", hr)
			}
			a, at := hr.Amounts[0], hr.Attributes[0]
			if a.Name != "amount.custom" || a.Min == nil || *a.Min != "2" {
				t.Errorf("stored amount = %q min %v, want amount.custom min 2", a.Name, a.Min)
			}
			if at.Name != "attr.custom" || len(at.AnyOf) != 1 || at.AnyOf[0] != "blue" {
				t.Errorf("stored attribute = %q anyOf %q, want attr.custom anyOf [blue]", at.Name, at.AnyOf)
			}
		})
	}
}

func TestSubmit_ResolvedCapabilityNamesAreValidated(t *testing.T) {
	tests := []struct {
		name   string
		hr     string
		params map[string]string
		want   string
	}{
		{"a name without the amount prefix", "      amounts: [{name: \"{{Param.AmountName}}\", min: 1}]", map[string]string{"AmountName": "custom"}, `must begin with "amount."`},
		{"a reserved scope", "      amounts: [{name: \"{{Param.AmountName}}\", min: 1}]", map[string]string{"AmountName": "amount.worker.nonsense"}, "reserved"},
		{"names that resolve to the same capability", "      amounts: [{name: \"{{Param.AmountName}}\", min: 1}, {name: amount.CUSTOM, min: 1}]", nil, "duplicate amount capability name"},
		{"an attribute name that is too long", "      attributes: [{name: \"{{Param.AttrName}}\", anyOf: [x]}]", map[string]string{"AttrName": "attr." + strings.Repeat("a", 200)}, "/steps/0/hostRequirements/attributes/0/name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := submitHostReqTemplate(t, "", tc.hr, tc.params)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Submit() error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}
