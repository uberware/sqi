// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build conformance

package conformance_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/test/conformance"
)

// TestConformance_TaskChunkingJobOutputs scores the TASK_CHUNKING jobs/
// fixtures, which TestConformance_Templates does not read. Each one is a
// template, the job parameters it is created with, and the output its tasks
// must and must not print; a ".invalid" fixture is one whose job cannot be
// created at all.
//
// It runs each fixture through sqi's own pipeline rather than modeling it:
// openjd.Submitter creates the job (validation, parameter binding, range and
// chunk resolution, expansion into tasks), and each task's onRun action is
// resolved exactly as a worker resolves it at run time (internal/worker/
// fmtres, with the EXPR symbol table under EXPR and base-spec substitution
// otherwise). Every fixture prints raw Python string literals, so what a task
// prints is the text of its resolved script, and a listed line matches as a
// substring of it, the way the fixtures' own runner searches task output.
//
// A fixture in chunkingJobDivergences must still fail, as a baselined case
// must in baseline.go's DiffBaseline: one that starts passing is a stale
// entry.
func TestConformance_TaskChunkingJobOutputs(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(SuiteRoot, "TASK_CHUNKING", "jobs", "*.test.yaml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// The glob also matches AppleDouble sidecars ("._x.test.yaml"), which the
	// OS writes beside every file on an exFAT or network-share checkout.
	paths = slices.DeleteFunc(paths, func(p string) bool {
		return !conformance.IsFixtureFile(filepath.Base(p))
	})
	if len(paths) == 0 {
		t.Fatal("no TASK_CHUNKING jobs fixtures found; is the submodule checked out?")
	}
	for _, path := range paths {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			fx := loadChunkingJob(t, path)
			failures := scoreChunkingJob(t, fx, strings.HasSuffix(name, ".invalid.test.yaml"))
			why, baselined := chunkingJobDivergences[name]
			switch {
			case baselined && len(failures) == 0:
				t.Errorf("baselined as divergent (%s) but now passes; remove it from chunkingJobDivergences", why)
			case !baselined && len(failures) > 0:
				t.Error(strings.Join(failures, "; "))
			}
		})
	}
	for name := range chunkingJobDivergences {
		if !slices.ContainsFunc(paths, func(p string) bool { return filepath.Base(p) == name }) {
			t.Errorf("chunkingJobDivergences names %s, which is not a fixture", name)
		}
	}
}

// chunkingJobDivergences are the fixtures sqi is known to fail, each with its
// reason.
var chunkingJobDivergences = map[string]string{
	"noncontiguous-format-canonical.test.yaml":               argStringLineFeed,
	"noncontiguous-format-canonical-interior-pair.test.yaml": argStringLineFeed,
}

// argStringLineFeed is the reason two fixtures fail: each is a template
// WITHOUT the EXPR extension whose onRun argument is a YAML "|" block, which
// ends in a line feed. Template Schemas §5.2 allows any character in an
// <ArgString> except the Cc category, which holds U+000A, and it is the EXPR
// extension that relaxes that (RFC 0005, "string types must be relaxed to
// allow line feed"), so sqi rejects the template as written. The fixtures'
// subject — the canonical form of a NONCONTIGUOUS chunk — is covered by their
// EXPR twin, noncontiguous-format-canonical-interior-pair-expr.test.yaml, and
// by internal/openjd's TestExpand_ChunkNoncontiguousIsCompact.
const argStringLineFeed = "a multi-line <ArgString> in a template without EXPR, which §5.2 forbids"

// chunkingJob is one jobs/*.test.yaml fixture.
type chunkingJob struct {
	Template   yaml.Node      `yaml:"template"`
	Parameters map[string]any `yaml:"parameters"`
	Expected   struct {
		Output    []string `yaml:"output"`
		Forbidden []string `yaml:"forbidden"`
	} `yaml:"expected"`
}

func loadChunkingJob(t *testing.T, path string) chunkingJob {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fx chunkingJob
	if err := yaml.Unmarshal(data, &fx); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return fx
}

// scoreChunkingJob creates the fixture's job and returns one message per way
// its tasks' output misses the fixture's expectation.
func scoreChunkingJob(t *testing.T, fx chunkingJob, invalid bool) []string {
	t.Helper()
	lines, err := chunkingJobOutput(t, fx)
	if invalid {
		if err == nil {
			return []string{fmt.Sprintf("the job was created, but the fixture is .invalid; tasks printed %q", lines)}
		}
		return nil
	}
	if err != nil {
		return []string{err.Error()}
	}
	if len(fx.Expected.Output) == 0 {
		return []string{"fixture expects no output; nothing would be checked"}
	}
	var failures []string
	for _, want := range fx.Expected.Output {
		if !containsLine(lines, want) {
			failures = append(failures, fmt.Sprintf("no task printed %q", want))
		}
	}
	for _, bad := range fx.Expected.Forbidden {
		if containsLine(lines, bad) {
			failures = append(failures, fmt.Sprintf("a task printed forbidden %q", bad))
		}
	}
	if len(failures) > 0 {
		failures = append(failures, fmt.Sprintf("tasks printed %q", lines))
	}
	return failures
}

