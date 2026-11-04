// SPDX-License-Identifier: AGPL-3.0-or-later

// Package report turns a finished load run into the files an operator reads:
// the run record (run.json), summary.md and series.csv, the findings rules
// that flag suspicious behavior, run-against-run comparison, the merge of
// shard records and the knee of a sweep.
//
// The record is the contract between the runner, which fills it, and the
// commands that read it back (compare, merge, sweep). The names of its
// histograms and counters are constants in this package so the runner and the
// rules agree on them.
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/uberware/sqi/internal/loadtest/measure"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

// Run modes.
const (
	ModeLoad       = "load"
	ModeSoak       = "soak"
	ModeSweepPoint = "sweep-point"
)

// Keys of Record.Histograms. They match the fields of driver.Metrics.
const (
	// HistSubmitLatency is the HTTP round trip of a job submission.
	HistSubmitLatency = "submit_latency"
	// HistSubmitLag is how late a paced submission left the generator.
	HistSubmitLag = "submit_lag"
	// HistFirstLease is the time from a job's submission to its first task's
	// lease; driver.Metrics.FirstLease. A sweep's knee reads its p95.
	HistFirstLease = "first_lease"
	// HistLeaseReply is a simulated worker's lease request to reply time.
	HistLeaseReply = "lease_reply"
	// HistTaskE2E is a task's assignment to its acknowledged terminal status.
	HistTaskE2E = "task_e2e"
	// HistJobE2E is a job's submission to its terminal status.
	HistJobE2E = "job_e2e"
	// HistWSLag is the WebSocket push lag.
	HistWSLag = "ws_lag"
	// HistRESTPrefix prefixes a REST poller endpoint's histogram key.
	HistRESTPrefix = "rest."
)

// Keys of Record.Counters. The runner fills them from driver.Metrics and the
// verifier; the findings rules and the throughput figures read them.
const (
	// CounterTasksTerminal counts distinct tasks that reached a terminal
	// status on the server.
	CounterTasksTerminal = "tasks.terminal"
	// CounterJobsSubmitted counts jobs the server accepted.
	CounterJobsSubmitted = "jobs.submitted"
	// CounterWorkersLive is the size of the simulated fleet that was meant to
	// be online (a gauge, not a flow: it is not turned into a rate).
	CounterWorkersLive = "workers.live"
	// CounterWSDrops counts WebSocket pushes a client missed.
	CounterWSDrops = "ws.drops"
	// CounterWSEvents counts WebSocket pushes received.
	CounterWSEvents = "ws.events"
	// CounterWSErrors counts failed WebSocket dials and broken connections.
	CounterWSErrors = "ws.errors"
	// CounterRESTErrors counts failed poller calls.
	CounterRESTErrors = "rest.errors"
	// CounterSubmitStatusPrefix prefixes the count of submissions answered with
	// one HTTP status, for example "submit.status.429".
	CounterSubmitStatusPrefix = "submit.status."
)

// Env describes where a run happened.
type Env struct {
	OS         string   `json:"os"`
	Arch       string   `json:"arch"`
	CPUs       int      `json:"cpus"`
	SQIVersion string   `json:"sqi_version"`
	Commit     string   `json:"commit"`
	Store      string   `json:"store"`
	Flags      []string `json:"flags"`
}

// LocalEnv returns an Env with the operating system, architecture and CPU
// count of the machine running the generator. The caller fills the rest.
func LocalEnv() Env {
	return Env{OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU()}
}

