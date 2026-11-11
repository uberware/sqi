// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

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
