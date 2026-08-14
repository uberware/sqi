// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd/intrange"
)

// TestParseBoolParamValue transcribes RFC 0007's accepted-values table for
// <JobBoolParameterDefinition>, and every spelling in the 2.9--bool-param.yaml
// fixture.
//
// The .invalid fixtures for this rule (2.9--bool-param-{int,float,string}-
// invalid) already "pass" today by being rejected for the WRONG reason — an
// unknown type — so the aggregate conformance score cannot prove this table
// works. This test is what proves it; the three values those fixtures use
// (2, 0.5, "maybe") are in the invalid set below by name.
func TestParseBoolParamValue(t *testing.T) {
	tests := []struct {
		in    string
		want  bool
		valid bool
	}{
		// Bool literals.
		{"true", true, true},
		{"false", false, true},
		// Int values.
		{"1", true, true},
		{"0", false, true},
		// Float values.
		{"1.0", true, true},
		{"0.0", false, true},
		// String true/false, every casing the fixture uses.
		{"TRUE", true, true},
		{"FALSE", false, true},
		{"True", true, true},
		{"False", false, true},
		// String yes/no.
		{"yes", true, true},
		{"no", false, true},
		{"YES", true, true},
		{"NO", false, true},
		{"Yes", true, true},
		{"No", false, true},
		// String on/off.
		{"on", true, true},
		{"off", false, true},
		{"ON", true, true},
		{"OFF", false, true},
		{"On", true, true},
		{"Off", false, true},

		// Rejected. The first three are the .invalid fixtures' own values.
		{"2", false, false},
		{"0.5", false, false},
		{"maybe", false, false},
		{"", false, false},
		{"-1", false, false},
		{"1.5", false, false},
		{"y", false, false},
		{"t", false, false},
		{"2.0", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := parseBoolParamValue(tt.in)
			if ok != tt.valid {
				t.Fatalf("parseBoolParamValue(%q) ok = %v, want %v", tt.in, ok, tt.valid)
			}
			if ok && got != tt.want {
				t.Errorf("parseBoolParamValue(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestValidateBoolParamConstraints_RejectsAllowedValues pins the RFC's explicit
// carve-out: BOOL does not support allowedValues, because restricting a
// two-valued domain to one value provides no meaningful constraint.
func TestValidateBoolParamConstraints_RejectsAllowedValues(t *testing.T) {
	p := JobParameter{
		Name: "UseGpu", Type: JobParamTypeBool,
		AllowedValues: []string{"true"}, AllowedValuesSet: true,
	}
	errs := validateBoolParamConstraints(p, "/parameterDefinitions/0")
	if len(errs) == 0 {
		t.Fatal("allowedValues on a BOOL parameter was accepted")
	}
	if errs[0].Pointer != "/parameterDefinitions/0/allowedValues" {
		t.Errorf("Pointer = %q, want /parameterDefinitions/0/allowedValues", errs[0].Pointer)
	}
}

func TestValidateBoolParamConstraints_Default(t *testing.T) {
	bad := "maybe"
	errs := validateBoolParamConstraints(
		JobParameter{Name: "B", Type: JobParamTypeBool, Default: &bad},
		"/parameterDefinitions/0",
	)
	if len(errs) == 0 {
		t.Fatal(`default "maybe" on a BOOL parameter was accepted`)
	}
	if errs[0].Pointer != "/parameterDefinitions/0/default" {
		t.Errorf("Pointer = %q, want .../default", errs[0].Pointer)
	}

	good := "yes"
	if errs := validateBoolParamConstraints(
		JobParameter{Name: "B", Type: JobParamTypeBool, Default: &good},
		"/parameterDefinitions/0",
	); len(errs) != 0 {
		t.Errorf(`default "yes" was rejected: %v`, errs)
	}
}

// TestValidateBoolParamConstraints_RejectsScalarBounds pins that the numeric
// and length bounds of the scalar types are not silently ignored on a BOOL.
// RFC 0007's <JobBoolParameterDefinition> schema lists no such fields, and a
// bound nothing enforces is worse than a rejected one: the author believes it
// is in force.
func TestValidateBoolParamConstraints_RejectsScalarBounds(t *testing.T) {
	s, n := "1", 1
	tests := []struct {
		name string
		p    JobParameter
		want string
	}{
		{"minValue", JobParameter{Type: JobParamTypeBool, MinValue: &s}, "/p/minValue"},
		{"maxValue", JobParameter{Type: JobParamTypeBool, MaxValue: &s}, "/p/maxValue"},
		{"minLength", JobParameter{Type: JobParamTypeBool, MinLength: &n}, "/p/minLength"},
		{"maxLength", JobParameter{Type: JobParamTypeBool, MaxLength: &n}, "/p/maxLength"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateBoolParamConstraints(tt.p, "/p")
			if len(errs) == 0 {
				t.Fatalf("%s on a BOOL parameter was accepted", tt.name)
			}
			if errs[0].Pointer != tt.want {
				t.Errorf("Pointer = %q, want %q", errs[0].Pointer, tt.want)
			}
		})
	}
}

// TestValidateRangeExprParamConstraints_UsesTheSpecPolicy records this
// sub-project's most consequential ruling.
//
// internal/openjd deliberately parses <IntRangeExpr> more strictly than the
// spec: openjdRangePolicy sets PositiveStepOnly and AscendingOnly, rejecting
// start > end and a negative step (this repo's CLAUDE.md records and preserves
// both on purpose). A RANGE_EXPR PARAMETER must NOT use that policy. The
// conformance fixture 2.10--range-expr-param.yaml declares NegativeStep
// "10-1:-1" and NegativeRangeNegativeStep "-1--10:-1"; under the strict policy
// the fixture cannot pass.
//
// The permissive policy is also what the value meets downstream. E4b ruled
// that the LONE whole-field form range: "{{Param.FrameRange}}" evaluates as a
// VALUE through range_expr's own list[int] coercion and never re-enters
// <IntRangeExpr> text (resolve.go), and expr.ValueFromText's CodeRangeExpr
// case already calls the permissive expr.RangeExpr. The strict policy keeps
// governing literal base-spec range text, including text assembled from an
// EMBEDDED reference — E4b ruled on that separately and F1 does not touch it.
func TestValidateRangeExprParamConstraints_UsesTheSpecPolicy(t *testing.T) {
	// Every default in 2.10--range-expr-param.yaml, in fixture order.
	valid := []string{
		"42", "0", "-100",
		"1-100", "-10-10",
		"0-100:10", "1-9:3",
		"10-1:-1",   // descending: rejected by openjdRangePolicy, legal here
		"-1--10:-1", // descending AND negative step: same
		"1,3,5,7",
		"1-10,20-30:2,42",
		"0-3:3,5-10:5,12,13,14,15",
		"20-29,0-9,10-19",
		" 0 - 1 : 1, 2 - 100 : 1",
		"9999999",
		"1-10",
		"1-100",
	}
	for _, s := range valid {
		t.Run("valid/"+s, func(t *testing.T) {
			d := s
			errs := validateRangeExprParamConstraints(
				JobParameter{Name: "R", Type: JobParamTypeRangeExpr, Default: &d},
				"/parameterDefinitions/0",
			)
			if len(errs) != 0 {
				t.Errorf("default %q rejected: %v", s, errs)
			}
		})
	}

	invalid := []string{"", "abc", "1-", "-", "1..5", "1-2-3"}
	for _, s := range invalid {
		t.Run("invalid/"+s, func(t *testing.T) {
			d := s
			errs := validateRangeExprParamConstraints(
				JobParameter{Name: "R", Type: JobParamTypeRangeExpr, Default: &d},
				"/parameterDefinitions/0",
			)
			if len(errs) == 0 {
				t.Errorf("default %q accepted; it is not a valid <IntRangeExpr>", s)
			}
		})
	}
}

// TestValidateRangeExprParamConstraints_EmptyElementsAreTolerated records a
// pre-existing leniency rather than endorsing it.
//
// The <IntRangeExpr> grammar has no empty element and no empty step, so
// "1,,2" and "1-2:" are both strictly invalid — but the shared parser tolerates
// each: intrange.ParseWithPolicy skips empty comma-separated parts (intrange.go:
// `if part == "" { continue }`) and reads a trailing colon as "no step given",
// yielding [1 2] and [1-2] respectively. It has done so for both the base-spec
// range path and the expression language since before this sub-project. An
// earlier draft of the test above asserted rejection for both and failed here.
//
// F1 does NOT tighten it. The leniency is in the shared leaf package, so
// rejecting "1,,2" for a RANGE_EXPR parameter while a task parameter's
// range: "1,,2" keeps working would be incoherent, and tightening BOTH would
// be an acceptance change to the base spec — templates accepted today would
// start being rejected. That is exactly the class of change this repo's
// CLAUDE.md forbids making in passing.
func TestValidateRangeExprParamConstraints_EmptyElementsAreTolerated(t *testing.T) {
	for _, d := range []string{"1,,2", "1-2:"} {
		t.Run(d, func(t *testing.T) {
			def := d
			errs := validateRangeExprParamConstraints(
				JobParameter{Name: "R", Type: JobParamTypeRangeExpr, Default: &def}, "/p",
			)
			if len(errs) != 0 {
				t.Fatalf("%q rejected: %v\nIf this is now rejected, the shared intrange "+
					"leniency changed — check that the base-spec range path changed with "+
					"it, because that is a base-spec acceptance change.", d, errs)
			}

			// Equally tolerated by the strict base-spec policy: the leniency is
			// in the shared parser, not in either policy.
			if _, err := intrange.ParseWithPolicy(d, openjdRangePolicy); err != nil {
				t.Errorf("openjdRangePolicy rejected %q (%v), so the leniency is NOT "+
					"shared and this test's premise is wrong", d, err)
			}
		})
	}
}

// TestValidateRangeExprParamConstraints_StrictPolicyWouldReject is the other
// half of the ruling above, stated as an executable fact rather than a claim
// in a comment: these two defaults ARE rejected by internal/openjd's own
// policy, so choosing between the policies is a real decision and not a
// distinction without a difference.
//
// If this test ever stops failing under the strict policy, the two policies
// have converged and the ruling above needs revisiting.
func TestValidateRangeExprParamConstraints_StrictPolicyWouldReject(t *testing.T) {
	for _, s := range []string{"10-1:-1", "-1--10:-1"} {
		t.Run(s, func(t *testing.T) {
			if _, err := intrange.ParseWithPolicy(s, openjdRangePolicy); err == nil {
				t.Errorf("openjdRangePolicy accepted %q; the two policies have "+
					"converged and validateRangeExprParamConstraints' choice of the "+
					"permissive one no longer distinguishes anything", s)
			}
			if _, err := intrange.Parse(s); err != nil {
				t.Errorf("the spec's permissive policy rejected %q: %v", s, err)
			}
		})
	}
}

// TestValidateRangeExprParamConstraints_Length pins that minLength/maxLength
// bound the expression's STRING length (RFC 0007), with a default maximum of
// 1024 when the template declares none. A range expression is a compact
// notation precisely so that "1-1000000" is nine characters, so bounding the
// expanded element count here would be the wrong measure entirely.
func TestValidateRangeExprParamConstraints_Length(t *testing.T) {
	d := "1-100" // five characters
	minLen, maxLen := 10, 20

	if errs := validateRangeExprParamConstraints(
		JobParameter{Name: "R", Type: JobParamTypeRangeExpr, Default: &d, MinLength: &minLen},
		"/p",
	); len(errs) == 0 {
		t.Error("a 5-character default passed minLength 10")
	}
	if errs := validateRangeExprParamConstraints(
		JobParameter{Name: "R", Type: JobParamTypeRangeExpr, Default: &d, MaxLength: &maxLen},
		"/p",
	); len(errs) != 0 {
		t.Errorf("a 5-character default failed maxLength 20: %v", errs)
	}

	// The implicit 1024 cap applies when the template declares no maxLength.
	long := "1-" + strings.Repeat("9", 1024)
	if errs := validateRangeExprParamConstraints(
		JobParameter{Name: "R", Type: JobParamTypeRangeExpr, Default: &long},
		"/p",
	); len(errs) == 0 {
		t.Error("a default longer than 1024 characters passed with no maxLength declared")
	}
}

// TestValidateRangeExprParamConstraints_RejectsInapplicableFields mirrors the
// BOOL case: RFC 0007's <JobRangeExprParameterDefinition> schema lists only
// minLength and maxLength, so a numeric bound or an allowedValues list on one
// is a constraint nothing would ever enforce.
func TestValidateRangeExprParamConstraints_RejectsInapplicableFields(t *testing.T) {
	s := "1"
	tests := []struct {
		name string
		p    JobParameter
		want string
	}{
		{"minValue", JobParameter{Type: JobParamTypeRangeExpr, MinValue: &s}, "/p/minValue"},
		{"maxValue", JobParameter{Type: JobParamTypeRangeExpr, MaxValue: &s}, "/p/maxValue"},
		{
			"allowedValues",
			JobParameter{Type: JobParamTypeRangeExpr, AllowedValuesSet: true},
			"/p/allowedValues",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateRangeExprParamConstraints(tt.p, "/p")
			if len(errs) == 0 {
				t.Fatalf("%s on a RANGE_EXPR parameter was accepted", tt.name)
			}
			if errs[0].Pointer != tt.want {
				t.Errorf("Pointer = %q, want %q", errs[0].Pointer, tt.want)
			}
		})
	}
}

// TestValidateListParamConstraints transcribes each 2.11-2.16 .invalid
// fixture's rule as a direct test.
//
// Those fixtures already "pass" today by being rejected as an unknown type, so
// the aggregate conformance score cannot prove any of these rules works. Each
// case below names the fixture it stands in for; that mapping is the whole
// point of the file.
func TestValidateListParamConstraints(t *testing.T) {
	tests := []struct {
		name    string
		p       JobParameter
		wantErr bool
		wantPtr string // when wantErr, the pointer the FIRST error must carry
	}{
		{
			name: "valid list of strings",
			p:    JobParameter{Type: JobParamTypeListString, Default: new(`["a","b"]`)},
		},
		{
			name: "valid empty list",
			p:    JobParameter{Type: JobParamTypeListString, Default: new(`[]`)},
		},
		{
			name: "no default at all",
			p:    JobParameter{Type: JobParamTypeListString},
		},
		{ // 2.11--list-string-too-short.invalid
			name: "below minLength",
			p: JobParameter{
				Type: JobParamTypeListString, Default: new(`["a"]`),
				MinLength: new(2),
			},
			wantErr: true, wantPtr: "/p/default",
		},
		{ // 2.11--list-string-too-long.invalid
			name: "above maxLength",
			p: JobParameter{
				Type: JobParamTypeListString, Default: new(`["a","b","c"]`),
				MaxLength: new(2),
			},
			wantErr: true, wantPtr: "/p/default",
		},
		{ // 2.11--list-string-item-not-in-allowed.invalid
			name: "item not in item.allowedValues",
			p: JobParameter{
				Type: JobParamTypeListString, Default: new(`["a","z"]`),
				Item: &ItemConstraint{AllowedValues: []string{"a", "b"}, AllowedValuesSet: true},
			},
			wantErr: true, wantPtr: "/p/default/1",
		},
		{ // 2.11--list-string-item-too-short.invalid
			name: "item below item.minLength",
			p: JobParameter{
				Type: JobParamTypeListString, Default: new(`["ab","c"]`),
				Item: &ItemConstraint{MinLength: new(2)},
			},
			wantErr: true, wantPtr: "/p/default/1",
		},
		{ // 2.11--list-string-item-too-long.invalid
			name: "item above item.maxLength",
			p: JobParameter{
				Type: JobParamTypeListString, Default: new(`["abcdef"]`),
				Item: &ItemConstraint{MaxLength: new(3)},
			},
			wantErr: true, wantPtr: "/p/default/0",
		},
		{ // 2.12--list-path-wrong-item-type.invalid — default: [123, 456]
			name:    "number in a list of paths",
			p:       JobParameter{Type: JobParamTypeListPath, Default: new(`[123,456]`)},
			wantErr: true, wantPtr: "/p/default/0",
		},
		{
			name:    "number in a list of strings",
			p:       JobParameter{Type: JobParamTypeListString, Default: new(`["a",7]`)},
			wantErr: true, wantPtr: "/p/default/1",
		},
		{ // 2.13--list-int-wrong-item-type.invalid — default: ["not", "ints"]
			name:    "string in a list of ints",
			p:       JobParameter{Type: JobParamTypeListInt, Default: new(`["not","ints"]`)},
			wantErr: true, wantPtr: "/p/default/0",
		},
		{
			name:    "fractional number in a list of ints",
			p:       JobParameter{Type: JobParamTypeListInt, Default: new(`[1,2.5]`)},
			wantErr: true, wantPtr: "/p/default/1",
		},
		{ // 2.14--list-float-wrong-item-type.invalid — default: ["not", "floats"]
			name:    "string in a list of floats",
			p:       JobParameter{Type: JobParamTypeListFloat, Default: new(`["not","floats"]`)},
			wantErr: true, wantPtr: "/p/default/0",
		},
		{
			name: "whole number is a valid float",
			p:    JobParameter{Type: JobParamTypeListFloat, Default: new(`[1,2.5]`)},
		},
		{ // 2.13--list-int-item-below-min.invalid
			name: "int item below item.minValue",
			p: JobParameter{
				Type: JobParamTypeListInt, Default: new(`[5,1]`),
				Item: &ItemConstraint{MinValue: new("2")},
			},
			wantErr: true, wantPtr: "/p/default/1",
		},
		{ // 2.13--list-int-item-not-in-allowed.invalid
			name: "int item not in item.allowedValues",
			p: JobParameter{
				Type: JobParamTypeListInt, Default: new(`[1,9]`),
				Item: &ItemConstraint{AllowedValues: []string{"1", "2"}, AllowedValuesSet: true},
			},
			wantErr: true, wantPtr: "/p/default/1",
		},
		{ // 2.15--list-bool-wrong-item-type.invalid — default: ["maybe", "perhaps"]
			name:    "non-boolean in a list of bools",
			p:       JobParameter{Type: JobParamTypeListBool, Default: new(`["maybe"]`)},
			wantErr: true, wantPtr: "/p/default/0",
		},
		{
			// RFC 0007: a LIST[BOOL] item accepts the SAME spellings as a BOOL
			// parameter, so a quoted "yes" and a bare 1 are both valid. This is
			// why bool elements are checked as TEXT while every other element
			// type is checked by JSON type.
			name: "bool spellings are accepted in a list of bools",
			p:    JobParameter{Type: JobParamTypeListBool, Default: new(`[true,"yes",1,"off"]`)},
		},
		{
			name: "valid nested list",
			p:    JobParameter{Type: JobParamTypeListListInt, Default: new(`[[1,2],[3]]`)},
		},
		{ // 2.16--list-list-int-inner-too-short.invalid
			name: "inner list below item.minLength",
			p: JobParameter{
				Type: JobParamTypeListListInt, Default: new(`[[1,2],[3]]`),
				Item: &ItemConstraint{MinLength: new(2)},
			},
			wantErr: true, wantPtr: "/p/default/1",
		},
		{ // 2.16--list-list-int-inner-too-long.invalid
			name: "inner list above item.maxLength",
			p: JobParameter{
				Type: JobParamTypeListListInt, Default: new(`[[1,2,3]]`),
				Item: &ItemConstraint{MaxLength: new(2)},
			},
			wantErr: true, wantPtr: "/p/default/0",
		},
		{ // 2.16--list-list-int-inner-item-below-min.invalid
			name: "inner element below item.item.minValue",
			p: JobParameter{
				Type: JobParamTypeListListInt, Default: new(`[[5],[1]]`),
				Item: &ItemConstraint{Item: &ItemConstraint{MinValue: new("2")}},
			},
			wantErr: true, wantPtr: "/p/default/1/0",
		},
		{ // 2.16--list-list-int-inner-item-above-max.invalid
			name: "inner element above item.item.maxValue",
			p: JobParameter{
				Type: JobParamTypeListListInt, Default: new(`[[1],[99]]`),
				Item: &ItemConstraint{Item: &ItemConstraint{MaxValue: new("10")}},
			},
			wantErr: true, wantPtr: "/p/default/1/0",
		},
		{ // 2.16--list-list-int-inner-item-not-in-allowed.invalid — [[99]]
			name: "inner element not in item.item.allowedValues",
			p: JobParameter{
				Type: JobParamTypeListListInt, Default: new(`[[99]]`),
				Item: &ItemConstraint{Item: &ItemConstraint{
					AllowedValues: []string{"1", "2", "3"}, AllowedValuesSet: true,
				}},
			},
			wantErr: true, wantPtr: "/p/default/0/0",
		},
		{ // 2.16--list-list-int-string-in-inner.invalid — [[1, "a"]]
			name:    "string inside an inner list",
			p:       JobParameter{Type: JobParamTypeListListInt, Default: new(`[[1,"a"]]`)},
			wantErr: true, wantPtr: "/p/default/0/1",
		},
		{
			name: "minValue is not legal on a string item",
			p: JobParameter{
				Type: JobParamTypeListString, Default: new(`["a"]`),
				Item: &ItemConstraint{MinValue: new("1")},
			},
			wantErr: true, wantPtr: "/p/item/minValue",
		},
		{
			name: "minLength is not legal on an int item",
			p: JobParameter{
				Type: JobParamTypeListInt, Default: new(`[1]`),
				Item: &ItemConstraint{MinLength: new(1)},
			},
			wantErr: true, wantPtr: "/p/item/minLength",
		},
		{
			name: "a second item level is not legal on a flat list",
			p: JobParameter{
				Type: JobParamTypeListInt, Default: new(`[1]`),
				Item: &ItemConstraint{Item: &ItemConstraint{MinValue: new("1")}},
			},
			wantErr: true, wantPtr: "/p/item/item",
		},
		{
			name: "allowedValues is not legal on a bool item",
			p: JobParameter{
				Type: JobParamTypeListBool, Default: new(`[true]`),
				Item: &ItemConstraint{AllowedValues: []string{"true"}, AllowedValuesSet: true},
			},
			wantErr: true, wantPtr: "/p/item/allowedValues",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateListParamConstraints(tt.p, "/p")
			if tt.wantErr && len(errs) == 0 {
				t.Fatalf("accepted a parameter that must be rejected: %+v", tt.p)
			}
			if !tt.wantErr && len(errs) != 0 {
				t.Fatalf("rejected a valid parameter: %v", errs)
			}
			if tt.wantErr && errs[0].Pointer != tt.wantPtr {
				t.Errorf("Pointer = %q, want %q (all errors: %v)", errs[0].Pointer, tt.wantPtr, errs)
			}
		})
	}
}

// TestValidateListParamConstraints_SkipsFormatStringDefault mirrors the
// scalar types: a default containing "{{" is not known until submission, so
// validating it against the declared type here would reject a template that is
// perfectly valid.
func TestValidateListParamConstraints_SkipsFormatStringDefault(t *testing.T) {
	p := JobParameter{
		Type: JobParamTypeListInt, Default: new(`"{{Param.Other}}"`),
		MinLength: new(5),
	}
	if errs := validateListParamConstraints(p, "/p"); len(errs) != 0 {
		t.Errorf("a format-string default was validated as a literal list: %v", errs)
	}
}

// TestControlsByType_EXPRTypes transcribes RFC 0007's per-type control enums.
// HIDDEN is legal on every type, including LIST[LIST[INT]], which the RFC
// gives no control of its own because its identified use case (graph adjacency
// lists) is programmatic.
func TestControlsByType_EXPRTypes(t *testing.T) {
	tests := []struct {
		control ControlType
		typ     JobParamType
		want    bool
	}{
		{ControlCheckBox, JobParamTypeBool, true},
		{ControlHidden, JobParamTypeBool, true},
		{ControlLineEdit, JobParamTypeBool, false},

		{ControlLineEdit, JobParamTypeRangeExpr, true},
		{ControlHidden, JobParamTypeRangeExpr, true},
		{ControlSpinBox, JobParamTypeRangeExpr, false},

		{ControlLineEditList, JobParamTypeListString, true},
		{ControlHidden, JobParamTypeListString, true},
		{ControlSpinBoxList, JobParamTypeListString, false},
		{ControlLineEdit, JobParamTypeListString, false},

		{ControlChooseInputFileList, JobParamTypeListPath, true},
		{ControlChooseOutputFileList, JobParamTypeListPath, true},
		{ControlChooseDirectoryList, JobParamTypeListPath, true},
		{ControlChooseInputFile, JobParamTypeListPath, false},

		{ControlSpinBoxList, JobParamTypeListInt, true},
		{ControlSpinBoxList, JobParamTypeListFloat, true},
		{ControlSpinBox, JobParamTypeListInt, false},

		{ControlCheckBoxList, JobParamTypeListBool, true},
		{ControlCheckBox, JobParamTypeListBool, false},

		{ControlHidden, JobParamTypeListListInt, true},
		{ControlSpinBoxList, JobParamTypeListListInt, false},

		// The base-spec types are untouched by RFC 0007.
		{ControlLineEdit, JobParamTypeString, true},
		{ControlLineEditList, JobParamTypeString, false},
		{ControlSpinBoxList, JobParamTypeInt, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.typ)+"/"+string(tt.control), func(t *testing.T) {
			set, known := controlsByType[tt.typ]
			if !known {
				t.Fatalf("controlsByType has no entry for %s", tt.typ)
			}
			if _, ok := set[tt.control]; ok != tt.want {
				t.Errorf("control %q on %s: allowed = %v, want %v",
					tt.control, tt.typ, ok, tt.want)
			}
		})
	}
}

// TestValidateUserInterface_ListControlsEndToEnd drives the real validator
// with the exact userInterface blocks the 2.11-2.16 fixtures declare, so the
// controls are exercised through validateUserInterface rather than only as map
// membership.
//
// singleStepDelta and decimals are the two that need more than a map entry:
// both were written against the scalar SPIN_BOX, and 2.13/2.14 pair them with
// SPIN_BOX_LIST.
func TestValidateUserInterface_ListControlsEndToEnd(t *testing.T) {
	tests := []struct {
		name  string
		param string
	}{
		{"list[string] LINE_EDIT_LIST", `
- name: P
  type: LIST[STRING]
  default: ["a"]
  userInterface:
    control: LINE_EDIT_LIST
    label: Items`},
		{"list[path] CHOOSE_INPUT_FILE_LIST", `
- name: P
  type: LIST[PATH]
  default: ["/tmp/a"]
  objectType: FILE
  dataFlow: IN
  userInterface:
    control: CHOOSE_INPUT_FILE_LIST`},
		{"list[int] SPIN_BOX_LIST with singleStepDelta", `
- name: P
  type: LIST[INT]
  default: [1]
  userInterface:
    control: SPIN_BOX_LIST
    singleStepDelta: 5`},
		{"list[float] SPIN_BOX_LIST with decimals and singleStepDelta", `
- name: P
  type: LIST[FLOAT]
  default: [1.5]
  userInterface:
    control: SPIN_BOX_LIST
    decimals: 2
    singleStepDelta: 0.1`},
		{"list[bool] CHECK_BOX_LIST", `
- name: P
  type: LIST[BOOL]
  default: [true]
  userInterface:
    control: CHECK_BOX_LIST`},
		{"list[list[int]] HIDDEN", `
- name: P
  type: LIST[LIST[INT]]
  default: [[1]]
  userInterface:
    control: HIDDEN`},
		{"range_expr LINE_EDIT", `
- name: P
  type: RANGE_EXPR
  default: "1-10"
  userInterface:
    control: LINE_EDIT
    label: Frame Range`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := Parse([]byte(`
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: T
parameterDefinitions:`+tt.param+`
steps:
- name: S
  script:
    actions:
      onRun:
        command: echo
`), FormatYAML)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			for _, e := range Validate(tmpl) {
				if strings.HasPrefix(e.Pointer, "/parameterDefinitions/0") {
					t.Errorf("%s: %s", e.Pointer, e.Message)
				}
			}
		})
	}
}