// chunkingJobOutput creates the fixture's job and returns, for each task, the
// text its resolved onRun action prints.
func chunkingJobOutput(t *testing.T, fx chunkingJob) ([]string, error) {
	t.Helper()
	raw, err := yaml.Marshal(&fx.Template)
	if err != nil {
		t.Fatalf("re-serialize template: %v", err)
	}
	st := fake.New()
	farm, err := st.CreateFarm(t.Context(), store.Farm{ID: "farm-1", Name: "farm"})
	if err != nil {
		t.Fatal(err)
	}
	queue, err := st.CreateQueue(t.Context(), store.Queue{ID: "queue-1", FarmID: farm.ID, Name: "queue"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := openjd.NewSubmitter(st).Submit(t.Context(), string(raw), store.TemplateFormatYAML, openjd.SubmitOptions{
		FarmID: farm.ID, QueueID: queue.ID, Parameters: submittedParameters(t, fx.Parameters),
	})
	if err != nil {
		return nil, fmt.Errorf("create job: %w", err)
	}

	tmpl, err := openjd.Parse(raw, openjd.FormatYAML)
	if err != nil {
		t.Fatalf("openjd.Parse: %v", err)
	}
	if len(tmpl.Steps) != 1 || tmpl.Steps[0].Script == nil {
		t.Fatal("fixture template must have exactly one step with an onRun action")
	}
	step := tmpl.Steps[0]
	onRun := protocol.Action{Command: step.Script.Actions.OnRun.Command, Args: step.Script.Actions.OnRun.Args}
	exprEnabled := slices.Contains(tmpl.Extensions, "EXPR")

	lines := make([]string, 0, len(res.Tasks))
	for _, task := range res.Tasks {
		action, err := resolveTaskAction(tmpl, step, onRun, res.BoundParameters, task.Parameters, exprEnabled)
		if err != nil {
			return lines, fmt.Errorf("task %s: resolve onRun: %w", task.Name, err)
		}
		lines = append(lines, strings.Join(action.Args, " "))
	}
	return lines, nil
}

// submittedParameters writes the fixture's job parameter values the way a
// client submits them: a scalar as its text, a list as JSON.
func submittedParameters(t *testing.T, in map[string]any) map[string]string {
	t.Helper()
	out := make(map[string]string, len(in))
	for name, v := range in {
		switch v := v.(type) {
		case string:
			out[name] = v
		case []any:
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("encode parameter %s: %v", name, err)
			}
			out[name] = string(b)
		default:
			out[name] = fmt.Sprint(v)
		}
	}
	return out
}

// resolveTaskAction resolves one task's onRun action the way the worker does.
// The assignment carries the parameter values and their declared types, which
// is what the EXPR symbol table is built from.
func resolveTaskAction(
	tmpl *openjd.JobTemplate, step openjd.StepTemplate, onRun protocol.Action,
	jobParams, taskParams map[string]string, exprEnabled bool,
) (*protocol.Action, error) {
	const workDir = "/session"
	if !exprEnabled {
		return fmtres.ResolveAction(&onRun, fmtres.TaskScope(jobParams, taskParams, workDir, "", false))
	}
	msg := protocol.AssignMsg{
		Parameters:        taskParams,
		ParameterTypes:    map[string]string{},
		JobParameters:     jobParams,
		JobParameterTypes: map[string]string{},
		JobName:           tmpl.Name,
		StepName:          step.Name,
	}
	if step.ParameterSpace != nil {
		for _, tp := range step.ParameterSpace.TaskParameterDefinitions {
			msg.ParameterTypes[tp.Name] = string(tp.Type)
		}
	}
	for _, p := range tmpl.ParameterDefinitions {
		msg.JobParameterTypes[p.Name] = string(p.Type)
	}
	syms, err := fmtres.TaskSymbols(&msg, workDir, "", false, nil)
	if err != nil {
		return nil, err
	}
	return fmtres.ResolveActionExpr(&onRun, syms, nil, nil)
}

func containsLine(lines []string, s string) bool {
	for _, l := range lines {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}
