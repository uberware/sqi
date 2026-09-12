// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// preset_exec_test.go — Tier 3 of the preset validation harness (Phase 4, P1).
//
// Runs every preset the registry claims Tier 3 for through a REAL server and a
// REAL sqi-worker subprocess with the vendor command replaced by test/stubproc
// on PATH, so the whole lease → assign → execute → status → log path runs with
// no license and no vendor install. Then it cross-checks the argv the stub
// RECORDED against the computed Tier-1 golden: the goldens are only evidence
// because something independent confirms them.
//
// NOT tagged `integration` — see preset_argv_test.go's header for why.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/presettest"
	"github.com/uberware/sqi/internal/store"
	stubrecord "github.com/uberware/sqi/test/stubproc/record"
)

// The stub binary is cached across every test in this package (see
// presettest.BuildStub), so no single test can own its directory. TestMain
// drains packageCleanups; this is the same arrangement the OIDC suite's
// container uses.
func init() {
	packageCleanups = append(packageCleanups, presettest.RemoveStub)
}

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
//
// A missing file means the stub was never invoked at all, which is a real
// outcome rather than an I/O error: it is what a preset whose command the stub
// failed to shadow looks like. Returning no records lets
// [assertInvocationCounts] say "ffmpeg invoked 0 times, fixture expects 3"
// instead of failing on a confusing ENOENT.
func (e tier3Env) records(t *testing.T) []stubrecord.Record {
	t.Helper()
	data, err := os.ReadFile(e.recordPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
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

// presetCaseTemplate returns the template one case runs: the shipped preset's
// bytes, with the case's chunk patch applied when it declares one.
//
// Both the computed side ([capturePresetCase]) and the submitted side
// ([runTier3Case]) must go through this. Fetching the template twice and
// patching only one of them would make the cross-check compare a 2-task
// expansion against a 10-task one, and the diff would read as a preset defect.
func presetCaseTemplate(t *testing.T, entry presettest.Entry, c presettest.Case) string {
	t.Helper()
	tmpl, err := presettest.PresetTemplate(entry.Source, entry.Name)
	if err != nil {
		t.Fatalf("PresetTemplate: %v", err)
	}
	if c.Patch == nil {
		return tmpl
	}
	patched, err := presettest.ApplyChunkPatch(tmpl, *c.Patch)
	if err != nil {
		t.Fatalf("ApplyChunkPatch: %v", err)
	}
	return patched
}

// capturePresetCase returns the computed snapshot for one registry entry and
// case — the Tier-1 answer the observed run is checked against.
func capturePresetCase(t *testing.T, entry presettest.Entry, c presettest.Case) presettest.Snapshot {
	t.Helper()
	tmpl := presetCaseTemplate(t, entry, c)
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

// TestPresetTier3 runs every preset the registry claims Tier 3 for, end to end
// against the stub, and cross-checks the observed argv against the computed
// golden.
//
// Each subtest records its outcome — including its skip — through
// presettest.RecordOutcome, because the registry treats a skip on a platform it
// lists in required_on as a FAILURE. A target that can pass while running
// nothing is worse than no target.
func TestPresetTier3(t *testing.T) {
	reg, err := presettest.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	for _, entry := range reg.Presets {
		if entry.Tier3 == nil {
			continue
		}
		cases, err := presettest.LoadCases(filepath.Join(presetCaseDir, entry.Name+".yaml"))
		if err != nil {
			t.Fatalf("%s: LoadCases: %v", entry.Name, err)
		}
		claimed := false
		for _, c := range cases {
			caseName := "TestPresetTier3/" + entry.Name + "/" + c.Name
			if entry.Tier3.Case != caseName {
				continue // the registry names one case per preset for Tier 3
			}
			claimed = true
			t.Run(entry.Name+"/"+c.Name, func(t *testing.T) {
				runTier3Case(t, entry, c, caseName)
			})
		}
		// A tier3.case naming a case the fixture does not define would otherwise
		// leave the claim silently unexecuted -- the precise failure mode
		// RecordOutcome exists to make visible.
		if !claimed {
			t.Errorf("%s: tier3 claims case %q but %s.yaml defines no such case",
				entry.Name, entry.Tier3.Case, entry.Name)
		}
	}
}

func runTier3Case(t *testing.T, entry presettest.Entry, c presettest.Case, caseName string) {
	t.Helper()
	presettest.RecordOutcome(caseName, false, "")
	if runtime.GOOS == "windows" {
		presettest.RecordOutcome(caseName, true, "preset tier-3 uses a POSIX worker")
		t.Skip("preset tier-3 uses a POSIX worker; skipping on Windows")
	}
	if want := unsatisfiableOSFamily(t, entry); want != "" {
		reason := fmt.Sprintf("preset requires attr.worker.os.family %q; this host reports %q", want, hostOSFamily())
		presettest.RecordOutcome(caseName, true, reason)
		t.Skip(reason)
	}

	snap := capturePresetCase(t, entry, c)
	names := presettest.CommandNames(snap)
	if scriptShaped(snap) {
		// The onRun command is an absolute path, which cannot be shadowed on
		// PATH. The commands the SHELL (or the script) invokes are what the stub
		// intercepts, so install those instead — the fixture names them.
		names = invocationNames(c)
	}
	if len(names) == 0 {
		t.Fatalf("%s: nothing to stub -- give the case expect_invocations", entry.Name)
	}

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	env := newTier3Env(t, names, workerTagEnv(t, entry)...)
	startRealWorkerWithOptions(t, ts, farmID, queueID, nil, env.env)

	jobID := submitPresetJob(t, ts, farmID, queueID, presetCaseTemplate(t, entry, c), c.Params)
	// Job statuses are "completed"/"failed"/"canceled"/"paused" (store.JobStatus)
	// -- "succeeded" is a TASK status, not a job one.
	if status := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, presetJobTimeout); status != "completed" {
		t.Fatalf("job status = %q, want completed (first task failure_reason: %q)",
			status, firstTaskFailureReason(t, ts, jobID))
	}

	recs := env.records(t)
	// Both assertions below are vacuous on an empty recording:
	// assertInvocationCounts returns early when the fixture names no expected
	// invocations (which Case's doc comment permits), and
	// assertObservedMatchesComputed iterates OVER recs, so its body never runs.
	// Without this guard such a case would pass having observed only that the job
	// reached "completed" -- which the stub was installed precisely to go beyond.
	if len(recs) == 0 {
		t.Fatalf("stub recorded nothing; Tier 3 observed only that the job completed")
	}
	assertInvocationCounts(t, c, recs)
	if !scriptShaped(snap) {
		assertObservedMatchesComputed(t, snap, recs)
	}
}

// scriptShaped reports whether any of this preset's task commands is an
// ABSOLUTE path, which is the one shape [presettest.InstallStub] cannot
// intercept: the worker execs the path directly and never consults PATH.
//
// Exactly one shipped case is in this shape today — the `script` built-in runs
// `/bin/sh -c "<Command>"` — and a materialized embedded file used as the
// command ("<WORKDIR>/main.sh") would be too. For these the stub intercepts what
// the shell or script invokes INSIDE, so the observed argv has no computed
// counterpart (the computed side is the shell's own argv) and the
// observed-vs-computed cross-check cannot apply: expect_invocations carries the
// whole claim.
func scriptShaped(snap presettest.Snapshot) bool {
	for _, step := range snap.Steps {
		for _, task := range step.Tasks {
			if filepath.IsAbs(task.Command) {
				return true
			}
		}
	}
	return false
}

// invocationNames returns the command basenames a case expects the stub to
// observe, sorted so InstallStub is deterministic.
func invocationNames(c presettest.Case) []string {
	out := make([]string, 0, len(c.ExpectInvocations))
	for name := range c.ExpectInvocations {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// hostAttributeRequirements returns every attribute host requirement this
// preset's steps declare, read out of the parsed template.
//
// Parsed rather than pattern-matched over the template text: the same
// requirements decide BOTH which capability tags the worker must advertise and
// whether this host can run the preset at all, and a regexp cannot tell an
// `anyOf` value from an `allOf` one or a requirement from a mention.
func hostAttributeRequirements(t *testing.T, entry presettest.Entry) []openjd.AttributeRequirement {
	t.Helper()
	raw, err := presettest.PresetTemplate(entry.Source, entry.Name)
	if err != nil {
		t.Fatalf("PresetTemplate: %v", err)
	}
	tmpl, err := openjd.Parse([]byte(raw), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("openjd.Parse %s: %v", entry.Name, err)
	}
	var out []openjd.AttributeRequirement
	for _, step := range tmpl.Steps {
		if step.HostRequirements == nil {
			continue
		}
		out = append(out, step.HostRequirements.Attributes...)
	}
	return out
}

// workerTagEnv returns the SQI_WORKER_CAPABILITY_TAGS entry that satisfies this
// preset's attr.worker.tag.* requirements, or nothing when it declares none.
//
// The tags are FORCED rather than left to detection: the stub happens to satisfy
// a binary-on-PATH detector, but relying on that coincidence would make this
// test's scheduling depend on it, and a detector change would surface here as a
// job stuck in `pending` rather than as a failure in the detector's own test.
//
// They are spelled key=value, not as bare names. A bare tag is stored by
// capabilities.MergeManualTags as presence-only (Tags["maya"] = ""), and
// scheduler/matcher.go's workerAttributeValue hands that empty string to the
// requirement's anyOf list, so "maya" alone never satisfies
// `attr.worker.tag.maya anyOf ["true"]`. Task 10 found that the slow way: the
// job sat `pending` for the full 90s timeout with no worker ever matching.
func workerTagEnv(t *testing.T, entry presettest.Entry) []string {
	t.Helper()
	const prefix = "attr.worker.tag."
	seen := map[string]bool{}
	var tags []string
	for _, attr := range hostAttributeRequirements(t, entry) {
		name, ok := strings.CutPrefix(attr.Name, prefix)
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		tags = append(tags, name+"="+requiredAttributeValue(attr))
	}
	if len(tags) == 0 {
		return nil
	}
	sort.Strings(tags)
	return []string{"SQI_WORKER_CAPABILITY_TAGS=" + strings.Join(tags, ",")}
}

// requiredAttributeValue returns a value that satisfies attr.
func requiredAttributeValue(attr openjd.AttributeRequirement) string {
	if len(attr.AnyOf) > 0 {
		return attr.AnyOf[0]
	}
	if len(attr.AllOf) > 0 {
		return attr.AllOf[0]
	}
	return "true"
}

// unsatisfiableOSFamily returns the attr.worker.os.family requirement this host
// cannot meet, or "" when every such requirement is satisfiable here.
//
// No registry entry reaches it today -- ffmpeg-segment-transcode-powershell,
// which requires "windows", carries no tier3 block at all for that very reason.
// It stays because the requirement is a property of the HOST that no test
// environment can fake: the worker reports runtime.GOOS at
// registration (capabilities.Detect) and the scheduler translates it
// (internal/scheduler/matcher.go osFamily) — unlike a capability tag, which
// SQI_WORKER_CAPABILITY_TAGS can simply assert. Without this gate such a preset's
// job never leaves `pending` and the case fails on the 90s timeout with nothing
// naming the cause.
//
// Both anyOf and allOf are inspected. os.family is single-valued per worker, so
// an allOf listing anything other than exactly this host's family is
// unsatisfiable here for the same reason; checking only anyOf would leave a
// future allOf preset hanging for the whole 90s timeout instead of skipping with
// a reason.
func unsatisfiableOSFamily(t *testing.T, entry presettest.Entry) string {
	t.Helper()
	host := hostOSFamily()
	for _, attr := range hostAttributeRequirements(t, entry) {
		if attr.Name != "attr.worker.os.family" {
			continue
		}
		if len(attr.AnyOf) > 0 && !slices.Contains(attr.AnyOf, host) {
			return strings.Join(attr.AnyOf, "|")
		}
		for _, want := range attr.AllOf {
			if want != host {
				return strings.Join(attr.AllOf, "&")
			}
		}
	}
	return ""
}

// hostOSFamily mirrors internal/scheduler's unexported osFamily translation for
// the platforms sqi builds for.
//
// Duplicated deliberately and kept to one line: package integration cannot
// reach the scheduler's copy, and the only consequence of the two drifting apart
// is a skip that should have run — never a pass that should have failed.
func hostOSFamily() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

// assertInvocationCounts compares what the stub actually recorded against the
// case fixture's expect_invocations.
//
// It prints both maps on failure: "ffmpeg ran 2 times" is not actionable
// without knowing the fixture said 3, and a segmented preset that silently
// transcodes one slice fewer is exactly the bug this tier exists to catch.
func assertInvocationCounts(t *testing.T, c presettest.Case, recs []stubrecord.Record) {
	t.Helper()
	if len(c.ExpectInvocations) == 0 {
		return
	}
	got := map[string]int{}
	for _, rec := range recs {
		got[rec.Command]++
	}
	for name, want := range c.ExpectInvocations {
		if got[name] != want {
			t.Errorf("stub %q invoked %d times, fixture expects %d\n  observed: %v\n  expected: %v",
				name, got[name], want, got, c.ExpectInvocations)
		}
	}
	for name, count := range got {
		if _, claimed := c.ExpectInvocations[name]; !claimed {
			t.Errorf("stub %q was invoked %d times but the fixture claims nothing about it", name, count)
		}
	}
}

// assertObservedMatchesComputed is the cross-check: every argv the stub
// recorded must equal an argv the computed snapshot predicted.
//
// Order is not asserted — tasks are leased concurrently, so the run order is
// genuinely nondeterministic. Set equality is the real claim.
//
// Arguments that are absolute paths to one of this preset's MATERIALIZED
// EMBEDDED FILES are folded to a placeholder first. They cannot be compared
// verbatim: such a path contains the session directory, which is a different
// temp directory in presettest's computed pipeline than in the real worker's
// session root. Comparing them raw would fail for every preset that delivers a
// script or a concat list — python, houdini-rop-render, and both runnable
// segment-transcode variants — i.e. exactly the presets whose argv most needs
// an independent witness.
func assertObservedMatchesComputed(t *testing.T, snap presettest.Snapshot, recs []stubrecord.Record) {
	t.Helper()
	files := materializedFileNames(snap)
	computed := map[string]bool{}
	for _, step := range snap.Steps {
		for _, task := range step.Tasks {
			computed[argvKey(filepath.Base(task.Command), foldFilePaths(task.Args, files))] = true
		}
	}
	for _, rec := range recs {
		key := argvKey(rec.Command, foldFilePaths(rec.Args, files))
		if !computed[key] {
			t.Errorf("observed argv has no computed counterpart:\n  observed: %s\n  computed set: %v",
				key, keysOf(computed))
		}
	}
}

// materializedFileNames returns the on-disk basenames of every embedded file in
// snap.
func materializedFileNames(snap presettest.Snapshot) map[string]bool {
	out := map[string]bool{}
	for _, step := range snap.Steps {
		for _, task := range step.Tasks {
			for _, f := range task.Files {
				name := f.Filename
				if name == "" {
					name = f.Name
				}
				out[name] = true
			}
		}
	}
	return out
}

// foldFilePaths replaces every argument that is an absolute path to one of files
// with a stable placeholder. See [assertObservedMatchesComputed].
func foldFilePaths(args []string, files map[string]bool) []string {
	out := make([]string, len(args))
	for i, a := range args {
		base := filepath.Base(a)
		if filepath.IsAbs(a) && files[base] {
			out[i] = "<FILE:" + base + ">"
			continue
		}
		out[i] = a
	}
	return out
}

func argvKey(command string, args []string) string {
	return command + " " + strings.Join(args, "\x1f")
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
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
