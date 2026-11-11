// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/chaos"
	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/driver"
	"github.com/uberware/sqi/internal/loadtest/internal/sleep"
	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/loadtest/target"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

// plainTarget is a target with no fault injection.
type plainTarget struct{ closed bool }

func (*plainTarget) HTTPURL() string                    { return "http://127.0.0.1:1" }
func (*plainTarget) NATSURL() string                    { return "nats://127.0.0.1:1" }
func (*plainTarget) APIKey() string                     { return "" }
func (*plainTarget) FarmID() string                     { return "farm" }
func (*plainTarget) QueueIDs() map[string]string        { return map[string]string{"render": "q"} }
func (*plainTarget) DataDir() string                    { return "" }
func (*plainTarget) Credentials() simworker.Credentials { return nil }
func (*plainTarget) TLS() simworker.TLSFiles            { return simworker.TLSFiles{} }
func (*plainTarget) NoopCommand() scenario.Command      { return scenario.Command{} }
func (*plainTarget) PoolName(name string) string        { return name }
func (*plainTarget) PostgresDSN() string                { return "" }
func (*plainTarget) StoreLabel() string                 { return "fake" }
func (p *plainTarget) Close(context.Context) error {
	p.closed = true
	return nil
}

// latencyScenario is the smoke scenario, 10 s of 5 jobs without churn, with a
// latency fault on its one group from 2 s to 4 s.
func latencyScenario(t *testing.T) *scenario.Scenario {
	t.Helper()
	sc := smokeScenario(t, "10s")
	sc.Workers[0].Churn = scenario.Churn{}
	sc.Jobs[0].Total = 5
	sc.Faults = []scenario.Fault{{
		At:       scenario.Duration{Duration: 2 * time.Second},
		Duration: scenario.Duration{Duration: 2 * time.Second},
		Latency:  &scenario.LatencySpec{Groups: []string{sc.Workers[0].Name}, Delay: scenario.Duration{Duration: time.Millisecond}},
	}}
	if err := sc.Validate(scenario.TargetManaged); err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestRun_FaultsRejectedWithoutFaultTarget(t *testing.T) {
	tgt := &plainTarget{}
	ctlMade := false
	rec, err := Run(t.Context(), Options{
		Scenario: latencyScenario(t), RunID: "no-faults", OutDir: t.TempDir(), Logger: testLogger(t),
		startTarget: func(context.Context, *run) (target.Target, error) { return tgt, nil },
		newController: func([]scenario.Fault, []scenario.WorkerGroup, target.Faults, chaos.Fleet, *slog.Logger) (faultController, error) {
			ctlMade = true
			return nil, errors.New("unreachable")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot inject") {
		t.Fatalf("Run = %v, %v; want an error saying the target cannot inject faults", rec, err)
	}
	if rec != nil {
		t.Errorf("Run returned a record for a run that could not be set up")
	}
	if ctlMade {
		t.Error("a fault controller was made for a target without faults")
	}
	if !tgt.closed {
		t.Error("the target was not closed")
	}
}

// slowHeal is a fault controller whose Run returns heal after the end of
// submissions.
type slowHeal struct {
	duration, heal time.Duration

	mu       sync.Mutex
	start    time.Time
	returned time.Time
	healAll  int
}

func (s *slowHeal) Run(ctx context.Context, start time.Time) error {
	s.mu.Lock()
	s.start = start
	s.mu.Unlock()
	ok := sleep.Until(ctx, start.Add(s.duration+s.heal))
	s.mu.Lock()
	s.returned = time.Now()
	s.mu.Unlock()
	if !ok {
		return context.Cause(ctx)
	}
	return nil
}

func (s *slowHeal) Records() []report.FaultRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []report.FaultRecord{{Index: 0, Kind: scenario.FaultLatency, Target: "groups [sim]", Start: s.start.Add(2 * time.Second), Healed: s.returned}}
}

func (s *slowHeal) Windows() []verify.FaultWindow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []verify.FaultWindow{{Start: s.start.Add(2 * time.Second), End: s.start.Add(4 * time.Second), Groups: []string{"sim"}}}
}

func (s *slowHeal) HealAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healAll++
}