// TestValidateBool_CheckBoxControlIsValid guards a cross-task hazard rather
// than a rule of its own.
//
// RFC 0007 gives BOOL the CHECK_BOX control, and separately forbids
// allowedValues on it. The BASE spec's CHECK_BOX rule (validateCheckBoxValues)
// requires EXACTLY TWO allowedValues. Today those do not collide only because
// controlsByType has no BOOL entry, so validateUserInterface returns before
// reaching the control check at all.
//
// Sub-project F1's Task 8 adds BOOL to controlsByType. The moment it does, a
// CHECK_BOX on a BOOL reaches validateCheckBoxValues and is rejected for
// having no allowedValues — which RFC 0007 says it must not have. This test
// fails at that moment, which is the point: it is the only thing standing
// between that change and a silently regressed 2.9--bool-param.yaml.
func TestValidateBool_CheckBoxControlIsValid(t *testing.T) {
	tmpl, err := Parse([]byte(`
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: T
parameterDefinitions:
- name: UseGpu
  type: BOOL
  default: true
  userInterface:
    control: CHECK_BOX
    label: Enable Feature
steps:
- name: S
  script:
    actions:
      onRun:
        command: echo
`), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, e := range Validate(tmpl) {
		if e.Pointer == "/parameterDefinitions/0/userInterface/control" {
			t.Fatalf("CHECK_BOX rejected on a BOOL parameter: %s\n"+
				"RFC 0007 gives BOOL the CHECK_BOX control AND forbids allowedValues on it, "+
				"so the base spec's two-allowedValues requirement must not apply here.",
				e.Message)
		}
	}
}
