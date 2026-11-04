// SPDX-License-Identifier: AGPL-3.0-or-later

package driver

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
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

// amountMins returns the parsed host requirement amounts as name -> min.
func amountMins(st openjd.StepTemplate) map[string]string {
	got := map[string]string{}
	if st.HostRequirements == nil {
		return got
	}
	for _, a := range st.HostRequirements.Amounts {
		if a.Min != nil {
			got[a.Name] = *a.Min
		} else {
			got[a.Name] = ""
		}
	}
	return got
}

func TestBuildTemplate_Structure(t *testing.T) {
	tests := []struct {
		name  string
		jc    scenario.JobClass
		check func(t *testing.T, tmpl *openjd.JobTemplate)
	}{
		{
			name: "step chain",
			jc:   scenario.JobClass{Steps: 3, TasksPerStep: 2},
			check: func(t *testing.T, tmpl *openjd.JobTemplate) {
				for i, st := range tmpl.Steps {
					if want := fmt.Sprintf("step-%d", i+1); st.Name != want {
						t.Errorf("step %d name = %q, want %q", i, st.Name, want)
					}
				}
				if len(tmpl.Steps[0].Dependencies) != 0 {
					t.Errorf("step-1 has dependencies: %v", tmpl.Steps[0].Dependencies)
				}
				for i := 1; i < 3; i++ {
					deps := tmpl.Steps[i].Dependencies
					if want := fmt.Sprintf("step-%d", i); len(deps) != 1 || deps[0].DependsOn != want {
						t.Errorf("step-%d dependencies = %v, want %s", i+1, deps, want)
					}
				}
			},
		},
		{
			name: "cores and usage pool",
			jc:   scenario.JobClass{Steps: 1, TasksPerStep: 1, Cores: 8, UsagePool: "gpu"},
			check: func(t *testing.T, tmpl *openjd.JobTemplate) {
				want := map[string]string{"amount.worker.vcpu": "8", "amount.worker.usagepool.gpu": "1"}
				if got := amountMins(tmpl.Steps[0]); !reflect.DeepEqual(got, want) {
					t.Errorf("amounts = %v, want %v", got, want)
				}
			},
		},
		{
			name: "no host requirements",
			jc:   scenario.JobClass{Steps: 1, TasksPerStep: 1},
			check: func(t *testing.T, tmpl *openjd.JobTemplate) {
				if hr := tmpl.Steps[0].HostRequirements; hr != nil {
					t.Errorf("host requirements = %+v, want none", hr)
				}
			},
		},
		{
			name: "command, name and class arg",
			jc:   scenario.JobClass{Name: "shot-render", Steps: 2, TasksPerStep: 1},
			check: func(t *testing.T, tmpl *openjd.JobTemplate) {
				if tmpl.Name != "load-job" {
					t.Errorf("name = %q", tmpl.Name)
				}
				wantArgs := append(slices.Clone(noopCmd.Args), "--sqi-load-class=shot-render")
				for _, st := range tmpl.Steps {
					run := st.Script.Actions.OnRun
					if run.Command != noopCmd.Path || !reflect.DeepEqual(run.Args, wantArgs) {
						t.Errorf("%s onRun = %q %q, want %q %q", st.Name, run.Command, run.Args, noopCmd.Path, wantArgs)
					}
				}
				if len(tmpl.Extensions) != 0 {
					t.Errorf("extensions = %v, want none", tmpl.Extensions)
				}
			},
		},
		{
			name: "EXPR chunked",
			jc:   scenario.JobClass{Name: "comp", Steps: 1, TasksPerStep: 4, Chunk: 2, EXPR: true},
			check: func(t *testing.T, tmpl *openjd.JobTemplate) {
				if got, want := tmpl.Extensions, []string{"TASK_CHUNKING", "EXPR"}; !reflect.DeepEqual(got, want) {
					t.Errorf("extensions = %v, want %v", got, want)
				}
				st := tmpl.Steps[0]
				if len(st.Script.Let) != 1 {
					t.Errorf("let = %v, want one binding", st.Script.Let)
				}
				args := st.Script.Actions.OnRun.Args
				if len(args) != len(noopCmd.Args)+2 || args[len(args)-1] != "--sqi-load-class=comp" {
					t.Errorf("args = %v, want the command's, one expression, then the class arg", args)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, _ := buildAndValidate(t, tc.jc)
			tc.check(t, tmpl)
		})
	}
}

func TestBuildTemplate_Errors(t *testing.T) {
	tests := []struct {
		name string
		jc   scenario.JobClass
		cmd  scenario.Command
	}{
		{"empty command path", scenario.JobClass{Steps: 1, TasksPerStep: 1}, scenario.Command{}},
		{"zero steps", scenario.JobClass{Steps: 0, TasksPerStep: 1}, noopCmd},
		{"unsplittable task count", scenario.JobClass{Steps: 1, TasksPerStep: 1031}, noopCmd},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildTemplate(tc.jc, "j", tc.cmd); err == nil {
				t.Error("BuildTemplate succeeded, want an error")
			}
		})
	}
}

