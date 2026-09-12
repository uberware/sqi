// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// preset_exec_test.go — Tier 3 of the preset validation harness (Phase 4, P1).
//
// Runs a shipped preset through a REAL server and a REAL sqi-worker subprocess
// with the vendor command replaced by test/stubproc on PATH, so the whole
// lease → assign → execute → status → log path runs with no license and no
// vendor install. Then it cross-checks the argv the stub RECORDED against the
// computed Tier-1 golden: the goldens are only evidence because something
// independent confirms them.
//
// NOT tagged `integration` — see preset_argv_test.go's header for why.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/presettest"
	"github.com/uberware/sqi/internal/store"
	stubrecord "github.com/uberware/sqi/test/stubproc/record"
)

// presetJobTimeout covers worker registration plus a stubbed "render".
const presetJobTimeout = 90 * time.Second

// tier3Env is the stub configuration plus PATH for one Tier-3 run.
type tier3Env struct {
	stubDir    string
	recordPath string
	env        []string
}

// newTier3Env installs the stub under each name and returns the worker
// environment that makes a worker resolve those commands to it.
func newTier3Env(t *testing.T, names []string, extra ...string) tier3Env {
	t.Helper()
	stubDir := t.TempDir()
	if err := presettest.InstallStub(stubDir, names); err != nil {
		t.Skipf("InstallStub: %v (no Go toolchain?)", err)
	}
	recordPath := filepath.Join(t.TempDir(), "records.jsonl")
	env := append([]string{
		"PATH=" + stubDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"SQI_STUB_RECORD=" + recordPath,
	}, extra...)
	return tier3Env{stubDir: stubDir, recordPath: recordPath, env: env}
}

// records decodes everything the stub wrote, in invocation order.
func (e tier3Env) records(t *testing.T) []stubrecord.Record {
	t.Helper()
	data, err := os.ReadFile(e.recordPath)
	if err != nil {
		t.Fatalf("read stub records: %v", err)
	}
	var out []stubrecord.Record
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var rec stubrecord.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode stub record %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// capturePresetCase returns the computed snapshot for one registry entry and
// case — the Tier-1 answer the observed run is checked against.
func capturePresetCase(t *testing.T, entry presettest.Entry, c presettest.Case) presettest.Snapshot {
	t.Helper()
	tmpl, err := presettest.PresetTemplate(entry.Source, entry.Name)
	if err != nil {
		t.Fatalf("PresetTemplate: %v", err)
	}
	if c.Patch != nil {
		tmpl, err = presettest.ApplyChunkPatch(tmpl, *c.Patch)
		if err != nil {
			t.Fatalf("ApplyChunkPatch: %v", err)
		}
	}
	snap, err := presettest.Capture(t.Context(), tmpl, presettest.Options{
		Params: c.Params, Format: store.TemplateFormatYAML,
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	snap.Preset, snap.Case, snap.Patch = entry.Name, c.Name, c.Patch
	return snap
}

// submitPresetJob posts tmpl to POST /api/v1/jobs with params bound as
// param.<Name> query values -- the convention parseParamQueryParams
// (internal/api/jobs.go) expects. presettest.Case.Params holds bare parameter
// names (they are also fed straight to openjd.SubmitOptions in
// presettest.Capture, which has no such prefix), so this prefixing is required:
// without it every param is silently dropped and a preset with a required
// parameter like SceneFile fails submission outright.
//
// This is a local, untagged equivalent of retry_test.go's submitJobWithParams:
// that helper lives behind `//go:build integration`, so a preset_exec_test.go
// that must run untagged (see this file's header) cannot call it.
func submitPresetJob(t *testing.T, ts *testServer, farmID, queueID, tmpl string, params map[string]string) string {
	t.Helper()
	q := url.Values{}
	q.Set("farm_id", farmID)
	q.Set("queue_id", queueID)
	q.Set("owner", "test")
	for k, v := range params {
		q.Set("param."+k, v)
	}
	var resp struct {
		ID string `json:"id"`
	}
	mustDoJSON(t, http.MethodPost, apiURL(ts, "/api/v1/jobs")+"?"+q.Encode(),
		[]byte(tmpl), "application/x-yaml", http.StatusCreated, &resp)
	if resp.ID == "" {
		t.Fatal("submitPresetJob: server returned empty job ID")
	}
	return resp.ID
}

// TestPresetTier3Maya runs maya-layer-render end to end against the stub.
// Task 12 generalizes this into a table over the registry; it exists on its own
// first so the mechanism is proven on one preset before it is applied to all.
func TestPresetTier3Maya(t *testing.T) {
	const caseName = "default"
	presettest.RecordOutcome("TestPresetTier3Maya", false, "")
	if runtime.GOOS == "windows" {
		presettest.RecordOutcome("TestPresetTier3Maya", true, "POSIX worker path")
		t.Skip("preset tier-3 uses a POSIX worker; skipping on Windows")
	}

	reg, err := presettest.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	entry, ok := reg.Entry("maya-layer-render")
	if !ok {
		t.Fatal("registry has no maya-layer-render entry")
	}
	cases, err := presettest.LoadCases(filepath.Join(presetCaseDir, entry.Name+".yaml"))
	if err != nil {
		t.Fatalf("LoadCases: %v", err)
	}
	var c presettest.Case
	for _, candidate := range cases {
		if candidate.Name == caseName {
			c = candidate
		}
	}
	if c.Name == "" {
		t.Fatalf("case %q not found in %s.yaml", caseName, entry.Name)
	}

	snap := capturePresetCase(t, entry, c)
	names := presettest.CommandNames(snap)

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	// "maya" alone is a presence-only tag (Tags["maya"] = ""), which does not
	// satisfy the preset's "attr.worker.tag.maya anyOf [\"true\"]" requirement
	// (internal/worker/capabilities.MergeManualTags, internal/scheduler/matcher.go
	// workerAttributeValue) -- it must be key=value.
	env := newTier3Env(t, names, "SQI_WORKER_CAPABILITY_TAGS=maya=true")
	startRealWorkerWithOptions(t, ts, farmID, queueID, nil, env.env)

	tmpl, err := presettest.PresetTemplate(entry.Source, entry.Name)
	if err != nil {
		t.Fatalf("PresetTemplate: %v", err)
	}
	jobID := submitPresetJob(t, ts, farmID, queueID, tmpl, c.Params)
	// Job statuses are "completed"/"failed"/"canceled"/"paused" (store.JobStatus)
	// -- "succeeded" is a TASK status, not a job one.
	if status := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, presetJobTimeout); status != "completed" {
		t.Fatalf("job status = %q, want completed", status)
	}

	recs := env.records(t)
	if len(recs) != c.ExpectTasks {
		t.Fatalf("stub was invoked %d times, fixture expects %d", len(recs), c.ExpectTasks)
	}
	assertObservedMatchesComputed(t, snap, recs)
}

// assertObservedMatchesComputed is the cross-check: every argv the stub
// recorded must equal an argv the computed snapshot predicted.
//
// Order is not asserted — tasks are leased concurrently, so the run order is
// genuinely nondeterministic. Set equality is the real claim.
func assertObservedMatchesComputed(t *testing.T, snap presettest.Snapshot, recs []stubrecord.Record) {
	t.Helper()
	computed := map[string]bool{}
	for _, step := range snap.Steps {
		for _, task := range step.Tasks {
			computed[argvKey(filepath.Base(task.Command), task.Args)] = true
		}
	}
	for _, rec := range recs {
		key := argvKey(rec.Command, rec.Args)
		if !computed[key] {
			t.Errorf("observed argv has no computed counterpart:\n  observed: %s\n  computed set: %v",
				key, keysOf(computed))
		}
	}
}

func argvKey(command string, args []string) string {
	return command + " " + strings.Join(args, "\x1f")
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestPresetTier3_StubFailureSurfacesAsFailedTask proves the harness can see a
// failure, not just a success: a non-zero vendor exit must reach the API as a
// failed task carrying a failure_reason.
func TestPresetTier3_StubFailureSurfacesAsFailedTask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("preset tier-3 uses a POSIX worker; skipping on Windows")
	}
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	env := newTier3Env(t, []string{"failing-renderer"}, "SQI_STUB_EXIT=3")
	startRealWorkerWithOptions(t, ts, farmID, queueID, nil, env.env)

	jobID := submitJobCustomYAML(t, ts, farmID, queueID, stubJobYAML("failing-renderer"))
	if status := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, presetJobTimeout); status != "failed" {
		t.Fatalf("job status = %q, want failed", status)
	}
	reason := firstTaskFailureReason(t, ts, jobID)
	if reason == "" {
		t.Error("failed task carries no failure_reason")
	}
}

