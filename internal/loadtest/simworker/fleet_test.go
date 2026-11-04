// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/driver"
	"github.com/uberware/sqi/internal/loadtest/internal/testserver"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
)

func fleetGroup(name string, count int, rate float64) scenario.WorkerGroup {
	return scenario.WorkerGroup{
		Name: name, Count: count, CPU: 2, RAMMb: 4096, OS: "linux",
		Ramp:      scenario.Rate{PerSecond: rate},
		Heartbeat: scenario.Duration{Duration: 15 * time.Second},
	}
}

func idleBehavior() simworker.Behavior {
	return fixedPlan(simworker.TaskPlan{Duration: 10 * time.Millisecond, Outcome: simworker.OutcomeSucceeded})
}

func stopFleet(t *testing.T, fl *simworker.Fleet) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		if err := fl.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
}

func TestFleet_RampRate(t *testing.T) {
	f := newFarm(t)
	fl := simworker.NewFleet(simworker.FleetConfig{
		RunID: "r1", FarmID: f.farmID, NATSURL: f.srv.NATSURL(), IDPrefix: "ramp",
		Groups: []scenario.WorkerGroup{fleetGroup("a", 100, 50)}, Seed: 1,
	}, idleBehavior(), simworker.NewLedger(), slog.New(slog.DiscardHandler))

	start := time.Now()
	if err := fl.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	took := time.Since(start)
	t.Logf("100 workers at 50/s were up after %s", took)
	if got := fl.Live(); got != 100 {
		t.Fatalf("Live() = %d after Start, want 100", got)
	}
	if took < 1800*time.Millisecond {
		t.Errorf("fleet was up after %s, want at least 1.8s at 50/s", took)
	}
	if took > 6*time.Second {
		t.Errorf("fleet took %s to start, want at most 6s", took)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err := fl.Stop(ctx); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if got := fl.Live(); got != 0 {
		t.Errorf("Live() = %d after Stop, want 0", got)
	}
}

func TestFleet_IDsUnique(t *testing.T) {
	cfg := simworker.FleetConfig{
		RunID: "run9", FarmID: "farm", NATSURL: "nats://unused", IDPrefix: "pfx",
		Groups: []scenario.WorkerGroup{fleetGroup("render", 5, 0), fleetGroup("comp", 7, 0)},
		Seed:   3,
	}
	fl := simworker.NewFleet(cfg, idleBehavior(), simworker.NewLedger(), nil)
	specs := fl.Specs()
	if len(specs) != 12 {
		t.Fatalf("Specs() has %d entries, want 12", len(specs))
	}
	ids, hosts, seeds := map[string]bool{}, map[string]bool{}, map[uint64]bool{}
	for _, s := range specs {
		if !strings.HasPrefix(s.ID, "pfx-") {
			t.Errorf("ID %q does not carry the prefix", s.ID)
		}
		if !strings.HasPrefix(s.Hostname, "sim-run9-") {
			t.Errorf("hostname %q does not carry the run ID", s.Hostname)
		}
		if ids[s.ID] || hosts[s.Hostname] || seeds[s.RandSeed] {
			t.Errorf("duplicate ID, hostname or seed in %+v", s)
		}
		ids[s.ID], hosts[s.Hostname], seeds[s.RandSeed] = true, true, true
		if s.FarmID != "farm" || s.NATSURL != "nats://unused" {
			t.Errorf("spec %q has farm %q and URL %q", s.ID, s.FarmID, s.NATSURL)
		}
	}
	if !ids["pfx-render-1"] || !ids["pfx-comp-7"] || !hosts["sim-run9-render-1"] {
		t.Errorf("IDs are not <prefix>-<group>-<n> from 1: %v", ids)
	}
}

func TestFleet_StartRejectsDuplicateIDs(t *testing.T) {
	fl := simworker.NewFleet(simworker.FleetConfig{
		RunID: "r", FarmID: "f", NATSURL: "nats://127.0.0.1:1", IDPrefix: "p",
		Groups: []scenario.WorkerGroup{fleetGroup("a", 1, 0), fleetGroup("a", 1, 0)},
	}, idleBehavior(), simworker.NewLedger(), nil)
	err := fl.Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), "p-a-1") {
		t.Fatalf("Start() = %v, want an error naming the duplicate ID p-a-1", err)
	}
}

