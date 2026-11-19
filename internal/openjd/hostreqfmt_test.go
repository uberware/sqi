// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"errors"
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
// the scheduler is the resolved host requirement: names, amount bounds and
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

// TestSubmit_AmountBoundsAreStoredWithoutWhiteSpace pins that an amount bound
// written with the white space Template Schemas §7.5 allows reaches the
// scheduler as a plain number, whether or not it was a format string: the
// scheduler compares it with worker capabilities as written.
func TestSubmit_AmountBoundsAreStoredWithoutWhiteSpace(t *testing.T) {
	res, err := submitHostReqTemplate(t, "", "      amounts: [{name: amount.custom, min: \" 4 \", max: \"8\\n\"}]", nil)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	a := res.Steps[0].HostRequirements.Amounts[0]
	if a.Min == nil || *a.Min != "4" || a.Max == nil || *a.Max != "8" {
		t.Errorf("stored bounds = %v, %v; want 4 and 8", a.Min, a.Max)
	}
}

// TestSubmit_ResolvedHostRequirementsHoldNoFormatStrings pins that what a
// host-requirement format string resolves to is checked as resolved: a
// parameter value that itself looks like a format string is not a reference
// still to be resolved, so it cannot slip past the capability-name and value
// constraints by looking like one. Neither base-spec nor EXPR substitution
// re-reads its own output, so such text would otherwise reach the scheduler,
// which could never match it.
func TestSubmit_ResolvedHostRequirementsHoldNoFormatStrings(t *testing.T) {
	tests := []struct {
		name   string
		ext    string
		hr     string
		params map[string]string
	}{
		{"an amount name", "", "      amounts: [{name: \"{{Param.AmountName}}\", min: 1}]", map[string]string{"AmountName": "{{amount.worker.vcpu}}"}},
		{"an attribute name", "", "      attributes: [{name: \"{{Param.AttrName}}\", anyOf: [x]}]", map[string]string{"AttrName": "{{nope}}"}},
		{"an attribute value", "", "      attributes: [{name: attr.custom, anyOf: [\"{{Param.Value}}\"]}]", map[string]string{"Value": "{{x}}"}},
		{"an EXPR amount name", "extensions: [EXPR]", "      amounts: [{name: \"{{ Param.AmountName }}\", min: 1}]", map[string]string{"AmountName": "{{amount.worker.vcpu}}"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := submitHostReqTemplate(t, tc.ext, tc.hr, tc.params)
			if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
				t.Fatalf("Submit() error = %v, want a SubmitValidationError", err)
			}
			if !strings.Contains(err.Error(), "format string") {
				t.Errorf("Submit() error = %v, want it to say the resolved text holds a format string", err)
			}
		})
	}
}

// TestSubmit_StorageLocationCoverageUsesTheResolvedComputeLocation pins that a
// step's storage-location roots are checked against the compute location it
// will run in: the resolved attr.worker.computelocation, not the
// format string that produces it.
func TestSubmit_StorageLocationCoverageUsesTheResolvedComputeLocation(t *testing.T) {
	template := func(site string) string {
		return fmt.Sprintf(`
specificationVersion: jobtemplate-2023-09
name: LocJob
parameterDefinitions:
  - {name: Site, type: STRING, default: %s}
steps:
  - name: Step1
    hostRequirements:
      attributes: [{name: attr.worker.computelocation, anyOf: ["{{Param.Site}}"]}]
    script:
      actions:
        onRun: {command: cp, args: ["loc://site_assets/a.ma", "b.ma"]}
`, site)
	}
	submit := func(t *testing.T, site string) error {
		t.Helper()
		st := fake.New()
		farmID, queueID := seedSubmitPrereqs(t, st)
		if _, err := st.CreateStorageLocation(t.Context(), store.StorageLocation{
			ID: "loc-1", Name: "site_assets", Type: store.StorageLocationTypeFilesystem,
			Roots: map[string]string{"windows_workers": `Z:\assets`},
		}); err != nil {
			t.Fatal(err)
		}
		_, err := openjd.NewSubmitter(st).Submit(t.Context(), template(site), store.TemplateFormatYAML, openjd.SubmitOptions{
			FarmID: farmID, QueueID: queueID,
		})
		return err
	}
	if err := submit(t, "windows_workers"); err != nil {
		t.Errorf("Submit() with a covered resolved compute location = %v, want nil", err)
	}
	err := submit(t, "linux_workers")
	if err == nil || !strings.Contains(err.Error(), `no root for compute location "linux_workers"`) {
		t.Errorf("Submit() with an uncovered resolved compute location = %v, want a coverage error naming it", err)
	}
}
