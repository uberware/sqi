// SPDX-License-Identifier: AGPL-3.0-or-later

package driver

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/openjd"
)

var noopCmd = scenario.Command{Path: "/bin/true", Args: []string{"-x"}}

func buildAndValidate(t *testing.T, jc scenario.JobClass) (*openjd.JobTemplate, []byte) {
	t.Helper()
	b, err := BuildTemplate(jc, "load-job", noopCmd)
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	tmpl, err := openjd.Parse(b, openjd.FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, b)
	}
	errs, err := openjd.ValidateWithBudget(tmpl, openjd.ValidateOptions{
		EnforceLimits: true,
		ExprLimits:    openjd.DefaultExprLimits(),
	})
	if err != nil {
		t.Fatalf("ValidateWithBudget: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("template does not validate: %v\n%s", errs, b)
	}
	return tmpl, b
}

func TestBuildTemplate_ValidatesLikeTheServer(t *testing.T) {
	tests := []struct {
		name string
		jc   scenario.JobClass
	}{
		{"one step one task", scenario.JobClass{Steps: 1, TasksPerStep: 1}},
		{"three steps of 100", scenario.JobClass{Steps: 3, TasksPerStep: 100}},
		{"chunk 10", scenario.JobClass{Steps: 2, TasksPerStep: 50, Chunk: 10}},
		{"cores 8", scenario.JobClass{Steps: 1, TasksPerStep: 10, Cores: 8}},
		{"usage pool", scenario.JobClass{Steps: 1, TasksPerStep: 10, UsagePool: "gpu"}},
		{"expr", scenario.JobClass{Steps: 2, TasksPerStep: 20, EXPR: true}},
		{"expr chunked", scenario.JobClass{Steps: 1, TasksPerStep: 20, Chunk: 5, EXPR: true}},
		{"250000 tasks in one step", scenario.JobClass{Steps: 1, TasksPerStep: 250000}},
		{"split with chunk", scenario.JobClass{Steps: 1, TasksPerStep: 4000, Chunk: 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, _ := buildAndValidate(t, tc.jc)
			if len(tmpl.Steps) != tc.jc.Steps {
				t.Fatalf("steps = %d, want %d", len(tmpl.Steps), tc.jc.Steps)
			}
			for i, st := range tmpl.Steps {
				tasks, err := openjd.ExpandParameterSpace(st.ParameterSpace)
				if err != nil {
					t.Fatalf("step %d expand: %v", i+1, err)
				}
				if len(tasks) != tc.jc.TasksPerStep {
					t.Fatalf("step %d expands to %d tasks, want %d", i+1, len(tasks), tc.jc.TasksPerStep)
				}
			}
		})
	}
}

func TestBuildTemplate_StepChain(t *testing.T) {
	_, b := buildAndValidate(t, scenario.JobClass{Steps: 3, TasksPerStep: 2})
	tmpl, err := openjd.Parse(b, openjd.FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	for i, st := range tmpl.Steps {
		if want := "step-" + string(rune('1'+i)); st.Name != want {
			t.Fatalf("step %d name = %q, want %q", i, st.Name, want)
		}
	}
	if len(tmpl.Steps[0].Dependencies) != 0 {
		t.Fatalf("step-1 has dependencies: %v", tmpl.Steps[0].Dependencies)
	}
	deps := tmpl.Steps[2].Dependencies
	if len(deps) != 1 || deps[0].DependsOn != "step-2" {
		t.Fatalf("step-3 dependencies = %v, want step-2", deps)
	}
}

func TestBuildTemplate_HostRequirements(t *testing.T) {
	_, b := buildAndValidate(t, scenario.JobClass{Steps: 1, TasksPerStep: 1, Cores: 8, UsagePool: "gpu"})
	s := string(b)
	for _, want := range []string{"amount.worker.vcpu", "min: 8", "amount.worker.usagepool.gpu", "min: 1"} {
		if !strings.Contains(s, want) {
			t.Errorf("template lacks %q:\n%s", want, s)
		}
	}
	_, plain := buildAndValidate(t, scenario.JobClass{Steps: 1, TasksPerStep: 1})
	if strings.Contains(string(plain), "hostRequirements") {
		t.Errorf("plain template carries hostRequirements:\n%s", plain)
	}
}

func TestBuildTemplate_CommandAndName(t *testing.T) {
	_, b := buildAndValidate(t, scenario.JobClass{Steps: 1, TasksPerStep: 1})
	s := string(b)
	for _, want := range []string{"name: load-job", "command: /bin/true", "-x"} {
		if !strings.Contains(s, want) {
			t.Errorf("template lacks %q:\n%s", want, s)
		}
	}
	if _, err := BuildTemplate(scenario.JobClass{Steps: 1, TasksPerStep: 1}, "j", scenario.Command{}); err == nil {
		t.Error("empty command path accepted")
	}
	if _, err := BuildTemplate(scenario.JobClass{Steps: 0, TasksPerStep: 1}, "j", noopCmd); err == nil {
		t.Error("zero steps accepted")
	}
	if _, err := BuildTemplate(scenario.JobClass{Steps: 1, TasksPerStep: 1031}, "j", noopCmd); err == nil {
		t.Error("unsplittable task count accepted")
	}
}

func TestBuildTemplate_EXPRDeclaresExtension(t *testing.T) {
	_, b := buildAndValidate(t, scenario.JobClass{Steps: 1, TasksPerStep: 4, Chunk: 2, EXPR: true})
	s := string(b)
	for _, want := range []string{"EXPR", "TASK_CHUNKING", "let:"} {
		if !strings.Contains(s, want) {
			t.Errorf("template lacks %q:\n%s", want, s)
		}
	}
}

// The EXPR variant must stay far below the default per-evaluation operation
// budget, so a tight 2,000-operation limit still accepts it.
func TestBuildTemplate_EXPRCostBound(t *testing.T) {
	b, err := BuildTemplate(scenario.JobClass{Steps: 3, TasksPerStep: 20, Chunk: 5, EXPR: true}, "j", noopCmd)
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := openjd.Parse(b, openjd.FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	limits := openjd.DefaultExprLimits()
	limits.SubmissionOperations = 2000
	errs, err := openjd.ValidateWithBudget(tmpl, openjd.ValidateOptions{EnforceLimits: true, ExprLimits: limits})
	if err != nil || len(errs) != 0 {
		t.Fatalf("EXPR template exceeds a 2000-operation budget: err=%v errs=%v", err, errs)
	}
}