// Finding is one thing the rules flagged about a run.
type Finding struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// Record is everything one run measured. It is what run.json holds.
type Record struct {
	RunID string `json:"run_id"`
	// Mode is ModeLoad, ModeSoak or ModeSweepPoint.
	Mode string `json:"mode"`
	// Shard is "i/n" for a shard of a multi-generator run, "" otherwise.
	Shard string `json:"shard,omitempty"`
	// Complete is false when the run ended before its planned duration.
	Complete bool `json:"complete"`
	// Valid is false when the measurement cannot be trusted, typically because
	// the generator was saturated; InvalidReason says why.
	Valid         bool   `json:"valid"`
	InvalidReason string `json:"invalid_reason,omitempty"`
	// Scenario is the scenario as resolved. In a record merged from shards it
	// is shard 0's, whose counts and rates are per shard, not for the whole
	// farm.
	Scenario *scenario.Scenario `json:"scenario"`
	Env      Env                `json:"env"`
	Started  time.Time          `json:"started"`
	Ended    time.Time          `json:"ended"`
	// Histograms cover the whole run, warmup included.
	Histograms map[string]*measure.Histogram `json:"histograms"`
	// Counters are the counter values at the end of the run.
	Counters map[string]int64 `json:"counters"`
	// SteadyCounters are the counter values at SteadyFrom, snapshotted by the
	// runner. A rate over the steady window is (Counters - SteadyCounters)
	// divided by that window. Nil (no snapshot: warmup 0, or an older record)
	// makes every rate a run average, Counters over Ended - Started.
	SteadyCounters map[string]int64 `json:"steady_counters"`
	Series         []measure.Series `json:"series"`
	Invariants     []verify.Result  `json:"invariants"`
	Unavailable    []string         `json:"unavailable"`
	Findings       []Finding        `json:"findings"`
}

// warmup returns the scenario's warmup, or zero when there is no scenario.
func (r *Record) warmup() time.Duration {
	if r.Scenario == nil {
		return 0
	}
	return r.Scenario.Warmup.Duration
}

// SteadyFrom is when the steady-state window starts: the run's start plus the
// scenario's warmup.
func (r *Record) SteadyFrom() time.Time { return r.Started.Add(r.warmup()) }

// steadyWindow is the length of the steady-state window: Ended - Started -
// warmup, never negative.
func (r *Record) steadyWindow() time.Duration {
	return max(r.Ended.Sub(r.Started)-r.warmup(), 0)
}

// hasSteadyBasis reports whether rates can be measured over the steady window:
// the runner snapshotted the counters at SteadyFrom and the window is not
// empty.
func (r *Record) hasSteadyBasis() bool { return r.SteadyCounters != nil && r.steadyWindow() > 0 }

// rate returns counter k per second: over the steady window from the
// SteadyCounters snapshot when steady is true (use hasSteadyBasis), else the
// run average. ok is false when the window is empty.
func (r *Record) rate(k string, steady bool) (perSecond float64, ok bool) {
	if steady {
		w := r.steadyWindow()
		if w <= 0 {
			return 0, false
		}
		return float64(r.Counters[k]-r.SteadyCounters[k]) / w.Seconds(), true
	}
	w := r.Ended.Sub(r.Started)
	if w <= 0 {
		return 0, false
	}
	return float64(r.Counters[k]) / w.Seconds(), true
}

// Write writes run.json, summary.md and series.csv into dir, creating it.
//
// Call it only after every recording into the record's histograms has
// stopped: a histogram marshaled while it is still being recorded into can
// produce invalid JSON.
func (r *Record) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	js, err := json.MarshalIndent(r, "", "  ") //nolint:musttag // the scenario is recorded as resolved, under its Go field names
	if err != nil {
		return fmt.Errorf("report: encode run record: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), append(js, '\n'), 0o644); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	if err := writeFile(filepath.Join(dir, "summary.md"), func(w *os.File) error { return writeSummary(w, r) }); err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, "series.csv"), func(w *os.File) error { return writeSeriesCSV(w, r.Series) })
}

// writeFile creates path and fills it with fill.
func writeFile(path string, fill func(*os.File) error) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("report: %w", err)
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("report: %w", cerr)
		}
	}()
	if err := fill(f); err != nil {
		return fmt.Errorf("report: write %s: %w", filepath.Base(path), err)
	}
	return nil
}

// Read loads a run record from a run.json file, or from a run directory
// holding one.
func Read(path string) (*Record, error) {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		path = filepath.Join(path, "run.json")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("report: %w", err)
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil { //nolint:musttag // see Write
		return nil, fmt.Errorf("report: read %s: %w", path, err)
	}
	return &r, nil
}
