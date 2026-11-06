// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker

import (
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// Task plan outcomes.
const (
	// OutcomeSucceeded ends the task with a succeeded status and exit code 0.
	OutcomeSucceeded = "succeeded"
	// OutcomeFailed ends the task with a failed status and exit code 1.
	OutcomeFailed = "failed"
	// OutcomeHang emits the plan's lines over its duration, then runs until
	// the task is canceled or the worker stops.
	OutcomeHang = "hang"
)

// fallbackDuration is how long a task runs when it names no known class.
const fallbackDuration = time.Second

// TaskPlan is what a simulated worker does with one assignment: run for
// Duration, emit LogLines lines of LineBytes bytes evenly over it, then end
// with Outcome.
type TaskPlan struct {
	Duration  time.Duration
	Outcome   string
	LogLines  int
	LineBytes int
	// ProgressEvery, when positive, makes the task write an openjd_progress
	// line this often, carrying elapsed/Duration as a percentage.
	ProgressEvery time.Duration
}

// Behavior decides the plan for each assignment. The dispatcher calls Plan
// with its own seeded generator under a lock, so Plan may use r freely but
// must not keep it.
type Behavior interface {
	Plan(m *protocol.AssignMsg, r *rand.Rand) TaskPlan
}

// BehaviorFunc adapts a function to [Behavior].
type BehaviorFunc func(m *protocol.AssignMsg, r *rand.Rand) TaskPlan

// Plan calls f.
func (f BehaviorFunc) Plan(m *protocol.AssignMsg, r *rand.Rand) TaskPlan { return f(m, r) }

type scenarioBehavior struct {
	classes map[string]scenario.TaskSpec
}

// ScenarioBehavior plans each task from the TaskSpec of its job's class.
//
// The class is the one named by the assignment's last onRun argument starting
// with [scenario.ClassArgPrefix], which the driver adds to every template. A
// task with no such argument, or one naming an unknown class, runs for one
// second, succeeds and writes no lines.
//
// Sampling, in a fixed number of draws per plan: Duration is the class's
// duration sample in seconds and LogLines its line-count sample rounded down;
// then one draw decides hang (probability hang_rate) and a second decides
// failed (fail_rate) when it is not a hang. The assignment carries no attempt
// number, so fail_rate applies to every attempt, not only the first.
func ScenarioBehavior(classes map[string]scenario.TaskSpec) Behavior {
	return &scenarioBehavior{classes: classes}
}

func (b *scenarioBehavior) Plan(m *protocol.AssignMsg, r *rand.Rand) TaskPlan {
	spec, ok := b.specFor(m)
	if !ok {
		return TaskPlan{Duration: fallbackDuration, Outcome: OutcomeSucceeded}
	}
	p := TaskPlan{
		Duration:  time.Duration(spec.Duration.Sample(r) * float64(time.Second)),
		LogLines:  int(math.Floor(spec.LogLines.Sample(r))),
		LineBytes: spec.LogLineBytes,
		Outcome:   OutcomeSucceeded,

		ProgressEvery: spec.ProgressEvery.Duration,
	}
	hang, fail := r.Float64(), r.Float64()
	switch {
	case hang < spec.HangRate:
		p.Outcome = OutcomeHang
	case fail < spec.FailRate:
		p.Outcome = OutcomeFailed
	}
	return p
}

// specFor finds m's class from the class argument the driver puts last in
// every onRun.
func (b *scenarioBehavior) specFor(m *protocol.AssignMsg) (scenario.TaskSpec, bool) {
	if m.OnRun == nil {
		return scenario.TaskSpec{}, false
	}
	class, ok := scenario.ClassFromArgs(m.OnRun.Args)
	if !ok {
		return scenario.TaskSpec{}, false
	}
	spec, known := b.classes[class]
	return spec, known
}

// LogLine is line n (from 1) of a simulated task's output: "sim <taskID> line
// <n> " padded with 'x' to lineBytes. A lineBytes shorter than the prefix
// leaves the prefix whole, so every line stays identifiable.
func LogLine(taskID string, n, lineBytes int) string {
	var num [20]byte
	digits := strconv.AppendInt(num[:0], int64(n), 10)
	prefixLen := len("sim ") + len(taskID) + len(" line ") + len(digits) + 1
	var b strings.Builder
	b.Grow(max(lineBytes, prefixLen))
	b.WriteString("sim ")
	b.WriteString(taskID)
	b.WriteString(" line ")
	b.Write(digits)
	b.WriteByte(' ')
	for pad := lineBytes - prefixLen; pad > 0; pad -= min(pad, len(logPad)) {
		b.WriteString(logPad[:min(pad, len(logPad))])
	}
	return b.String()
}

// logPad is the padding LogLine copies from, so a line costs one allocation.
var logPad = strings.Repeat("x", 4096)
