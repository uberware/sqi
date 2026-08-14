// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// TestSchedulerConfig_CarriesTheSubmittersExprLimits pins EXPR sub-project E4d
// Task 3's server-side wiring: the scheduler's dispatch gate compares each
// worker's advertised EXPR caps against the limits a template was ACCEPTED
// under, so it must be given the SAME limits the submitter is built with.
//
// The values below are deliberately non-default and distinct: this is four
// int64 fields crossing a struct boundary, where a transposition compiles.
func TestSchedulerConfig_CarriesTheSubmittersExprLimits(t *testing.T) {
	want := openjd.ExprLimits{
		SubmissionOperations:  11_111,
		SubmissionMemoryBytes: 222_222,
		TemplatePositions:     3_333,
		TemplateRetainedBytes: 4_444_444,
	}
	cfg := DefaultConfig()
	cfg.OpenJDExprLimits = want

	if got := schedulerConfig(cfg).ExprLimits; got != want {
		t.Fatalf("scheduler.Config.ExprLimits = %+v, want the submitter's %+v -- a scheduler "+
			"gating on limits no submission used would refuse the wrong workers, or none",
			got, want)
	}
}

// TestSchedulerConfig_DefaultsAreTheOpenJDDefaults pins that a server left
// alone hands the scheduler exactly the limits internal/openjd enforces, so the
// gate is inert on a farm at defaults.
func TestSchedulerConfig_DefaultsAreTheOpenJDDefaults(t *testing.T) {
	if got := schedulerConfig(DefaultConfig()).ExprLimits; got != openjd.DefaultExprLimits() {
		t.Fatalf("scheduler.Config.ExprLimits = %+v at defaults, want %+v",
			got, openjd.DefaultExprLimits())
	}
}

// TestSchedulerConfig_KeepsTheCallersOtherSchedulerSettings pins that the
// override is narrow: everything else the operator configured must survive.
func TestSchedulerConfig_KeepsTheCallersOtherSchedulerSettings(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Scheduler.FarmID = "farm-42"
	cfg.Scheduler.AssignBatchSize = 7

	got := schedulerConfig(cfg)
	if got.FarmID != "farm-42" || got.AssignBatchSize != 7 {
		t.Fatalf("scheduler config = %+v, want the caller's FarmID and AssignBatchSize preserved", got)
	}
}
