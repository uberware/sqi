// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// aftereffects_preset_test.go -- the After Effects presets' wrapper, run for
// real.
//
// aerender is reported to exit 0 when a render fails, so every After Effects
// preset runs it through a wrapper script that also fails the task when
// aerender's output contains "aerender ERROR". Tier 3 cannot check that, because
// its stub exits 0 with nothing on stdout, which a wrapper that trusted the exit
// status would also pass. These tests drive the shipped templates through a
// real worker and a real shell, with aerender replaced by test/stubproc
// printing chosen text (SQI_STUB_STDOUT) and exiting with a chosen status
// (SQI_STUB_EXIT), set per job by a sidecar beside the job's scene, and assert
// that a failed render is never reported as succeeded.
//
// Not tagged `integration`, for the reason ffmpeg_presets_test.go gives: CI's
// ordinary `go test` runs only untagged files. Every helper used here is
// untagged too.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
)

// aeWrapperVariant is one After Effects preset whose wrapper is exercised.
type aeWrapperVariant struct {
	// preset is the name under presets/sqi.
	preset string
	// macOS marks a macOS preset. It runs on every host but Windows, submitted
	// with its attr.worker.os.family requirement removed
	// (presettest.StripOSFamilyGate) so a host of another family can lease
	// the task; a Windows preset runs on Windows alone, as shipped.
	macOS bool
}

// runsHere reports whether v runs on this host.
func (v aeWrapperVariant) runsHere() bool {
	return (runtime.GOOS == "windows") != v.macOS
}

// aeWrapperVariants lists every After Effects preset whose wrapper the table
// below drives.
//
// The macOS variants run on every host but Windows, with their os.family gate
// stripped. Their Tier 3 cases run only on a Mac (required_on [darwin]), and no
// CI job runs the Go tests on macOS, so these rows are what exercises the sh
// wrapper's exit status in CI. Stripping the gate changes nothing the wrapper
// sees: the same /bin/sh runs the same embedded script with the same arguments
// (presettest's TestStripOSFamilyGate_KeepsEverythingButTheGate). A Linux run
// uses that host's /bin/sh (dash on Ubuntu) rather than macOS's (bash in POSIX
// mode), so the script keeps to POSIX sh. They do not run on Windows, which
// has no /bin/sh. On a Mac the strip is unnecessary but harmless.
var aeWrapperVariants = []aeWrapperVariant{
	{preset: "after-effects-render-windows"},
	{preset: "after-effects-queue-render-windows"},
	{preset: "after-effects-render-macos", macOS: true},
	{preset: "after-effects-queue-render-macos", macOS: true},
}

// aeWrapperOutcome is one way aerender can end and the job outcome it must
// produce.
type aeWrapperOutcome struct {
	name string
	// env configures the aerender stub for this outcome's jobs, as the lines
	// of their scenes' .stubenv sidecars; unused with noAerender.
	env []string
	// noAerender leaves aerender off PATH entirely.
	noAerender bool
	// want is the job status.
	want string
	// wantCode, when non-zero, is the exit code the task's failure_reason
	// must report.
	wantCode int
	// logLine, when non-empty, must reach the task log unchanged.
	logLine string
}

