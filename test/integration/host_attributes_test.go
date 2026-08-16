// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// host_attributes_test.go — proves the two reserved OpenJD host attributes whose
// vocabulary differs from Go's actually schedule work, end to end, through a
// real sqi-worker.
//
// Both were broken in the same way and neither was caught by a unit test,
// because the unit tests set store.Worker fields directly and so could not see
// that the value never reached the store at all:
//
//   - attr.worker.os.family compared a template's "macos" against a worker's
//     runtime.GOOS "darwin", so no Mac could ever be matched.
//   - attr.worker.cpu.arch had no case in the matcher AND no field on the
//     worker, so it resolved to "" and matched nothing on any platform.
//
// A regression in either shows up here as a job that never leaves ready and
// times out, which is why these assert on completion rather than on a value.
//
// NOT tagged `integration`, for the reason worker_binary_test.go and
// ffmpeg_presets_test.go are not: the CI `test` job runs `make test-cover`,
// which passes no `-tags`, so a file behind that tag compiles and lints but
// never executes there.

import (
	"fmt"
	"net/http"
	"runtime"
	"testing"
	"time"
)

// jobStatusNow reads a job's status once. [pollJobStatus] cannot serve the
// negative case below: it fails the test when its target never arrives, and
// "never arrives" is exactly what that case asserts.
func jobStatusNow(t *testing.T, ts *testServer, jobID string) string {
	t.Helper()
	var resp jobResp
	mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/jobs/"+jobID), nil, "", http.StatusOK, &resp)
	return resp.Status
}

// specHostAttributes returns the attr.worker.os.family and attr.worker.cpu.arch
// values a template must use to select THIS host — the specification's tokens,
// which differ from runtime.GOOS/GOARCH for exactly two values.
//
// Written as an explicit mapping rather than by calling the scheduler's own
// osFamily/cpuArch: reusing the code under test would make this test agree with
// a wrong implementation. If sqi's translation regresses, these literals stay
// right and the job stops scheduling.
func specHostAttributes(t *testing.T) (osFamily, cpuArch string) {
	t.Helper()

	switch runtime.GOOS {
	case "darwin":
		osFamily = "macos"
	case "linux", "windows":
		osFamily = runtime.GOOS
	default:
		t.Skipf("no OpenJD os.family token for GOOS %q", runtime.GOOS)
	}

	switch runtime.GOARCH {
	case "amd64":
		cpuArch = "x86_64"
	case "arm64":
		cpuArch = "arm64"
	default:
		t.Skipf("no OpenJD cpu.arch token for GOARCH %q", runtime.GOARCH)
	}
	return osFamily, cpuArch
}

// hostAttributeJobYAML builds a one-step job gated on a single host attribute.
func hostAttributeJobYAML(attribute, value string) string {
	return fmt.Sprintf(`specificationVersion: "jobtemplate-2023-09"
name: Host Attribute Integration Test

steps:
  - name: Run
    hostRequirements:
      attributes:
        - name: %s
          anyOf: [%q]
    script:
      actions:
        onRun:
          command: echo
          args:
            - "matched %s=%s"
`, attribute, value, attribute, value)
}

// runHostAttributeJob submits a job gated on one attribute and requires it to
// complete. A requirement this host cannot satisfy leaves the task ready
// forever, so the failure mode is the timeout rather than a status of "failed".
func runHostAttributeJob(t *testing.T, attribute, value string) {
	t.Helper()

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorker(t, ts, farmID, queueID)

	jobID := submitJobCustomYAML(t, ts, farmID, queueID,
		hostAttributeJobYAML(attribute, value))

	got := pollJobStatus(t, ts, jobID,
		[]string{"completed", "failed", "canceled"}, 90*time.Second)
	if got != "completed" {
		t.Fatalf("job gated on %s=%q ended %q, want completed — "+
			"a job stuck in ready means this host's reported value never "+
			"matched the requirement", attribute, value, got)
	}
}

// TestHostAttribute_OSFamilyMatchesThisHost is the end-to-end guard for the
// darwin/macos translation. On a Mac it is the whole point of the test; on
// Linux and Windows it is a cheap regression check that the pass-through cases
// still work.
func TestHostAttribute_OSFamilyMatchesThisHost(t *testing.T) {
	osFamily, _ := specHostAttributes(t)
	runHostAttributeJob(t, "attr.worker.os.family", osFamily)
}

// TestHostAttribute_CPUArchMatchesThisHost is the end-to-end guard for
// attr.worker.cpu.arch. It covers the whole chain the unit tests cannot: the
// worker probing runtime.GOARCH, advertising it in its registration payload,
// the server persisting it, and the matcher translating it back into the
// specification's token. Before that chain existed this job could not schedule
// on any host at all.
func TestHostAttribute_CPUArchMatchesThisHost(t *testing.T) {
	_, cpuArch := specHostAttributes(t)
	runHostAttributeJob(t, "attr.worker.cpu.arch", cpuArch)
}

// TestHostAttribute_CPUArchMismatchDoesNotSchedule is the negative half. Without
// it the test above would pass against a matcher that accepted everything —
// including the "unreported architecture matches anything" mistake, which would
// send x86_64 work to an arm64 host.
//
// It asserts the job is still waiting rather than that it failed: an unmatched
// requirement is not an error, it is a task no worker will take.
func TestHostAttribute_CPUArchMismatchDoesNotSchedule(t *testing.T) {
	_, cpuArch := specHostAttributes(t)
	other := "x86_64"
	if cpuArch == "x86_64" {
		other = "arm64"
	}

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorker(t, ts, farmID, queueID)

	jobID := submitJobCustomYAML(t, ts, farmID, queueID,
		hostAttributeJobYAML("attr.worker.cpu.arch", other))

	// Long enough that a worker which was going to take the job already would
	// have — the positive cases above complete in a second or two.
	time.Sleep(5 * time.Second)

	status := jobStatusNow(t, ts, jobID)
	if status == "completed" {
		t.Fatalf("a job requiring %s ran on a %s host: the arch requirement is "+
			"matching values it should not", other, cpuArch)
	}
}
