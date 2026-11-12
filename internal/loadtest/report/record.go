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
	// HistTaskE2E is one simulated attempt from its lease reaching the worker
	// to the worker publishing its terminal status. It holds the scenario's
	// sampled task duration, so it describes the input as much as the server.
	HistTaskE2E = "task_e2e"
	// HistTerminalIngest is how long the server took to store a task's
	// terminal status: the task's updated_at once terminal minus the moment
	// the simulated worker published that status. A clock difference between
	// the generator and an attached server shifts it.
	HistTerminalIngest = "terminal_ingest"
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
	// CounterLogLines counts the task log lines the simulated workers emitted
	// in the attempts that have ended: the log load offered to the server.
	// Whether the server kept them is the logs invariant's question.
	CounterLogLines = "logs.lines"
	// CounterWorkersLive is the size of the simulated fleet that was meant to
	// be online (a gauge, not a flow: it is not turned into a rate).
	CounterWorkersLive = "workers.live"
	// CounterWSDrops counts WebSocket pushes a client missed.
	CounterWSDrops = "ws.drops"
	// CounterWSEvents counts WebSocket pushes received.
	CounterWSEvents = "ws.events"
	// CounterWSLagNegative counts WebSocket pushes stamped later than they
	// arrived, left out of the lag histogram: a sign of clock skew between
	// the generator and the server.
	CounterWSLagNegative = "ws.lag_negative"
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

// Backlog compares the work left open when submissions stopped with how
// fast the server was finishing work just before.
type Backlog struct {
	// Open is the number of the farm's tasks not yet terminal at WindowEnd.
	Open int64 `json:"open"`
	// TailCompleted is the number of tasks that reached a terminal status in
	// the last tenth of the measured window, from TailFrom to WindowEnd.
	TailCompleted int64     `json:"tail_completed"`
	TailFrom      time.Time `json:"tail_from"`
}

// Finding is one thing the rules flagged about a run.
type Finding struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// FaultRecord is one fault the run injected: when it fired and when it
// healed.
type FaultRecord struct {
	// Index is the fault's position in the scenario's faults.
	Index int    `json:"index"`
	Kind  string `json:"kind"`
	// Target is what the fault hit: "server", "group render (real 0,1)",
	// "groups [rack-b]" or "all".
	Target string    `json:"target"`
	Start  time.Time `json:"start"`
	// Healed is zero when the fault never healed: a worker left dead, an
	// error, or a run interrupted before the heal.
	Healed time.Time `json:"healed"`
	Error  string    `json:"error,omitempty"`
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
	// Started is when submissions began, Ended when the run's jobs had
	// drained (or the drain timed out) and its invariants were about to be
	// checked.
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended"`
	// WindowEnd is when submissions stopped: the end of the measured window,
	// which runs from SteadyFrom to WindowEnd. The run drains from WindowEnd
	// to Ended. Zero (a record without it, or a run stopped before
	// submissions ended) makes the window end at Ended.
	WindowEnd time.Time `json:"window_end"`
	// Histograms cover the whole run, warmup included.
	Histograms map[string]*measure.Histogram `json:"histograms"`
	// Counters are the counter values at the end of the run.
	Counters map[string]int64 `json:"counters"`
	// SteadyCounters are the counter values at SteadyFrom, snapshotted by the
	// runner. A rate over the measured window is (WindowCounters -
	// SteadyCounters) divided by WindowEnd - SteadyFrom. Nil (no snapshot,
	// as with warmup 0) makes every rate a run average, WindowCounters over
	// WindowEnd - Started.
	SteadyCounters map[string]int64 `json:"steady_counters"`
	// WindowCounters are the counter values at WindowEnd, with every key of
	// Counters. Nil (no snapshot) means Counters.
	WindowCounters map[string]int64 `json:"window_counters"`
	// Backlog is the work the server still held when submissions stopped,
	// beside its recent pace; nil when it could not be read.
	Backlog     *Backlog         `json:"backlog,omitempty"`
	Series      []measure.Series `json:"series"`
	Invariants  []verify.Result  `json:"invariants"`
	Unavailable []string         `json:"unavailable"`
	Findings    []Finding        `json:"findings"`
	// Faults are the faults the run injected, in scenario order; empty for a
	// run without faults.
	Faults []FaultRecord `json:"faults,omitempty"`
	// Rework is the work faults made the farm do twice; nil when the run did
	// not measure it (a run without chaos checks, or a store it could not read).
	Rework *Rework `json:"rework,omitempty"`
}

