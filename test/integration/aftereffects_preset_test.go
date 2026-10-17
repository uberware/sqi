// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// aftereffects_preset_test.go -- the After Effects presets' wrapper, run for
// real.
//
// aerender is reported to exit 0 when a render fails (design §13.1), so every
// After Effects preset runs it through a wrapper script that also fails the
// task when aerender's output contains "aerender ERROR". Tier 3 cannot see
// that: its stub always exits 0 with nothing on stdout, so a wrapper that
// trusted the exit status would pass it. These tests drive the shipped
// templates through a real worker and a real shell, with aerender replaced by
// test/stubproc printing chosen text (SQI_STUB_STDOUT) and exiting with a
// chosen status (SQI_STUB_EXIT), and assert the job's outcome. This is the pin
// for the design's first anti-goal: a failed render reported as succeeded.
//
// NOT tagged `integration`, for the reason ffmpeg_presets_test.go gives: an
// untagged file is what CI's ordinary `go test` runs, and every helper used
// here is untagged too.

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/presettest"
)

// aeWrapperVariant is one After Effects preset whose wrapper is exercised.
type aeWrapperVariant struct {
	// preset is the name under presets/sqi.
	preset string
	// goos is the runtime.GOOS the row runs on; a leading "!" negates it.
	goos string
	// stripOSGate submits the template with its attr.worker.os.family
	// requirement removed (stripOSFamilyGate), so a host of another family
	// can lease the task. Off when the host satisfies the gate as shipped.
	stripOSGate bool
}

// aeWrapperVariants lists every After Effects preset whose wrapper the table
// below drives.
//
// The macOS variants run on every host but Windows, with their os.family gate
// stripped. Their Tier-3 cases can run only on a Mac (required_on [darwin]),
// and CI has no macOS runner, so without these rows the POSIX sh wrapper's
// exit-status contract would be exercised nowhere in CI. Stripping the gate
// changes nothing the wrapper sees: the same /bin/sh runs the same embedded
// script with the same arguments (TestAfterEffectsPreset_StripOSFamilyGate).
// What a Linux run cannot show is macOS's own /bin/sh, which is bash in POSIX
// mode rather than Ubuntu's dash; the script keeps to POSIX sh for that reason.
// They do not run on Windows, which has no /bin/sh. On a Mac the strip is
// unnecessary but harmless.
var aeWrapperVariants = []aeWrapperVariant{
	{preset: "after-effects-render-windows", goos: "windows"},
	{preset: "after-effects-queue-render-windows", goos: "windows"},
	{preset: "after-effects-render-macos", goos: "!windows", stripOSGate: true},
	{preset: "after-effects-queue-render-macos", goos: "!windows", stripOSGate: true},
}

