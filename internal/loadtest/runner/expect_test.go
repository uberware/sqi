// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/loadtest/chaos"
	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/target"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

// stuckFaults is a fault target that cannot stop for the store checks.
type stuckFaults struct{ target.Faults }

func (stuckFaults) StopForChecks(context.Context) error { return errors.New("worker 0 would not stop") }

// TestExpectations_UnmeasuredReworkFailsTheBound: when the store checks cannot
// run, the rework bound must fail and the record must say why.
func TestExpectations_UnmeasuredReworkFailsTheBound(t *testing.T) {
	bound := 0
	r := &run{
		logger:   testLogger(t),
		faults:   stuckFaults{},
		complete: true,
		sc:       &scenario.Scenario{Expect: scenario.Expect{MaxReworkAttempts: &bound}},
		tgt:      &plainTarget{},
		invariants: []verify.Result{
			{Name: verify.NameTerminal, OK: true},
		},
	}
	r.storeChecks(t.Context())
	r.applyExpectations()
	var names []string
	for _, inv := range r.invariants {
		names = append(names, inv.Name)
		if inv.Name == "rework" && (inv.OK || !slices.Equal(inv.Evidence, []string{"rework could not be measured"})) {
			t.Errorf("rework = %+v, want failed, could not be measured", inv)
		}
	}
	if !slices.Contains(names, "rework") {
		t.Fatalf("results %v have no rework", names)
	}
	if !slices.ContainsFunc(r.unavailable, func(u string) bool {
		return strings.HasPrefix(u, "rework: ") && strings.Contains(u, "worker 0 would not stop")
	}) {
		t.Errorf("unavailable = %q, want a rework entry saying why", r.unavailable)
	}
	if got := names[len(names)-1]; got != "rework" {
		t.Errorf("last result %q, want rework after the store checks %v", got, names)
	}
}

func TestExpectations_NotAppliedToAnIncompleteRun(t *testing.T) {
	bound := 0
	r := &run{faults: stuckFaults{}, sc: &scenario.Scenario{Expect: scenario.Expect{
		MaxReworkAttempts: &bound,
		KnownViolations:   []scenario.KnownViolation{{Check: "caps", Kind: scenario.KnownMust, Reason: "x", Tracker: "H"}},
	}}}
	r.applyExpectations()
	if len(r.invariants) != 0 {
		t.Errorf("invariants = %+v, want none for a run that did not check them", r.invariants)
	}
}

// TestExpectations_UnreadableMarkersLeaveReworkUnmeasured: when the real
// tasks' markers cannot be read their executions are not counted, so a rework
// within the bound would be an undercount that passes. It must fail instead,
// marked unmeasured so no known violation excuses it.
func TestExpectations_UnreadableMarkersLeaveReworkUnmeasured(t *testing.T) {
	bound := 5
	r := &run{
		logger:   testLogger(t),
		faults:   stuckFaults{},
		complete: true,
		sc: &scenario.Scenario{Expect: scenario.Expect{
			MaxReworkAttempts: &bound,
			KnownViolations: []scenario.KnownViolation{
				{Check: scenario.ReworkCheck, Kind: scenario.KnownMay, Reason: "x", Tracker: "H", MaxAttempts: new(60)},
			},
		}},
		tgt: &plainTarget{},
	}
	r.recordRework(chaos.StoreReport{Rework: &report.Rework{}}, errors.New("read the real tasks' markers: boom"))
	r.applyExpectations()
	var got *verify.Result
	for i := range r.invariants {
		if r.invariants[i].Name == scenario.ReworkCheck {
			got = &r.invariants[i]
		}
	}
	if got == nil {
		t.Fatalf("no rework result in %+v", r.invariants)
	}
	if got.OK || !got.Unmeasured || got.Known != "" {
		t.Errorf("rework = %+v, want failed, unmeasured and not known", *got)
	}
	if !slices.ContainsFunc(r.unavailable, func(u string) bool { return strings.Contains(u, "boom") }) {
		t.Errorf("unavailable = %q, want the marker error", r.unavailable)
	}
}