var aeWrapperOutcomes = []aeWrapperOutcome{
	{
		// The case the wrapper exists for: aerender logs an error and still
		// exits 0.
		name:     "logged-error-exit-0",
		env:      []string{"SQI_STUB_STDOUT=aerender ERROR: Unable to render"},
		want:     "failed",
		wantCode: 1,
		logLine:  "aerender ERROR: Unable to render",
	},
	{
		// The marker matches case-insensitively in both wrappers
		// (PowerShell's -like, grep -i), so a differently cased spelling of
		// it is still a failure.
		name:     "logged-error-other-case",
		env:      []string{"SQI_STUB_STDOUT=AERENDER error: Unable to render"},
		want:     "failed",
		wantCode: 1,
		logLine:  "AERENDER error: Unable to render",
	},
	{
		// A non-zero exit is forwarded as is, not flattened to 1.
		name:     "non-zero-exit",
		env:      []string{"SQI_STUB_EXIT=2"},
		want:     "failed",
		wantCode: 2,
	},
	{
		// Ordinary progress output is not mistaken for an error.
		name:    "progress-exit-0",
		env:     []string{"SQI_STUB_STDOUT=PROGRESS: 1 (1): 0 Seconds"},
		want:    "completed",
		logLine: "PROGRESS: 1 (1): 0 Seconds",
	},
	{
		// A shell that cannot find aerender must not fall through to the
		// wrapper's final exit 0. Windows PowerShell reports a missing
		// command as an error and carries on, which without the wrapper's
		// trap would end the script with exit 0; sh reports 127, which the
		// sh wrapper must forward through its pipeline. No exit code is
		// pinned: the shells report a missing command differently.
		name:       "aerender-not-on-path",
		noAerender: true,
		want:       "failed",
	},
}

// TestPresetAfterEffects_WrapperFailsOnLoggedError runs each After Effects
// preset's shipped wrapper against every outcome in aeWrapperOutcomes.
//
// Starting a server and a worker is most of the cost, so every outcome the stub
// can play shares one: each job's scene carries a sidecar setting the stub's
// behavior for that job alone (SQI_STUB_ENV_FROM_ARG on -project). Leaving
// aerender off PATH is a property of the worker, not of a job, so that outcome
// gets a worker of its own. Every job is submitted up front and then polled in
// turn, under <outcome>/<preset>.
func TestPresetAfterEffects_WrapperFailsOnLoggedError(t *testing.T) {
	var variants []aeWrapperVariant
	for _, v := range aeWrapperVariants {
		if v.runsHere() {
			variants = append(variants, v)
		}
	}
	if len(variants) == 0 {
		t.Skipf("no After Effects wrapper variant runs on GOOS=%s", runtime.GOOS)
	}
	tmpls := aeWrapperTemplates(t, variants)

	var stubbed, unstubbed []aeWrapperOutcome
	for _, oc := range aeWrapperOutcomes {
		if oc.noAerender {
			unstubbed = append(unstubbed, oc)
		} else {
			stubbed = append(stubbed, oc)
		}
	}

	runAEWrapperFarm(t, variants, tmpls, stubbed, []string{"aerender"}, "SQI_STUB_ENV_FROM_ARG=-project")

	if path, err := exec.LookPath("aerender"); err == nil {
		for _, oc := range unstubbed {
			t.Run(oc.name, func(t *testing.T) {
				t.Skipf("a real aerender is on this host's PATH (%s); the row needs it absent", path)
			})
		}
		return
	}
	// A stub under an unrelated name, so the recording path exists and a
	// stray aerender invocation would still show up as a record.
	runAEWrapperFarm(t, variants, tmpls, unstubbed, []string{"sqi-not-aerender"})
}

