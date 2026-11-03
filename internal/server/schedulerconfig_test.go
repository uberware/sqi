// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// TestSchedulerConfig_CarriesTheSubmittersExprLimits pins the server-side
// wiring of the EXPR dispatch gate: the scheduler compares each worker's
// advertised EXPR caps against the limits a template was accepted under, so
// it must be given the same limits the submitter is built with.
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

	if got := schedulerConfig(cfg, nil).ExprLimits; got != want {
		t.Fatalf("scheduler.Config.ExprLimits = %+v, want the submitter's %+v -- a scheduler "+
			"gating on limits no submission used would refuse the wrong workers, or none",
			got, want)
	}
}

// TestSchedulerConfig_DefaultsAreTheOpenJDDefaults pins that a server left
// alone hands the scheduler exactly the limits internal/openjd enforces, so the
// gate is inert on a farm at defaults.
func TestSchedulerConfig_DefaultsAreTheOpenJDDefaults(t *testing.T) {
	if got := schedulerConfig(DefaultConfig(), nil).ExprLimits; got != openjd.DefaultExprLimits() {
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

	got := schedulerConfig(cfg, nil)
	if got.FarmID != "farm-42" || got.AssignBatchSize != 7 {
		t.Fatalf("scheduler config = %+v, want the caller's FarmID and AssignBatchSize preserved", got)
	}
}

// TestSchedulerConfig_WiresTheClientsBusUpSince pins that the
// scheduler's liveness grace is fed by the server's bus client, and that a
// caller-supplied source is not overwritten.
func TestSchedulerConfig_WiresTheClientsBusUpSince(t *testing.T) {
	client := &bus.Client{}
	got := schedulerConfig(DefaultConfig(), client)
	if got.BusUpSince == nil {
		t.Fatal("BusUpSince is nil; the scheduler would never defer its liveness sweeps")
	}
	if !got.BusUpSince().IsZero() {
		t.Error("a client that never connected must report the bus as down (zero time)")
	}

	marker := time.Unix(1, 0)
	cfg := DefaultConfig()
	cfg.Scheduler.BusUpSince = func() time.Time { return marker }
	if got := schedulerConfig(cfg, client).BusUpSince(); !got.Equal(marker) {
		t.Errorf("BusUpSince = %v, want the caller's source (%v) kept", got, marker)
	}
}

// TestSchedulerConfig_GraceCoversAWorkersRedial pins the grace the
// server wires. Workers reconnect on their own backoff, independent of the
// server's, so after a long outage a live worker can redial that much later
// than the server did; a grace shorter than worker timeout + longest redial
// (with jitter) + one heartbeat reclaims the tasks of a worker that is merely
// still waiting to reconnect.
func TestSchedulerConfig_GraceCoversAWorkersRedial(t *testing.T) {
	cfg := DefaultConfig()
	got := schedulerConfig(cfg, &bus.Client{}).BusReconnectGrace

	workerTimeout := cfg.Scheduler.WorkerTimeout
	if workerTimeout <= 0 {
		workerTimeout = scheduler.DefaultConfig().WorkerTimeout
	}
	redial := time.Duration(float64(protocol.MaxReconnectBackoff) * (1 + protocol.ReconnectJitterFraction))
	want := workerTimeout + redial + protocol.DefaultHeartbeatInterval
	if got < want {
		t.Errorf("wired BusReconnectGrace = %v, want >= %v (timeout %v + redial %v + heartbeat %v)",
			got, want, workerTimeout, redial, protocol.DefaultHeartbeatInterval)
	}

	cfg.Scheduler.WorkerTimeout = 3 * time.Minute
	if got := schedulerConfig(cfg, &bus.Client{}).BusReconnectGrace; got < 3*time.Minute+redial+protocol.DefaultHeartbeatInterval {
		t.Errorf("grace %v does not follow a configured WorkerTimeout of 3m", got)
	}

	cfg.Scheduler.BusReconnectGrace = 7 * time.Second
	if got := schedulerConfig(cfg, &bus.Client{}).BusReconnectGrace; got != 7*time.Second {
		t.Errorf("BusReconnectGrace = %v, want the caller's 7s kept", got)
	}
}