func TestFleet_StartFailureNamesWorker(t *testing.T) {
	// Nothing listens on port 1, so the first connect fails.
	fl := simworker.NewFleet(simworker.FleetConfig{
		RunID: "r", FarmID: "f", NATSURL: "nats://127.0.0.1:1", IDPrefix: "p",
		Groups: []scenario.WorkerGroup{fleetGroup("a", 3, 0)},
	}, idleBehavior(), simworker.NewLedger(), nil)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	err := fl.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "p-a-") {
		t.Fatalf("Start() = %v, want an error naming the worker", err)
	}
	if got := fl.Live(); got != 0 {
		t.Errorf("Live() = %d, want 0", got)
	}
}

func TestFleet_ChurnRuns(t *testing.T) {
	tests := []struct {
		name  string
		churn scenario.Churn
		check func(t *testing.T, c simworker.ChurnCounts)
	}{
		{
			"crash then restart",
			scenario.Churn{CrashPerHour: 7200, RestartAfter: scenario.Duration{Duration: 200 * time.Millisecond}},
			func(t *testing.T, c simworker.ChurnCounts) {
				if c.Crashes == 0 {
					t.Error("no crash ran in 4s at 2/s per worker")
				}
			},
		},
		{
			"disconnect",
			scenario.Churn{DisconnectPerHour: 1800},
			func(t *testing.T, c simworker.ChurnCounts) {
				if c.Disconnects == 0 {
					t.Error("no disconnect ran in 4s at 0.5/s per worker")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFarm(t)
			g := fleetGroup("c", 2, 0)
			g.Churn = tt.churn
			fl := simworker.NewFleet(simworker.FleetConfig{
				RunID: "r", FarmID: f.farmID, NATSURL: f.srv.NATSURL(), IDPrefix: "churn",
				Groups: []scenario.WorkerGroup{g}, Seed: 11,
			}, idleBehavior(), simworker.NewLedger(), nil)
			if err := fl.Start(t.Context()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			stopFleet(t, fl)

			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			fl.RunChurn(ctx)

			c := fl.ChurnCounts()
			t.Logf("churn: %+v", c)
			tt.check(t, c)
			if c.Failures != 0 {
				t.Errorf("%d churn events failed", c.Failures)
			}
			if got := fl.Live(); got != 2 {
				t.Errorf("Live() = %d after churn, want 2 (every crash restarts)", got)
			}
		})
	}
}

// Stop while churn is running ends the churn, leaves nothing running, and
// counts no event that merely lost the race with it as a failure.
func TestFleet_StopDuringChurn(t *testing.T) {
	f := newFarm(t)
	g := fleetGroup("s", 3, 0)
	g.Churn = scenario.Churn{
		CrashPerHour: 7200, DisconnectPerHour: 3600,
		RestartAfter: scenario.Duration{Duration: 300 * time.Millisecond},
	}
	fl := simworker.NewFleet(simworker.FleetConfig{
		RunID: "r", FarmID: f.farmID, NATSURL: f.srv.NATSURL(), IDPrefix: "stopchurn",
		Groups: []scenario.WorkerGroup{g}, Seed: 21,
	}, idleBehavior(), simworker.NewLedger(), nil)
	if err := fl.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stopFleet(t, fl)

	churnCtx, cancelChurn := context.WithCancel(t.Context())
	defer cancelChurn()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fl.RunChurn(churnCtx)
	}()
	waitFor(t, 10*time.Second, "some churn", func() bool {
		c := fl.ChurnCounts()
		return c.Crashes+c.Disconnects > 0
	})

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err := fl.Stop(ctx); err != nil {
		t.Errorf("Stop: %v", err)
	}
	select {
	case <-done:
	case <-time.After(45 * time.Second):
		t.Fatal("RunChurn did not return after Stop")
	}
	if got := fl.Live(); got != 0 {
		t.Errorf("Live() = %d after Stop, want 0", got)
	}
	if c := fl.ChurnCounts(); c.Failures != 0 {
		t.Errorf("%d churn events counted as failures, want 0: %+v", c.Failures, c)
	}
}

func TestJoinTokenCredentials(t *testing.T) {
	srv := testserver.Start(t, testserver.Options{BrokerAuth: true})
	if srv.JoinToken() == "" {
		t.Fatal("a broker-auth server exposes no join token")
	}
	c := client.New(srv.HTTPURL())
	farmID, err := c.CreateFarm(t.Context(), "auth-farm")
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queueID, err := c.CreateQueue(t.Context(), farmID, "auth-queue", 0)
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	t.Run("a wrong token is refused", func(t *testing.T) {
		_, _, err := simworker.JoinTokenCredentials(c, "not-a-token").For(t.Context(), "w-bad")
		if err == nil {
			t.Fatal("For with a wrong join token succeeded")
		}
	})

	t.Run("a worker with no credential cannot connect", func(t *testing.T) {
		fl := simworker.NewFleet(simworker.FleetConfig{
			RunID: "anon", FarmID: farmID, NATSURL: srv.NATSURL(), IDPrefix: "anon",
			Groups: []scenario.WorkerGroup{fleetGroup("n", 1, 0)},
		}, idleBehavior(), simworker.NewLedger(), nil)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		if err := fl.Start(ctx); err == nil {
			t.Fatal("an anonymous worker connected to a broker with auth on")
		}
	})

	t.Run("two workers enroll and complete one task each", func(t *testing.T) {
		l := simworker.NewLedger()
		// The task holds a worker's whole CPU for a second, so the second
		// worker has to take the second task.
		b := simworker.ScenarioBehavior(map[string]scenario.TaskSpec{"sim": {Duration: constDist(1)}})
		fl := simworker.NewFleet(simworker.FleetConfig{
			RunID: "auth", FarmID: farmID, NATSURL: srv.NATSURL(), IDPrefix: "auth",
			Groups: []scenario.WorkerGroup{fleetGroup("e", 2, 0)},
			Creds:  simworker.JoinTokenCredentials(c, srv.JoinToken()),
			Seed:   5,
		}, b, l, nil)
		if err := fl.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		stopFleet(t, fl)

		waitFor(t, 10*time.Second, "two online workers", func() bool {
			page, err := c.ListWorkers(t.Context(), farmID, 10, 0)
			if err != nil {
				return false
			}
			online := 0
			for _, w := range page.Items {
				if w.Status == "online" {
					online++
				}
			}
			return online == 2
		})

		jc := scenario.JobClass{Name: "sim", Steps: 1, TasksPerStep: 2}
		tmpl, err := driver.BuildTemplate(jc, "sim-1", scenario.Command{Path: "noop"})
		if err != nil {
			t.Fatalf("BuildTemplate: %v", err)
		}
		jobID, _, err := c.SubmitJob(t.Context(), client.SubmitRequest{FarmID: farmID, QueueID: queueID, Template: tmpl})
		if err != nil {
			t.Fatalf("SubmitJob: %v", err)
		}
		f := &farm{c: c}
		f.waitJob(t, jobID, "completed", 30*time.Second)

		byWorker := map[string]int{}
		for _, e := range l.Events() {
			if e.Kind == simworker.KindTerminal && e.Outcome == simworker.OutcomeSucceeded {
				byWorker[e.WorkerID]++
			}
		}
		if len(byWorker) != 2 {
			t.Errorf("tasks succeeded on workers %v, want one on each of two", byWorker)
		}
	})
}