// TestPresetTier3_StubHangIsKilledByTimeout proves the timeout path: a vendor
// command that never returns must not wedge the task forever.
func TestPresetTier3_StubHangIsKilledByTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("preset tier-3 uses a POSIX worker; skipping on Windows")
	}
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	env := newTier3Env(t, []string{"hanging-renderer"}, "SQI_STUB_SLEEP=120s")
	startRealWorkerWithOptions(t, ts, farmID, queueID, nil, env.env)

	// timeout: 2 in the template, so the worker kills the process after ~2s.
	jobID := submitJobCustomYAML(t, ts, farmID, queueID, stubJobYAMLWithTimeout("hanging-renderer", 2))
	if status := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, presetJobTimeout); status != "failed" {
		t.Fatalf("job status = %q, want failed (timed out)", status)
	}
}

// firstTaskFailureReason fetches a job's task list and returns the first
// non-empty failure_reason found, or "" if every task's is empty.
func firstTaskFailureReason(t *testing.T, ts *testServer, jobID string) string {
	t.Helper()
	var resp struct {
		Items []struct {
			FailureReason string `json:"failure_reason"`
		} `json:"items"`
	}
	mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/jobs/"+jobID+"/tasks"), nil, "", http.StatusOK, &resp)
	for _, item := range resp.Items {
		if item.FailureReason != "" {
			return item.FailureReason
		}
	}
	return ""
}

// stubJobYAML is a minimal single-task template invoking command.
func stubJobYAML(command string) string {
	return fmt.Sprintf(`specificationVersion: jobtemplate-2023-09
name: Stub Job
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: %s
`, command)
}

// stubJobYAMLWithTimeout is [stubJobYAML] with an onRun timeout in seconds.
func stubJobYAMLWithTimeout(command string, seconds int) string {
	return fmt.Sprintf(`specificationVersion: jobtemplate-2023-09
name: Stub Timeout Job
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: %s
          timeout: %d
`, command, seconds)
}
