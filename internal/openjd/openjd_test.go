// SPDX-License-Identifier: AGPL-3.0-only

package openjd_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// minimalValidYAML returns a well-formed single-step YAML template for use as
// a base in sub-tests.
func minimalValidYAML() string {
	return `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
          args: ["hello"]
`
}

// mustParse is a test helper that parses YAML and fails immediately on error.
func mustParse(t *testing.T, yaml string) *openjd.JobTemplate {
	t.Helper()
	tmpl, err := openjd.Parse([]byte(yaml), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	return tmpl
}

// ── Parse: YAML ───────────────────────────────────────────────────────────────

func TestParse_YAML_MinimalTemplate(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	if tmpl.SpecificationVersion != openjd.SpecVersion {
		t.Errorf("specificationVersion: got %q, want %q", tmpl.SpecificationVersion, openjd.SpecVersion)
	}
	if tmpl.Name != "TestJob" {
		t.Errorf("name: got %q, want %q", tmpl.Name, "TestJob")
	}
	if len(tmpl.Steps) != 1 {
		t.Fatalf("steps: got %d, want 1", len(tmpl.Steps))
	}
	if tmpl.Steps[0].Name != "Step1" {
		t.Errorf("steps[0].name: got %q", tmpl.Steps[0].Name)
	}
}

func TestParse_YAML_FullTemplate(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: RenderJob
description: A full render job template.
parameterDefinitions:
  - name: SceneName
    type: STRING
    default: "main"
  - name: StartFrame
    type: INT
    minValue: "1"
    maxValue: "9999"
steps:
  - name: Render
    script:
      actions:
        onRun:
          command: blender
          args: ["--background", "{{Param.SceneName}}"]
          timeout: 3600
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "1-10"
    hostRequirements:
      amounts:
        - name: amount.worker.vcpu
          min: "4"
      attributes:
        - name: attr.worker.os.family
          anyOf: ["linux", "windows"]
  - name: Composite
    dependencies:
      - dependsOn: Render
    script:
      actions:
        onRun:
          command: composite
`
	tmpl := mustParse(t, yaml)
	if len(tmpl.ParameterDefinitions) != 2 {
		t.Fatalf("parameterDefinitions: got %d, want 2", len(tmpl.ParameterDefinitions))
	}
	if tmpl.ParameterDefinitions[0].Name != "SceneName" {
		t.Errorf("parameterDefinitions[0].name: got %q", tmpl.ParameterDefinitions[0].Name)
	}
	if len(tmpl.Steps) != 2 {
		t.Fatalf("steps: got %d, want 2", len(tmpl.Steps))
	}
	step := tmpl.Steps[0]
	if step.ParameterSpace == nil {
		t.Fatal("steps[0].parameterSpace: nil")
	}
	if step.HostRequirements == nil {
		t.Fatal("steps[0].hostRequirements: nil")
	}
	if len(step.HostRequirements.Amounts) != 1 {
		t.Errorf("amounts: got %d, want 1", len(step.HostRequirements.Amounts))
	}
	dep := tmpl.Steps[1]
	if len(dep.Dependencies) != 1 || dep.Dependencies[0].DependsOn != "Render" {
		t.Errorf("steps[1].dependencies: got %v", dep.Dependencies)
	}
}

func TestParse_YAML_Malformed(t *testing.T) {
	_, err := openjd.Parse([]byte(":\tnot: valid: yaml:::"), openjd.FormatYAML)
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
	if !strings.Contains(err.Error(), "parse YAML") {
		t.Errorf("error should mention 'parse YAML': %v", err)
	}
}

// ── Parse: JSON ───────────────────────────────────────────────────────────────

func TestParse_JSON_MinimalTemplate(t *testing.T) {
	j := `{
	"specificationVersion": "jobtemplate-2023-09",
	"name": "JSONJob",
	"steps": [{"name": "S1", "script": {"actions": {"onRun": {"command": "echo"}}}}]
}`
	tmpl, err := openjd.Parse([]byte(j), openjd.FormatJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tmpl.Name != "JSONJob" {
		t.Errorf("name: got %q", tmpl.Name)
	}
}

func TestParse_JSON_Malformed(t *testing.T) {
	_, err := openjd.Parse([]byte("{bad json"), openjd.FormatJSON)
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), "parse JSON") {
		t.Errorf("error should mention 'parse JSON': %v", err)
	}
}

// ── Parse: parameter definitions ─────────────────────────────────────────────

func TestParse_YAML_AllowedValues(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: Job
parameterDefinitions:
  - name: Mode
    type: STRING
    allowedValues: ["fast", "slow", "auto"]
steps:
  - name: S
    script:
      actions:
        onRun:
          command: run
`
	tmpl := mustParse(t, yaml)
	p := tmpl.ParameterDefinitions[0]
	if len(p.AllowedValues) != 3 {
		t.Fatalf("allowedValues: got %d", len(p.AllowedValues))
	}
	if p.AllowedValues[1] != "slow" {
		t.Errorf("allowedValues[1]: got %q", p.AllowedValues[1])
	}
}

func TestParse_YAML_IntParamBounds(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: Job
parameterDefinitions:
  - name: Count
    type: INT
    minValue: "1"
    maxValue: "100"
    default: "10"
steps:
  - name: S
    script:
      actions:
        onRun:
          command: run
`
	tmpl := mustParse(t, yaml)
	p := tmpl.ParameterDefinitions[0]
	if p.MinValue == nil || *p.MinValue != "1" {
		t.Errorf("minValue: got %v", p.MinValue)
	}
	if p.MaxValue == nil || *p.MaxValue != "100" {
		t.Errorf("maxValue: got %v", p.MaxValue)
	}
	if p.Default == nil || *p.Default != "10" {
		t.Errorf("default: got %v", p.Default)
	}
}

// ── Validate ──────────────────────────────────────────────────────────────────

func TestValidate_ValidTemplate(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	errs := openjd.Validate(tmpl)
	if len(errs) != 0 {
		t.Errorf("expected no errors; got: %v", errs)
	}
}

func TestValidate_MissingSpecVersion(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.SpecificationVersion = ""
	errs := openjd.Validate(tmpl)
	if !containsPointer(errs, "/specificationVersion") {
		t.Errorf("expected /specificationVersion error; got %v", errs)
	}
}

func TestValidate_WrongSpecVersion(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.SpecificationVersion = "jobtemplate-2099-01"
	errs := openjd.Validate(tmpl)
	if !containsPointer(errs, "/specificationVersion") {
		t.Errorf("expected /specificationVersion error; got %v", errs)
	}
}

func TestValidate_EmptyName(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Name = "   "
	errs := openjd.Validate(tmpl)
	if !containsPointer(errs, "/name") {
		t.Errorf("expected /name error; got %v", errs)
	}
}

func TestValidate_NoSteps(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Steps = nil
	errs := openjd.Validate(tmpl)
	if !containsPointer(errs, "/steps") {
		t.Errorf("expected /steps error; got %v", errs)
	}
}

func TestValidate_DuplicateParameterName(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.ParameterDefinitions = []openjd.JobParameter{
		{Name: "Foo", Type: openjd.JobParamTypeString},
		{Name: "Foo", Type: openjd.JobParamTypeInt},
	}
	errs := openjd.Validate(tmpl)
	if !containsMessage(errs, "duplicate") {
		t.Errorf("expected duplicate-name error; got %v", errs)
	}
}

func TestValidate_InvalidParameterType(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.ParameterDefinitions = []openjd.JobParameter{
		{Name: "X", Type: "BOGUS"},
	}
	errs := openjd.Validate(tmpl)
	if !containsPointer(errs, "/parameterDefinitions/0/type") {
		t.Errorf("expected type error; got %v", errs)
	}
}

func TestValidate_InvalidParameterIdentifier(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.ParameterDefinitions = []openjd.JobParameter{
		{Name: "123bad", Type: openjd.JobParamTypeString},
	}
	errs := openjd.Validate(tmpl)
	if !containsPointer(errs, "/parameterDefinitions/0/name") {
		t.Errorf("expected name error; got %v", errs)
	}
}

func TestValidate_DuplicateStepName(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Steps = append(tmpl.Steps, openjd.StepTemplate{Name: "Step1"})
	errs := openjd.Validate(tmpl)
	if !containsMessage(errs, "duplicate step name") {
		t.Errorf("expected duplicate step name error; got %v", errs)
	}
}

func TestValidate_UnknownStepDependency(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Steps[0].Dependencies = []openjd.StepDependency{
		{DependsOn: "NonExistent"},
	}
	errs := openjd.Validate(tmpl)
	if !containsMessage(errs, "unknown step") {
		t.Errorf("expected unknown step error; got %v", errs)
	}
}

func TestValidate_SelfReferencingStep(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Steps[0].Dependencies = []openjd.StepDependency{
		{DependsOn: "Step1"},
	}
	errs := openjd.Validate(tmpl)
	if !containsMessage(errs, "cannot depend on itself") {
		t.Errorf("expected self-dependency error; got %v", errs)
	}
}

func TestValidate_CyclicDependency(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Steps = []openjd.StepTemplate{
		{Name: "A", Dependencies: []openjd.StepDependency{{DependsOn: "B"}}},
		{Name: "B", Dependencies: []openjd.StepDependency{{DependsOn: "A"}}},
	}
	errs := openjd.Validate(tmpl)
	if !containsMessage(errs, "cycle") {
		t.Errorf("expected cycle error; got %v", errs)
	}
}

func TestValidate_TaskParamDuplicateName(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "F", Type: openjd.TaskParamTypeInt, RangeList: []string{"1", "2"}},
			{Name: "F", Type: openjd.TaskParamTypeInt, RangeList: []string{"3", "4"}},
		},
	}
	errs := openjd.Validate(tmpl)
	if !containsMessage(errs, "duplicate task parameter") {
		t.Errorf("expected duplicate task param error; got %v", errs)
	}
}

func TestValidate_InvalidTaskParamType(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "F", Type: "UNKNOWN", RangeList: []string{"1"}},
		},
	}
	errs := openjd.Validate(tmpl)
	if !containsMessage(errs, "unknown type") {
		t.Errorf("expected unknown type error; got %v", errs)
	}
}

func TestValidate_CombinationReferencesUndeclaredParam(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "F", Type: openjd.TaskParamTypeInt, RangeList: []string{"1"}},
		},
		Combination: new("F*Ghost"),
	}
	errs := openjd.Validate(tmpl)
	if !containsMessage(errs, "undeclared parameter") {
		t.Errorf("expected undeclared parameter error; got %v", errs)
	}
}

func TestValidate_MultipleErrors(t *testing.T) {
	// Zero-value template should produce multiple errors at once.
	tmpl := &openjd.JobTemplate{}
	errs := openjd.Validate(tmpl)
	if len(errs) < 2 {
		t.Errorf("expected multiple errors for zero template; got %d: %v", len(errs), errs)
	}
}

// ── ExpandParameterSpace ──────────────────────────────────────────────────────

func TestExpand_NilParameterSpace(t *testing.T) {
	rows, err := openjd.ExpandParameterSpace(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if len(rows[0]) != 0 {
		t.Errorf("want empty TaskParams, got %v", rows[0])
	}
}

func TestExpand_IntRangeExpression(t *testing.T) {
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "Frame", Type: openjd.TaskParamTypeInt, RangeExpr: new("1-5")},
		},
	}
	rows, err := openjd.ExpandParameterSpace(ps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("want 5 rows, got %d", len(rows))
	}
	for i, row := range rows {
		want := []string{"1", "2", "3", "4", "5"}[i]
		if row["Frame"] != want {
			t.Errorf("row[%d][Frame]: got %q, want %q", i, row["Frame"], want)
		}
	}
}

func TestExpand_IntRangeExprWithStep(t *testing.T) {
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "F", Type: openjd.TaskParamTypeInt, RangeExpr: new("0-10:2")},
		},
	}
	rows, err := openjd.ExpandParameterSpace(ps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 0,2,4,6,8,10 → 6 values.
	if len(rows) != 6 {
		t.Fatalf("want 6 rows, got %d: %v", len(rows), rows)
	}
}

func TestExpand_IntExplicitList(t *testing.T) {
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "F", Type: openjd.TaskParamTypeInt, RangeList: []string{"10", "20", "30"}},
		},
	}
	rows, err := openjd.ExpandParameterSpace(ps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	if rows[1]["F"] != "20" {
		t.Errorf("row[1][F]: got %q", rows[1]["F"])
	}
}

func TestExpand_FloatList(t *testing.T) {
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "Q", Type: openjd.TaskParamTypeFloat, RangeList: []string{"0.5", "1.0", "2.5"}},
		},
	}
	rows, err := openjd.ExpandParameterSpace(ps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
}

func TestExpand_StringList(t *testing.T) {
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "Scene", Type: openjd.TaskParamTypeString, RangeList: []string{"a", "b", "c"}},
		},
	}
	rows, err := openjd.ExpandParameterSpace(ps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	if rows[2]["Scene"] != "c" {
		t.Errorf("row[2][Scene]: got %q", rows[2]["Scene"])
	}
}

func TestExpand_CartesianProduct(t *testing.T) {
	// 3 × 2 = 6 tasks.
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "A", Type: openjd.TaskParamTypeInt, RangeExpr: new("1-3")},
			{Name: "B", Type: openjd.TaskParamTypeString, RangeList: []string{"x", "y"}},
		},
	}
	rows, err := openjd.ExpandParameterSpace(ps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("want 6 rows, got %d", len(rows))
	}
	// Every row must have both A and B.
	for i, row := range rows {
		if row["A"] == "" || row["B"] == "" {
			t.Errorf("row[%d] missing key: %v", i, row)
		}
	}
}

func TestExpand_CartesianExplicitCombination(t *testing.T) {
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "A", Type: openjd.TaskParamTypeInt, RangeList: []string{"1", "2"}},
			{Name: "B", Type: openjd.TaskParamTypeInt, RangeList: []string{"10", "20"}},
		},
		Combination: new("A*B"),
	}
	rows, err := openjd.ExpandParameterSpace(ps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("want 4 rows, got %d", len(rows))
	}
}

func TestExpand_Association(t *testing.T) {
	// (A,B) zips → 3 rows.
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "A", Type: openjd.TaskParamTypeInt, RangeList: []string{"1", "2", "3"}},
			{Name: "B", Type: openjd.TaskParamTypeString, RangeList: []string{"a", "b", "c"}},
		},
		Combination: new("(A,B)"),
	}
	rows, err := openjd.ExpandParameterSpace(ps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	if rows[1]["A"] != "2" || rows[1]["B"] != "b" {
		t.Errorf("row[1]: got A=%q B=%q, want A=2 B=b", rows[1]["A"], rows[1]["B"])
	}
}

func TestExpand_AssociationLengthMismatch_Error(t *testing.T) {
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "A", Type: openjd.TaskParamTypeInt, RangeList: []string{"1", "2"}},
			{Name: "B", Type: openjd.TaskParamTypeInt, RangeList: []string{"10"}},
		},
		Combination: new("(A,B)"),
	}
	_, err := openjd.ExpandParameterSpace(ps)
	if err == nil {
		t.Fatal("expected error for length mismatch in association group")
	}
}

func TestExpand_ChunkInt(t *testing.T) {
	// 10 frames in chunks of 3 → ceil(10/3)=4 tasks.
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{
				Name:      "Frames",
				Type:      openjd.TaskParamTypeChunkInt,
				RangeExpr: new("1-10"),
				Chunks:    &openjd.TaskChunks{DefaultTaskCount: 3},
			},
		},
	}
	rows, err := openjd.ExpandParameterSpace(ps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("want 4 rows, got %d: %v", len(rows), rows)
	}
	// First chunk: 1-3.
	if rows[0]["Frames"] != "1-3" {
		t.Errorf("rows[0][Frames]: got %q, want 1-3", rows[0]["Frames"])
	}
}

func TestExpand_InvalidRangeExpr_Error(t *testing.T) {
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "F", Type: openjd.TaskParamTypeInt, RangeExpr: new("not-a-range")},
		},
	}
	_, err := openjd.ExpandParameterSpace(ps)
	if err == nil {
		t.Fatal("expected error for invalid range expression")
	}
}

func TestExpand_EmptyStringList_Error(t *testing.T) {
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "S", Type: openjd.TaskParamTypeString, RangeList: []string{}},
		},
	}
	_, err := openjd.ExpandParameterSpace(ps)
	if err == nil {
		t.Fatal("expected error for empty range list")
	}
}

func TestExpand_CombinationUndeclaredParam_Error(t *testing.T) {
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "A", Type: openjd.TaskParamTypeInt, RangeList: []string{"1"}},
		},
		Combination: new("A*Ghost"),
	}
	_, err := openjd.ExpandParameterSpace(ps)
	if err == nil {
		t.Fatal("expected error for undeclared param in combination")
	}
}

// ── assertion helpers ─────────────────────────────────────────────────────────

func containsPointer(errs openjd.ValidationErrors, ptr string) bool {
	for _, e := range errs {
		if e.Pointer == ptr {
			return true
		}
	}
	return false
}

func containsMessage(errs openjd.ValidationErrors, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e.Message, substr) {
			return true
		}
	}
	return false
}