// TestRun_DrainWaitsForFaultsToHeal runs a fault scenario against a real
// sqi-server with a controller whose faults heal 2 s after submissions end:
// the drain must not start before they have.
func TestRun_DrainWaitsForFaultsToHeal(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real sqi binaries; skipped with -short")
	}
	sc := latencyScenario(t)
	ctl := &slowHeal{duration: sc.Duration.Duration, heal: 2 * time.Second}
	var (
		mu         sync.Mutex
		firstDrain time.Time
		drained    []driver.SubmittedJob
	)
	var gotFaults target.Faults
	rec, err := Run(t.Context(), Options{
		Scenario: sc, RunID: "drain-after-heal", OutDir: t.TempDir(), Seed: 3, Managed: binaries(t), Logger: testLogger(t),
		newController: func(faults []scenario.Fault, _ []scenario.WorkerGroup, tf target.Faults, _ chaos.Fleet, _ *slog.Logger) (faultController, error) {
			if len(faults) != 1 {
				t.Errorf("controller made with %d faults, want 1", len(faults))
			}
			gotFaults = tf
			return ctl, nil
		},
		waitDrained: func(ctx context.Context, v verify.ServerView, farmID string, jobs []driver.SubmittedJob, timeout time.Duration) error {
			mu.Lock()
			if firstDrain.IsZero() {
				firstDrain, drained = time.Now(), slices.Clone(jobs)
			}
			mu.Unlock()
			return verify.WaitDrained(ctx, v, farmID, jobs, timeout)
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotFaults == nil || gotFaults.Lane(sc.Workers[0].Name) == nil {
		t.Errorf("the controller's target %v has no lane for group %s", gotFaults, sc.Workers[0].Name)
	}
	ctl.mu.Lock()
	returned, healAll := ctl.returned, ctl.healAll
	ctl.mu.Unlock()
	switch {
	case returned.IsZero():
		t.Fatal("the controller's Run never returned")
	case firstDrain.IsZero():
		t.Fatal("the run never waited for its jobs to drain")
	case firstDrain.Before(returned):
		t.Errorf("the drain started at %s, %s before the faults healed at %s", stamp(firstDrain), returned.Sub(firstDrain), stamp(returned))
	}
	if healAll == 0 {
		t.Error("teardown did not call HealAll")
	}
	if !rec.Complete {
		t.Fatal("the run is incomplete")
	}
	if len(drained) < 5 {
		t.Errorf("WaitDrained was given %d jobs, want the 5 accepted ones at least", len(drained))
	}
	if len(rec.Faults) != 1 || rec.Faults[0].Kind != scenario.FaultLatency {
		t.Errorf("record faults = %+v, want the controller's one record", rec.Faults)
	}
	var names []string
	for _, inv := range rec.Invariants {
		names = append(names, inv.Name)
		if !inv.OK {
			t.Errorf("invariant %s failed: %q", inv.Name, inv.Evidence)
		}
		if inv.Name == verify.NameLogs {
			if _, ok := inv.Counters[verify.CounterLogsLostInFault]; !ok {
				t.Errorf("the logs result carries no %s counter: %v", verify.CounterLogsLostInFault, inv.Counters)
			}
		}
	}
	want := []string{
		verify.NameTerminal, verify.NameOutcomes, verify.NameSingleAttempt, verify.NameLogs,
		verify.NameWorkersConverge, verify.NameCaps, verify.NameSubmissionAtomic,
		chaos.NameStoreIntegrity, chaos.NameExecutionsRecorded,
	}
	if !slices.Equal(names, want) {
		t.Errorf("invariants %v, want %v", names, want)
	}
}

func stamp(t time.Time) string { return t.Format("15:04:05.000") }

func TestOrphanJobs(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	farm := map[string]client.Job{
		"j1": {ID: "j1", Name: "shot-1", CreatedAt: t0},
		// A submission that timed out after the server had created its job.
		"o1": {ID: "o1", Name: "shot-2", CreatedAt: t0.Add(time.Second)},
		// A second job with the name of a rejected submission, created later:
		// not an orphan; submission-atomic reports it.
		"o1b": {ID: "o1b", Name: "shot-2", CreatedAt: t0.Add(3 * time.Second)},
		// A job of an accepted submission's name but another ID.
		"dup": {ID: "dup", Name: "shot-1", CreatedAt: t0.Add(2 * time.Second)},
	}
	accepted := []driver.SubmittedJob{{ID: "j1", Name: "shot-1", Class: "shot", Status: 201}}
	rejected := []driver.SubmittedJob{
		{Name: "shot-2", Class: "shot", Status: 0},
		{Name: "shot-3", Class: "shot", Status: 503}, // created nothing
	}
	got := orphanJobs(farm, accepted, rejected)
	if len(got) != 1 || got[0].ID != "o1" || got[0].Name != "shot-2" || got[0].Class != "shot" || got[0].Accepted() {
		t.Errorf("orphanJobs = %+v, want shot-2 as o1, still not accepted", got)
	}
	if none := orphanJobs(map[string]client.Job{"j1": farm["j1"]}, accepted, rejected); none == nil || len(none) != 0 {
		t.Errorf("orphanJobs with none = %#v, want an empty, non-nil slice", none)
	}
}

func TestChaosConvergeWithin(t *testing.T) {
	sc := smokeScenario(t, "20s")
	base := convergeWithin(sc)
	grace := livenessGrace(heartbeatTimeout(sc)) + serverSweepInterval + convergeMargin
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		serverUp time.Time
		faults   []report.FaultRecord
		want     time.Duration
	}{
		{name: "long after the server started", serverUp: now.Add(-time.Hour), want: base},
		{name: "server just started", serverUp: now.Add(-time.Minute), want: grace - time.Minute},
		{
			name: "server restarted by a fault", serverUp: now.Add(-time.Hour),
			faults: []report.FaultRecord{
				{Kind: scenario.FaultKill, Target: "server", Start: now.Add(-40 * time.Second), Healed: now.Add(-30 * time.Second)},
				{Kind: scenario.FaultKill, Target: "group sim (real 0)", Start: now.Add(-5 * time.Second), Healed: now},
			},
			want: grace - 30*time.Second,
		},
		{
			name: "power cut never healed", serverUp: now.Add(-time.Hour),
			faults: []report.FaultRecord{{Kind: scenario.FaultPowerCut, Target: "all", Start: now.Add(-time.Minute)}},
			want:   grace,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chaosConvergeWithin(sc, tc.serverUp, tc.faults, now); got != max(tc.want, base) {
				t.Errorf("chaosConvergeWithin = %s, want %s", got, max(tc.want, base))
			}
		})
	}
}
