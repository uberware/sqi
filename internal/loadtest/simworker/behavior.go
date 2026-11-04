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

// fallbackDuration is how long a task runs when its job matches no class.
const fallbackDuration = time.Second

// TaskPlan is what a simulated worker does with one assignment: run for
// Duration, emit LogLines lines of LineBytes bytes evenly over it, then end
// with Outcome.
type TaskPlan struct {
	Duration  time.Duration
	Outcome   string
	LogLines  int
	LineBytes int
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

// BehaviorOption configures [ScenarioBehavior].
type BehaviorOption func(*scenarioBehavior)

// WithJobNames supplies the job name for an assignment that does not carry
// one. The server sends a job's name only with EXPR assignments, so a
// base-spec job's class can be found only through a lookup like this one
// (the driver records the name of each job it submits).
func WithJobNames(lookup func(jobID string) (name string, ok bool)) BehaviorOption {
	return func(b *scenarioBehavior) { b.lookup = lookup }
}

type scenarioBehavior struct {
	classes map[string]scenario.TaskSpec
	lookup  func(string) (string, bool)
}

// ScenarioBehavior plans each task from the TaskSpec of its job's class.
//
// The class is the one named by the assignment's last onRun argument starting
// with [scenario.ClassArgPrefix], which the driver adds to every template. An
// assignment without one (or naming an unknown class) falls back to the job's
// name. Job names are "<class>-<seq>", or "<prefix>-<class>-<seq>" for a shard. The
// class is found by dropping the last hyphen-separated token (the sequence
// number) and taking the longest class name that equals what is left or ends
// it after a hyphen, so "comp-render-3" is class "comp-render" even when a
// "render" class exists too. The name is the assignment's JobName when the
// server sent one (EXPR jobs only), otherwise the [WithJobNames] lookup's. A
// task whose class is found by none of these runs for one second, succeeds
// and writes no lines.
//
// Sampling, in a fixed number of draws per plan: Duration is the class's
// duration sample in seconds and LogLines its line-count sample rounded down;
// then one draw decides hang (probability hang_rate) and a second decides
// failed (fail_rate) when it is not a hang. The assignment carries no attempt
// number, so fail_rate applies to every attempt, not only the first.
func ScenarioBehavior(classes map[string]scenario.TaskSpec, opts ...BehaviorOption) Behavior {
	b := &scenarioBehavior{classes: classes}
	for _, o := range opts {
		o(b)
	}
	return b
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

// specFor finds m's class: the class argument the driver puts last in every
// onRun, else the job's name.
func (b *scenarioBehavior) specFor(m *protocol.AssignMsg) (scenario.TaskSpec, bool) {
	if m.OnRun != nil {
		if class, ok := scenario.ClassFromArgs(m.OnRun.Args); ok {
			if spec, known := b.classes[class]; known {
				return spec, true
			}
		}
	}
	return b.classFor(b.jobName(m))
}

func (b *scenarioBehavior) jobName(m *protocol.AssignMsg) string {
	if m.JobName != "" {
		return m.JobName
	}
	if b.lookup != nil {
		if n, ok := b.lookup(m.JobID); ok {
			return n
		}
	}
	return ""
}

// classFor returns the spec of the class jobName belongs to.
func (b *scenarioBehavior) classFor(jobName string) (scenario.TaskSpec, bool) {
	if jobName == "" {
		return scenario.TaskSpec{}, false
	}
	base := jobName
	if i := strings.LastIndexByte(jobName, '-'); i >= 0 {
		base = jobName[:i]
	}
	best := ""
	for c := range b.classes {
		if len(c) > len(best) && (base == c || strings.HasSuffix(base, "-"+c)) {
			best = c
		}
	}
	if best == "" {
		return scenario.TaskSpec{}, false
	}
	return b.classes[best], true
}

// LogLine is line n (from 1) of a simulated task's output: "sim <taskID> line
// <n> " padded with 'x' to lineBytes. A lineBytes shorter than the prefix
// leaves the prefix whole, so every line stays identifiable.
func LogLine(taskID string, n, lineBytes int) string {
	prefix := "sim " + taskID + " line " + strconv.Itoa(n) + " "
	if pad := lineBytes - len(prefix); pad > 0 {
		return prefix + strings.Repeat("x", pad)
	}
	return prefix
}
