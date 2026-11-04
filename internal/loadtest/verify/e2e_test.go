// SPDX-License-Identifier: AGPL-3.0-or-later

package verify_test

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/driver"
	"github.com/uberware/sqi/internal/loadtest/internal/testserver"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

func constDist(v float64) scenario.Dist { return scenario.Dist{Kind: scenario.DistConstant, Value: v} }

// TestCheck_AgainstARealServer runs a small real run, simulated workers on an
// in-process sqi-server, and requires all six invariants to pass, which shows
// the checks read the server's attempts, retries, cascade cancels and log
// chunks the way the server writes them. Half of all attempts fail, so
// tasks are retried, some fail for good and cancel the step after them, and
// one job is canceled as it starts.
func TestCheck_AgainstARealServer(t *testing.T) {
	srv := testserver.Start(t, testserver.Options{})
	c := client.New(srv.HTTPURL())
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	jc := scenario.JobClass{
		Name: "shot", Queue: "render", Steps: 2, TasksPerStep: 4, Cores: 1, UsagePool: "verify-lic",
		Task: scenario.TaskSpec{Duration: constDist(0.05), LogLines: constDist(120), LogLineBytes: 48, FailRate: 0.5},
	}
	sc := &scenario.Scenario{
		Farm: scenario.FarmSpec{
			Queues:     []scenario.QueueSpec{{Name: "render", MaxConcurrentTasks: 3}},
			UsagePools: []scenario.PoolSpec{{Name: "verify-lic", Limit: 2}},
		},
		Jobs: []scenario.JobClass{jc},
	}
	farmID, err := c.CreateFarm(ctx, "verify-e2e")
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queueID, err := c.CreateQueue(ctx, farmID, "render", 3)
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	if _, err := c.CreateUsagePool(ctx, "verify-lic", 2); err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}

	ledger := simworker.NewLedger()
	fleet := simworker.NewFleet(simworker.FleetConfig{
		RunID: "verify", FarmID: farmID, NATSURL: srv.NATSURL(), IDPrefix: "verify",
		Groups: []scenario.WorkerGroup{{
			Name: "sim", Count: 3, CPU: 2, RAMMb: 4096, OS: "linux",
			Heartbeat: scenario.Duration{Duration: 15 * time.Second},
		}},
		QueueIDs: map[string]string{"render": queueID},
		Seed:     7,
	}, simworker.ScenarioBehavior(map[string]scenario.TaskSpec{"shot": jc.Task}), ledger, slog.New(slog.DiscardHandler))
	if err := fleet.Start(ctx); err != nil {
		t.Fatalf("fleet Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := fleet.Stop(stopCtx); err != nil {
			t.Errorf("fleet Stop: %v", err)
		}
	})

	var jobs []driver.SubmittedJob
	for i := 1; i <= 4; i++ {
		name := fmt.Sprintf("shot-%d", i)
		tmpl, err := driver.BuildTemplate(jc, name, scenario.Command{Path: "noop"})
		if err != nil {
			t.Fatalf("BuildTemplate: %v", err)
		}
		submitted := time.Now()
		id, status, err := c.SubmitJob(ctx, client.SubmitRequest{FarmID: farmID, QueueID: queueID, Template: tmpl})
		if err != nil {
			t.Fatalf("SubmitJob: %v", err)
		}
		jobs = append(jobs, driver.SubmittedJob{ID: id, Name: name, Class: jc.Name, SubmittedAt: submitted, AcceptedAt: time.Now(), Status: status})
	}
	cancelRec := driver.ActionRecord{Kind: driver.ActionCancelJobs, Started: time.Now(), JobIDs: []string{jobs[3].ID}}
	if err := c.CancelJob(ctx, jobs[3].ID); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	recs := []driver.ActionRecord{cancelRec}

	if err := verify.WaitDrained(ctx, c, farmID, jobs, time.Minute); err != nil {
		t.Fatalf("WaitDrained: %v", err)
	}
	// Each job is 2 steps x 4 tasks, so outcomes also checks no task row is
	// missing.
	results := verify.Check(ctx, c, verify.Input{
		Jobs: jobs, Ledger: ledger, Scenario: sc, FarmID: farmID,
		LiveWorkers: fleet.Live(), LogSample: 1, AttemptSample: 1, Seed: 1,
		CanceledAt: verify.CancelTimes(recs), ConvergeWithin: 10 * time.Second,
	})
	for _, r := range results {
		if !r.OK {
			t.Errorf("%s failed: %q", r.Name, r.Evidence)
		}
	}

	// The run must have exercised what it is meant to prove.
	leases := map[string]int{}
	failed := 0
	for _, e := range ledger.Events() {
		switch {
		case e.Kind == simworker.KindLeased:
			leases[e.TaskID]++
		case e.Kind == simworker.KindTerminal && e.Outcome == simworker.OutcomeFailed:
			failed++
		}
	}
	retried := 0
	for _, n := range leases {
		if n > 1 {
			retried++
		}
	}
	statuses := map[string]int{}
	for _, j := range jobs {
		got, err := c.GetJob(ctx, j.ID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		statuses[got.Status]++
	}
	t.Logf("%d tasks leased, %d of them more than once; %d failed attempts; jobs %v", len(leases), retried, failed, statuses)
	if retried == 0 {
		t.Error("no task was retried, so the retry path went unchecked")
	}

	// A task's 120 lines reach the server as several multi-line chunks.
	var succeeded string
	for _, e := range ledger.Events() {
		if e.Kind == simworker.KindTerminal && e.Outcome == simworker.OutcomeSucceeded {
			succeeded = e.TaskID
		}
	}
	chunks, _, err := c.TaskLogs(ctx, succeeded, 0, 1000)
	if err != nil {
		t.Fatalf("TaskLogs: %v", err)
	}
	if len(chunks) < 2 {
		t.Errorf("task %s has %d log chunks, want several", succeeded, len(chunks))
	}

	// The same run with the ledger disagreeing about one task fails outcomes.
	if !ledger.SetOutcome(succeeded, simworker.OutcomeFailed) {
		t.Fatalf("SetOutcome(%s) found no terminal event", succeeded)
	}
	in := verify.Input{
		Jobs: jobs, Ledger: ledger, Scenario: sc, FarmID: farmID, LiveWorkers: fleet.Live(),
		CanceledAt: verify.CancelTimes(recs), Settle: time.Millisecond,
	}
	if r := verify.Check(ctx, c, in)[1]; r.OK || len(r.Evidence) != 1 {
		t.Errorf("outcomes after SetOutcome = %+v, want one violation naming task %s", r, succeeded)
	}
}