// runAEWrapperFarm starts one server and one worker with stubs under names on
// PATH, submits a job per outcome per variant, and checks each job.
func runAEWrapperFarm(
	t *testing.T, variants []aeWrapperVariant, tmpls []string, outcomes []aeWrapperOutcome,
	names []string, extraEnv ...string,
) {
	t.Helper()
	if len(outcomes) == 0 {
		return
	}
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	// Every variant carries the same tag requirement, so the first one's tag
	// environment serves them all.
	tags := workerTagEnv(t, presettest.Entry{Source: presettest.SourcePresets, Name: variants[0].preset})
	env := newTier3Env(t, names, append(tags, extraEnv...)...)
	startRealWorkerAnyOS(t, ts, farmID, queueID, env.env...)

	// A scene per job, so each job's aerender invocation is told apart in the
	// shared recording, and so each can carry its own stub sidecar.
	sceneDir := t.TempDir()
	scenes := make([][]string, len(outcomes))
	jobIDs := make([][]string, len(outcomes))
	for o, oc := range outcomes {
		scenes[o] = make([]string, len(variants))
		jobIDs[o] = make([]string, len(variants))
		for i, v := range variants {
			scene := filepath.Join(sceneDir, oc.name+"-"+v.preset+".aep")
			if len(oc.env) > 0 {
				sidecar := strings.Join(oc.env, "\n") + "\n"
				if err := os.WriteFile(scene+".stubenv", []byte(sidecar), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			scenes[o][i] = scene
			jobIDs[o][i] = submitPresetJob(t, ts, farmID, queueID, tmpls[i], aeWrapperParams(v.preset, scene))
		}
	}

	for o, oc := range outcomes {
		t.Run(oc.name, func(t *testing.T) {
			for i, v := range variants {
				t.Run(v.preset, func(t *testing.T) {
					checkAEWrapperJob(t, ts, env, oc, jobIDs[o][i], scenes[o][i])
				})
			}
		})
	}
}

// aeWrapperTemplates returns each variant's shipped template, with a macOS
// variant's os.family gate stripped.
func aeWrapperTemplates(t *testing.T, variants []aeWrapperVariant) []string {
	t.Helper()
	tmpls := make([]string, len(variants))
	for i, v := range variants {
		tmpl, err := presettest.PresetTemplate(presettest.SourcePresets, v.preset)
		if err != nil {
			t.Fatalf("PresetTemplate: %v", err)
		}
		if v.macOS {
			if tmpl, err = presettest.StripOSFamilyGate(tmpl); err != nil {
				t.Fatalf("StripOSFamilyGate: %v", err)
			}
		}
		tmpls[i] = tmpl
	}
	return tmpls
}

// checkAEWrapperJob asserts one job's outcome, its failure_reason, its log
// and that aerender really ran for it (or, with no aerender, did not).
func checkAEWrapperJob(t *testing.T, ts *testServer, env tier3Env, oc aeWrapperOutcome, jobID, scene string) {
	t.Helper()
	status := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, presetJobTimeout)
	reason := firstTaskFailureReason(t, ts, jobID)
	if status != oc.want {
		t.Fatalf("job status = %q, want %q (first task failure_reason: %q)\n"+
			"the aerender wrapper is missing or ineffective", status, oc.want, reason)
	}
	if status == "failed" {
		if reason == "" {
			t.Error("failed task carries no failure_reason")
		}
		if want := fmt.Sprintf("exited with code %d", oc.wantCode); oc.wantCode != 0 && !strings.Contains(reason, want) {
			t.Errorf("failure_reason = %q, want it to contain %q", reason, want)
		}
	}
	if oc.logLine != "" {
		pollTaskLogs(t, ts, firstTaskID(t, ts, jobID), oc.logLine, presetJobTimeout)
	}

	ran := false
	for _, rec := range env.records(t) {
		if rec.Command == "aerender" && argFollows(rec.Args, "-project", scene) {
			ran = true
		}
	}
	switch {
	case oc.noAerender && ran:
		t.Errorf("aerender recorded for %s, but it was not on PATH", scene)
	case !oc.noAerender && !ran:
		t.Errorf("no aerender invocation recorded with -project %s; the job's outcome did not come from aerender", scene)
	}
}

// aeWrapperParams returns one task's worth of parameters for preset: the
// per-target presets render frames 1-10, one task at the default
// FramesPerTask.
func aeWrapperParams(preset, scene string) map[string]string {
	params := map[string]string{"SceneFile": scene}
	if !strings.HasPrefix(preset, "after-effects-queue-render-") {
		params["Frames"] = "1-10"
		params["RenderQueueItem"] = "1"
	}
	return params
}

// argFollows reports whether args holds flag immediately followed by value.
func argFollows(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}