// Rework is the attempts that ran to completion on a worker and still did not
// count: the server recorded another outcome for a simulated worker's, or a
// real task's process finished more times than the server credited.
type Rework struct {
	Attempts int     `json:"attempts"`
	Seconds  float64 `json:"seconds"`
	// ByFault maps a fault's index to the attempts attributed to it: the latest
	// fault that began before the attempt's worker-side end.
	ByFault map[int]int `json:"by_fault,omitempty"`
	// Examples are the first few, at most 20.
	Examples []string `json:"examples,omitempty"`
	// Undercount, when set, says why Attempts leaves out some real tasks'
	// executions: the count is a lower bound, not a measurement.
	Undercount string `json:"undercount,omitempty"`
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

// MeasuredEnd is the end of the measured window: WindowEnd, or Ended when the
// record has none.
func (r *Record) MeasuredEnd() time.Time {
	if r.WindowEnd.IsZero() {
		return r.Ended
	}
	return r.WindowEnd
}

// atWindowEnd is counter k at the end of the measured window.
func (r *Record) atWindowEnd(k string) int64 {
	if r.WindowCounters == nil {
		return r.Counters[k]
	}
	return r.WindowCounters[k]
}

// Drain is how long the run waited, after submissions stopped, for its jobs
// to finish: Ended - WindowEnd, or 0 when the record has no WindowEnd.
func (r *Record) Drain() time.Duration {
	if r.WindowEnd.IsZero() {
		return 0
	}
	return max(r.Ended.Sub(r.WindowEnd), 0)
}

// steadyWindow is the length of the steady-state window, from the end of
// warmup to the end of submissions, never negative. The drain is not in it:
// counting it would dilute every rate by however long the jobs took to
// finish after the generator stopped offering work.
func (r *Record) steadyWindow() time.Duration {
	return max(r.MeasuredEnd().Sub(r.Started)-r.warmup(), 0)
}

// hasSteadyBasis reports whether rates can be measured over the steady window:
// the runner snapshotted the counters at SteadyFrom and the window is not
// empty.
func (r *Record) hasSteadyBasis() bool { return r.SteadyCounters != nil && r.steadyWindow() > 0 }

// inWindow is how much counter k grew in the measured window: from the
// SteadyCounters snapshot when steady is true, else from the start.
func (r *Record) inWindow(k string, steady bool) int64 {
	if steady {
		return r.atWindowEnd(k) - r.SteadyCounters[k]
	}
	return r.atWindowEnd(k)
}

// rate returns counter k per second: its growth over the steady window, from
// the end of warmup to the end of submissions, when steady is true (use
// hasSteadyBasis), else the average from the start to the end of
// submissions. ok is false when the window is empty.
func (r *Record) rate(k string, steady bool) (perSecond float64, ok bool) {
	w := r.MeasuredEnd().Sub(r.Started)
	if steady {
		w = r.steadyWindow()
	}
	if w <= 0 {
		return 0, false
	}
	return float64(r.inWindow(k, steady)) / w.Seconds(), true
}

// inRun returns s with only the points taken at or before Ended. The
// observer's last sample is taken after the fleet has stopped, so it
// describes the teardown, not the run: every peak, end value, slope and rule
// reads the series through this. A record with no Ended keeps every point;
// series.csv keeps every point too.
func (r *Record) inRun(s measure.Series) measure.Series {
	if r.Ended.IsZero() {
		return s
	}
	out := measure.Series{Name: s.Name, Unit: s.Unit}
	for _, p := range s.Points {
		if !p.T.After(r.Ended) {
			out.Points = append(out.Points, p)
		}
	}
	return out
}

// runSeries is every series cut to the run (see inRun).
func (r *Record) runSeries() []measure.Series {
	out := make([]measure.Series, len(r.Series))
	for i, s := range r.Series {
		out[i] = r.inRun(s)
	}
	return out
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
