// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker_test

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/worker/protocol"
)

func constDist(v float64) scenario.Dist { return scenario.Dist{Kind: scenario.DistConstant, Value: v} }

func specFor(seconds float64) scenario.TaskSpec {
	return scenario.TaskSpec{Duration: constDist(seconds)}
}

// assignFor is an assignment whose onRun names class the way the driver's
// templates do.
func assignFor(class string) *protocol.AssignMsg {
	return &protocol.AssignMsg{OnRun: &protocol.Action{Command: "noop", Args: []string{"-x", scenario.ClassArg(class)}}}
}

func TestScenarioBehavior_Class(t *testing.T) {
	classes := map[string]scenario.TaskSpec{
		"render":      specFor(2),
		"comp-render": specFor(3),
		"comp":        specFor(4),
	}
	b := simworker.ScenarioBehavior(classes)

	tests := []struct {
		name    string
		jobName string
		args    []string
		want    time.Duration
	}{
		{"class arg", "", []string{"-x", scenario.ClassArg("comp")}, 4 * time.Second},
		{"hyphenated class", "", []string{scenario.ClassArg("comp-render")}, 3 * time.Second},
		{"class arg beats the job name", "render-1", []string{scenario.ClassArg("comp-render")}, 3 * time.Second},
		{"last class arg wins", "", []string{scenario.ClassArg("render"), scenario.ClassArg("comp")}, 4 * time.Second},
		{"unknown class arg falls back", "render-1", []string{scenario.ClassArg("nope")}, time.Second},
		{"no class arg falls back", "render-1", []string{"-x"}, time.Second},
		{"no onRun falls back", "render-1", nil, time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &protocol.AssignMsg{JobName: tt.jobName}
			if tt.args != nil {
				m.OnRun = &protocol.Action{Command: "noop", Args: tt.args}
			}
			got := b.Plan(m, rand.New(rand.NewPCG(1, 2)))
			if got.Duration != tt.want {
				t.Errorf("Duration = %v, want %v", got.Duration, tt.want)
			}
		})
	}
}

func TestScenarioBehavior_Fallback(t *testing.T) {
	b := simworker.ScenarioBehavior(nil)
	got := b.Plan(assignFor("x"), rand.New(rand.NewPCG(1, 2)))
	want := simworker.TaskPlan{Duration: time.Second, Outcome: simworker.OutcomeSucceeded}
	if got != want {
		t.Errorf("Plan = %+v, want %+v", got, want)
	}
}

func TestScenarioBehavior_Sampling(t *testing.T) {
	tests := []struct {
		name string
		spec scenario.TaskSpec
		want simworker.TaskPlan
	}{
		{
			name: "succeeded with lines rounded down",
			spec: scenario.TaskSpec{Duration: constDist(1.5), LogLines: constDist(7.9), LogLineBytes: 120},
			want: simworker.TaskPlan{Duration: 1500 * time.Millisecond, Outcome: simworker.OutcomeSucceeded, LogLines: 7, LineBytes: 120},
		},
		{
			name: "hang wins over fail",
			spec: scenario.TaskSpec{Duration: constDist(1), HangRate: 1, FailRate: 1},
			want: simworker.TaskPlan{Duration: time.Second, Outcome: simworker.OutcomeHang},
		},
		{
			name: "fail",
			spec: scenario.TaskSpec{Duration: constDist(1), FailRate: 1},
			want: simworker.TaskPlan{Duration: time.Second, Outcome: simworker.OutcomeFailed},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := simworker.ScenarioBehavior(map[string]scenario.TaskSpec{"c": tt.spec})
			got := b.Plan(assignFor("c"), rand.New(rand.NewPCG(1, 2)))
			if got != tt.want {
				t.Errorf("Plan = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestScenarioBehavior_SeededIsReproducible(t *testing.T) {
	spec := scenario.TaskSpec{
		Duration: scenario.Dist{Kind: scenario.DistLognormal, Median: 20, P95: 90},
		LogLines: scenario.Dist{Kind: scenario.DistUniform, Min: 10, Max: 2000},
		FailRate: 0.3, HangRate: 0.1,
	}
	b := simworker.ScenarioBehavior(map[string]scenario.TaskSpec{"c": spec})
	draw := func() []simworker.TaskPlan {
		r := rand.New(rand.NewPCG(42, 7))
		var out []simworker.TaskPlan
		for range 50 {
			out = append(out, b.Plan(assignFor("c"), r))
		}
		return out
	}
	a, c := draw(), draw()
	outcomes := map[string]int{}
	for i := range a {
		if a[i] != c[i] {
			t.Fatalf("plan %d differs between runs with one seed: %+v vs %+v", i, a[i], c[i])
		}
		outcomes[a[i].Outcome]++
	}
	if len(outcomes) < 2 {
		t.Errorf("50 draws at fail 0.3 / hang 0.1 gave only %v", outcomes)
	}
}

func TestLogLine(t *testing.T) {
	tests := []struct {
		name  string
		bytes int
		want  string
	}{
		{"padded", 20, "sim t1 line 3 xxxxxx"},
		{"shorter than the prefix keeps the prefix", 4, "sim t1 line 3 "},
		{"zero keeps the prefix", 0, "sim t1 line 3 "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := simworker.LogLine("t1", 3, tt.bytes); got != tt.want {
				t.Errorf("LogLine = %q, want %q", got, tt.want)
			}
		})
	}
}