// TestBuildTemplate_SubmitsLikeTheServer runs each template through the
// submitter the server's POST /jobs path uses: phase-1 validation, the phase-2
// expression re-check with concrete parameters, parameter-space resolution and
// the single-transaction store write.
func TestBuildTemplate_SubmitsLikeTheServer(t *testing.T) {
	tests := []struct {
		name string
		jc   scenario.JobClass
		slow bool
	}{
		{"EXPR chunked", scenario.JobClass{Steps: 2, TasksPerStep: 40, Chunk: 5, EXPR: true}, false},
		{"EXPR split with chunk", scenario.JobClass{Steps: 1, TasksPerStep: 4000, Chunk: 2, EXPR: true}, false},
		{"three step chain", scenario.JobClass{Steps: 3, TasksPerStep: 100}, false},
		{"usage pool and cores", scenario.JobClass{Steps: 2, TasksPerStep: 10, Cores: 4, UsagePool: "gpu"}, false},
		{"one step of 250000", scenario.JobClass{Steps: 1, TasksPerStep: 250000}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.slow && testing.Short() {
				t.Skip("large submission skipped in -short mode")
			}
			ctx := t.Context()
			st := fake.New()
			farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "f"})
			if err != nil {
				t.Fatal(err)
			}
			queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "q"})
			if err != nil {
				t.Fatal(err)
			}
			b, err := BuildTemplate(tc.jc, "submit-job", noopCmd)
			if err != nil {
				t.Fatal(err)
			}
			sub := openjd.NewSubmitterWithOptions(st, openjd.SubmitterOptions{
				EnforceLimits: true,
				ExprLimits:    openjd.DefaultExprLimits(),
			})
			res, err := sub.Submit(ctx, string(b), store.TemplateFormatYAML, openjd.SubmitOptions{
				FarmID:   farm.ID,
				QueueID:  queue.ID,
				Deadline: time.Now().Add(5 * time.Second),
			})
			if err != nil {
				t.Fatalf("Submit: %v\n%s", err, b)
			}
			steps, err := st.ListSteps(ctx, res.Job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(steps) != tc.jc.Steps {
				t.Fatalf("stored %d steps, want %d", len(steps), tc.jc.Steps)
			}
			for i, step := range steps {
				var want []string
				if i > 0 {
					want = []string{fmt.Sprintf("step-%d", i)}
				}
				if !reflect.DeepEqual(step.DependsOn, want) && (len(step.DependsOn) != 0 || len(want) != 0) {
					t.Errorf("%s depends on %v, want %v", step.Name, step.DependsOn, want)
				}
				page, err := st.ListTasks(ctx, store.ListTasksOptions{
					JobID: res.Job.ID, StepID: step.ID, Pagination: store.Pagination{Limit: 1},
				})
				if err != nil {
					t.Fatal(err)
				}
				if page.Total != tc.jc.TasksPerStep {
					t.Errorf("%s stored %d tasks, want %d", step.Name, page.Total, tc.jc.TasksPerStep)
				}
			}
			if want := tc.jc.Steps * tc.jc.TasksPerStep; len(res.Tasks) != want {
				t.Errorf("submit result holds %d tasks, want %d", len(res.Tasks), want)
			}
		})
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