// aeWrapperOutcome is one way aerender can end and the job outcome it must
// produce.
type aeWrapperOutcome struct {
	name string
	// env configures the aerender stub; unused with noAerender.
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

// TestAfterEffectsPreset_WrapperFailsOnLoggedError runs each After Effects
// preset's shipped wrapper against every outcome in aeWrapperOutcomes.
//
// One server and one worker serve each outcome, because what the stub does is
// set by the worker's environment: every variant runnable on this host is
// submitted to that worker up front and then polled in turn.
func TestAfterEffectsPreset_WrapperFailsOnLoggedError(t *testing.T) {
	var variants []aeWrapperVariant
	for _, v := range aeWrapperVariants {
		if matchesGOOS(v.goos) {
			variants = append(variants, v)
		}
	}
	if len(variants) == 0 {
		t.Skipf("no After Effects wrapper variant runs on GOOS=%s", runtime.GOOS)
	}

	for _, oc := range aeWrapperOutcomes {
		t.Run(oc.name, func(t *testing.T) {
			runAEWrapperOutcome(t, variants, oc)
		})
	}
}

// runAEWrapperOutcome starts one server and worker configured for oc, submits
// every variant, and checks each job.
func runAEWrapperOutcome(t *testing.T, variants []aeWrapperVariant, oc aeWrapperOutcome) {
	t.Helper()
	if oc.noAerender {
		if path, err := exec.LookPath("aerender"); err == nil {
			t.Skipf("a real aerender is on this host's PATH (%s); the row needs it absent", path)
		}
	}

	tmpls := make([]string, len(variants))
	for i, v := range variants {
		tmpl, err := presettest.PresetTemplate(presettest.SourcePresets, v.preset)
		if err != nil {
			t.Fatalf("PresetTemplate: %v", err)
		}
		if v.stripOSGate {
			tmpl = stripOSFamilyGate(t, tmpl)
		}
		tmpls[i] = tmpl
	}

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	// Every variant carries the same tag requirement, so the first one's
	// tag environment serves them all.
	tags := workerTagEnv(t, presettest.Entry{Source: presettest.SourcePresets, Name: variants[0].preset})
	var env tier3Env
	if oc.noAerender {
		// A stub under an unrelated name, so the recording path exists and
		// a stray aerender invocation would still show up as a record.
		env = newTier3Env(t, []string{"sqi-not-aerender"}, tags...)
	} else {
		env = newTier3Env(t, []string{"aerender"}, append(tags, oc.env...)...)
	}
	startRealWorkerAnyOS(t, ts, farmID, queueID, env.env...)

	// A scene per variant, so each job's aerender invocation is told apart
	// in the shared recording.
	scenes := make([]string, len(variants))
	jobIDs := make([]string, len(variants))
	sceneDir := t.TempDir()
	for i, v := range variants {
		scenes[i] = filepath.Join(sceneDir, v.preset+".aep")
		jobIDs[i] = submitPresetJob(t, ts, farmID, queueID, tmpls[i], aeWrapperParams(v.preset, scenes[i]))
	}

	for i, v := range variants {
		t.Run(v.preset, func(t *testing.T) {
			checkAEWrapperJob(t, ts, env, oc, jobIDs[i], scenes[i])
		})
	}
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

// matchesGOOS reports whether want names this host's runtime.GOOS. A leading
// "!" negates it, so "!windows" matches every other platform.
func matchesGOOS(want string) bool {
	if rest, ok := strings.CutPrefix(want, "!"); ok {
		return runtime.GOOS != rest
	}
	return runtime.GOOS == want
}

// stripOSFamilyGate returns tmpl with every attr.worker.os.family host
// requirement removed, so a worker of any OS family can lease its steps.
// Everything else, the embedded files included, is kept. It fails the test
// when the template has no such requirement, since a strip that removed
// nothing would leave the test silently exercising an unchanged template.
func stripOSFamilyGate(t *testing.T, tmpl string) string {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(tmpl), &doc); err != nil {
		t.Fatalf("stripOSFamilyGate: parse template: %v", err)
	}
	if len(doc.Content) == 0 {
		t.Fatal("stripOSFamilyGate: empty template")
	}
	removed := 0
	steps := yamlMapValue(doc.Content[0], "steps")
	if steps != nil {
		for _, step := range steps.Content {
			removed += stripStepOSFamily(step)
		}
	}
	if removed == 0 {
		t.Fatal("stripOSFamilyGate: the template has no attr.worker.os.family requirement")
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatalf("stripOSFamilyGate: re-serialize: %v", err)
	}
	return string(out)
}

// stripStepOSFamily removes the os.family attribute requirements from one step
// node and returns how many it removed. An attribute list left empty is
// dropped, and so is a hostRequirements left with nothing in it, because
// OpenJD rejects both empty.
func stripStepOSFamily(step *yaml.Node) int {
	hr := yamlMapValue(step, "hostRequirements")
	attrs := yamlMapValue(hr, "attributes")
	if attrs == nil {
		return 0
	}
	kept := attrs.Content[:0]
	removed := 0
	for _, attr := range attrs.Content {
		if name := yamlMapValue(attr, "name"); name != nil && name.Value == "attr.worker.os.family" {
			removed++
			continue
		}
		kept = append(kept, attr)
	}
	attrs.Content = kept
	if len(kept) == 0 {
		yamlMapDelete(hr, "attributes")
	}
	if len(hr.Content) == 0 {
		yamlMapDelete(step, "hostRequirements")
	}
	return removed
}

// yamlMapValue returns the value node for key in mapping node m, or nil.
func yamlMapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// yamlMapDelete removes key and its value from mapping node m.
func yamlMapDelete(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// TestAfterEffectsPreset_StripOSFamilyGate checks the helper on every variant
// on every host, not only where a row strips: the os.family requirement goes,
// the capability tag stays, and each embedded file survives the YAML round
// trip byte for byte (the PowerShell wrapper's leading BOM and the sh
// wrapper's line breaks included).
func TestAfterEffectsPreset_StripOSFamilyGate(t *testing.T) {
	for _, v := range aeWrapperVariants {
		t.Run(v.preset, func(t *testing.T) {
			raw, err := presettest.PresetTemplate(presettest.SourcePresets, v.preset)
			if err != nil {
				t.Fatalf("PresetTemplate: %v", err)
			}
			before, err := openjd.Parse([]byte(raw), openjd.FormatYAML)
			if err != nil {
				t.Fatalf("parse shipped template: %v", err)
			}
			after, err := openjd.Parse([]byte(stripOSFamilyGate(t, raw)), openjd.FormatYAML)
			if err != nil {
				t.Fatalf("parse stripped template: %v", err)
			}
			if len(after.Steps) != len(before.Steps) {
				t.Fatalf("steps = %d after the strip, want %d", len(after.Steps), len(before.Steps))
			}
			for i, step := range after.Steps {
				tagged := false
				if step.HostRequirements != nil {
					for _, attr := range step.HostRequirements.Attributes {
						if attr.Name == "attr.worker.os.family" {
							t.Errorf("step %q still requires attr.worker.os.family", step.Name)
						}
						if attr.Name == "attr.worker.tag.aftereffects" {
							tagged = true
						}
					}
				}
				if !tagged {
					t.Errorf("step %q lost its attr.worker.tag.aftereffects requirement", step.Name)
				}
				assertEmbeddedFilesEqual(t, before.Steps[i], step)
			}
		})
	}
}

// assertEmbeddedFilesEqual fails when a step's embedded files differ.
func assertEmbeddedFilesEqual(t *testing.T, want, got openjd.StepTemplate) {
	t.Helper()
	if want.Script == nil || got.Script == nil {
		if (want.Script == nil) != (got.Script == nil) {
			t.Errorf("step %q: script presence changed", want.Name)
		}
		return
	}
	if len(got.Script.EmbeddedFiles) != len(want.Script.EmbeddedFiles) {
		t.Fatalf("step %q: %d embedded files, want %d",
			want.Name, len(got.Script.EmbeddedFiles), len(want.Script.EmbeddedFiles))
	}
	for j, f := range want.Script.EmbeddedFiles {
		if g := got.Script.EmbeddedFiles[j]; g.Name != f.Name || g.Filename != f.Filename || g.Data != f.Data {
			t.Errorf("step %q: embedded file %q changed in the round trip\n got: %q\nwant: %q",
				want.Name, f.Name, g.Data, f.Data)
		}
	}
}
