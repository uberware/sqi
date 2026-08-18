// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// ffmpeg_presets_test.go — executes the shipped ffmpeg reference presets
// (presets/sqi/ffmpeg-*.yaml) through a real sqi-server and a real sqi-worker,
// then asserts on the VIDEO THEY PRODUCE.
//
// Why this file exists. Until it landed, every ffmpeg-preset test checked
// shape: internal/product/sqipresets_test.go validates each preset against the
// real schema, and internal/product/exprpresetcost_test.go pins the portable
// variant's submission cost. Neither runs ffmpeg, so a preset could ship with a
// command that parses perfectly and transcodes nothing. That is not
// hypothetical — two commits on the branch that introduced these presets fixed
// exactly that class of bug:
//
//   - 667b921 — the PowerShell join wrote its concat list as BOM'd UTF-8, which
//     the concat demuxer rejects on the first "file" line.
//   - d7d8fcc — the same join was an embedded file with no .ps1 filename, which
//     `powershell -File` refuses to execute.
//
// Both were caught by reading, not by testing; both would have shipped green.
// The assertions below are chosen so that each would have failed here.
//
// NOT tagged `integration`. That is deliberate and load-bearing: the CI `test`
// job runs `make test-cover`, which passes no `-tags`, so a file behind that
// tag compiles and lints but NEVER EXECUTES in CI. worker_binary_test.go — the
// other real-worker test — is untagged for the same reason, and this file
// shares its cached binary, so the marginal cost is one ffmpeg run per case.
//
// Skips when ffmpeg or ffprobe is absent, which means A LOCAL PASS PROVES
// NOTHING ON ITS OWN — look for the "--- PASS: TestFFmpegPreset" lines.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/product"
)

// ── Fixture geometry ──────────────────────────────────────────────────────────

// The synthetic source is small and short so a case costs well under a second
// of encoding, but long enough to split into several slices: a segmented run
// that silently joined only its first slice must come out visibly short.
const (
	ffmpegSourceSeconds  = 6
	ffmpegSegmentSeconds = 2 // → ceil(6/2) = 3 slices
	ffmpegWantSlices     = 3

	// durationTolerance absorbs container rounding — concatenating N slices
	// lands a few ms long — while staying far below the ffmpegSegmentSeconds
	// error that dropping a single slice would produce.
	durationTolerance = 0.5

	// jobTimeout covers worker registration plus the encode itself.
	jobTimeout = 120 * time.Second
)

// ── ffmpeg helpers ────────────────────────────────────────────────────────────

// requireFFmpeg skips unless both binaries the presets and these assertions
// depend on are present.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("skipping ffmpeg preset test: %s not on PATH: %v", bin, err)
		}
	}
}

// runFFmpeg executes an ffmpeg/ffprobe command line and returns its combined
// output, failing the test on a non-zero exit.
func runFFmpeg(t *testing.T, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// makeSourceVideo writes a synthetic H.264+AAC source of the default length
// into dir and returns its path.
func makeSourceVideo(t *testing.T, dir string) string {
	t.Helper()
	return makeSourceVideoOfLength(t, dir, ffmpegSourceSeconds)
}

// makeSourceVideoOfLength is [makeSourceVideo] with an explicit duration, for
// the cases below that choose a source length to make a specific arithmetic
// mistake visible. Generating it with ffmpeg keeps binary fixtures out of the
// repository.
func makeSourceVideoOfLength(t *testing.T, dir string, seconds int) string {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("source%ds.mp4", seconds))
	runFFmpeg(
		t, "ffmpeg",
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i",
		fmt.Sprintf("testsrc=size=160x120:rate=10:duration=%d", seconds),
		"-f", "lavfi", "-i",
		fmt.Sprintf("sine=frequency=440:duration=%d", seconds),
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest",
		path,
	)
	return path
}

// probeDuration returns a media file's duration in seconds, failing the test if
// the file is missing or unreadable — which is itself the assertion that the
// preset produced an output at the path it promised.
func probeDuration(t *testing.T, path string) float64 {
	t.Helper()
	out := runFFmpeg(t, "ffprobe",
		"-v", "error", "-show_entries", "format=duration",
		"-of", "csv=p=0", path)
	secs, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
	if err != nil {
		t.Fatalf("probeDuration(%s): parse %q: %v", path, out, err)
	}
	return secs
}

// probeCodecs returns the codec name of every stream, in file order.
func probeCodecs(t *testing.T, path string) []string {
	t.Helper()
	out := runFFmpeg(t, "ffprobe",
		"-v", "error", "-show_entries", "stream=codec_name",
		"-of", "csv=p=0", path)
	var codecs []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			codecs = append(codecs, line)
		}
	}
	return codecs
}

// assertDuration fails when got is further than durationTolerance from want.
func assertDuration(t *testing.T, path string, want float64) {
	t.Helper()
	got := probeDuration(t, path)
	if math.Abs(got-want) > durationTolerance {
		t.Errorf("%s: duration = %.3fs, want %.3fs (±%.1f)\n"+
			"a short result means slices were dropped from the join; "+
			"a long one means the source was over-read",
			filepath.Base(path), got, want, durationTolerance)
	}
}

// ── Preset submission ─────────────────────────────────────────────────────────

// presetPath resolves a shipped preset by name, relative to this source file so
// the test does not depend on the working directory.
func presetPath(t *testing.T, name string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) returned no file path")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..",
		"presets", "sqi", name+".yaml")
}

// submitPreset parses a shipped preset exactly as the server does, then submits
// its template as a job with params supplied as param.<Name> query values.
//
// Parsing the real file (rather than an inline copy) is the point: a preset
// edited in the repository is the thing under test here.
func submitPreset(t *testing.T, ts *testServer, farmID, queueID, name string, params map[string]string) string {
	t.Helper()

	data, err := os.ReadFile(presetPath(t, name))
	if err != nil {
		t.Fatalf("read preset %s: %v", name, err)
	}
	p, err := product.ParseDefinition(data, product.ValidateOptions{EnforceLimits: true})
	if err != nil {
		t.Fatalf("ParseDefinition(%s): %v", name, err)
	}

	q := url.Values{}
	q.Set("farm_id", farmID)
	q.Set("queue_id", queueID)
	q.Set("owner", "ffmpeg-preset-test")
	for k, v := range params {
		q.Set("param."+k, v)
	}

	contentType := "application/x-yaml"
	if strings.EqualFold(string(p.Format), "json") {
		contentType = "application/json"
	}

	var resp struct {
		ID string `json:"id"`
	}
	mustDoJSON(t, http.MethodPost,
		"http://"+ts.HTTPAddr+"/api/v1/jobs?"+q.Encode(),
		[]byte(p.Template), contentType, http.StatusCreated, &resp)
	if resp.ID == "" {
		t.Fatalf("submit %s: server returned empty job ID", name)
	}
	t.Logf("submitted preset %s as job %s", name, resp.ID)
	return resp.ID
}

// submitPresetExpecting submits a preset's template expecting a non-2xx status,
// and returns the RFC 7807 detail string the server answered with.
//
// Separate from [submitPreset] because that helper asserts 201 and returns a
// job ID; the rejection cases below have neither.
func submitPresetExpecting(
	t *testing.T, ts *testServer, farmID, queueID, name string,
	params map[string]string, wantStatus int,
) string {
	t.Helper()

	data, err := os.ReadFile(presetPath(t, name))
	if err != nil {
		t.Fatalf("read preset %s: %v", name, err)
	}
	p, err := product.ParseDefinition(data, product.ValidateOptions{EnforceLimits: true})
	if err != nil {
		t.Fatalf("ParseDefinition(%s): %v", name, err)
	}

	q := url.Values{}
	q.Set("farm_id", farmID)
	q.Set("queue_id", queueID)
	q.Set("owner", "ffmpeg-preset-test")
	for k, v := range params {
		q.Set("param."+k, v)
	}

	var problem struct {
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	mustDoJSON(t, http.MethodPost,
		"http://"+ts.HTTPAddr+"/api/v1/jobs?"+q.Encode(),
		[]byte(p.Template), "application/x-yaml", wantStatus, &problem)
	return problem.Detail
}

// taskLogs fetches a task's log without asserting on its contents.
// [pollTaskLogs] cannot serve the failure path below: it fails the test when
// its substring never arrives, so it can only be used when success is expected.
func taskLogs(t *testing.T, ts *testServer, taskID string) string {
	t.Helper()
	var resp struct {
		Items []struct {
			Data string `json:"data"`
		} `json:"items"`
	}
	mustDoJSON(t, http.MethodGet,
		"http://"+ts.HTTPAddr+"/api/v1/tasks/"+taskID+"/logs",
		nil, "", http.StatusOK, &resp)
	var sb strings.Builder
	for _, item := range resp.Items {
		sb.WriteString(item.Data)
	}
	return sb.String()
}

// reportFailedTasks renders every non-succeeded task of a job with its failure
// reason and the tail of its log.
//
// It reports the FAILED tasks specifically rather than the job's first task,
// because these presets fan out: a segmented run's first task is a Transcode
// slice that almost always succeeded, so dumping it buries the Join step's
// actual error under a full ffmpeg banner. Only the tail is kept for the same
// reason — ffmpeg's diagnosis is its last few lines.
func reportFailedTasks(t *testing.T, ts *testServer, jobID string) string {
	t.Helper()
	var resp struct {
		Items []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			StepID        string `json:"step_id"`
			Status        string `json:"status"`
			FailureReason string `json:"failure_reason"`
		} `json:"items"`
	}
	mustDoJSON(t, http.MethodGet,
		"http://"+ts.HTTPAddr+"/api/v1/jobs/"+jobID+"/tasks",
		nil, "", http.StatusOK, &resp)

	var sb strings.Builder
	for _, task := range resp.Items {
		if task.Status == "succeeded" {
			continue
		}
		fmt.Fprintf(&sb, "\n── task %q (id=%s step=%s) status=%s failure_reason=%q\n",
			task.Name, task.ID, task.StepID, task.Status, task.FailureReason)
		logs := taskLogs(t, ts, task.ID)
		const tail = 1200
		if len(logs) > tail {
			logs = "…" + logs[len(logs)-tail:]
		}
		sb.WriteString(logs)
	}
	if sb.Len() == 0 {
		return "(no unsucceeded tasks — the job itself never dispatched)"
	}
	return sb.String()
}

// runPresetJob submits a preset and blocks until it reaches a terminal status,
// failing unless that status is "completed".
func runPresetJob(t *testing.T, ts *testServer, farmID, queueID, name string, params map[string]string) {
	t.Helper()
	jobID := submitPreset(t, ts, farmID, queueID, name, params)
	got := pollJobStatus(t, ts, jobID,
		[]string{"completed", "failed", "canceled"}, jobTimeout)
	if got != "completed" {
		t.Fatalf("preset %s: job status = %q, want %q%s",
			name, got, "completed", reportFailedTasks(t, ts, jobID))
	}
}

// sliceFiles returns the intermediate slice files a segmented preset wrote
// beside its output, matching the "<stem>_seg_*<ext>" name the templates build.
func sliceFiles(t *testing.T, outputFile string) []string {
	t.Helper()
	ext := filepath.Ext(outputFile)
	stem := strings.TrimSuffix(filepath.Base(outputFile), ext)
	matches, err := filepath.Glob(filepath.Join(
		filepath.Dir(outputFile), stem+"_seg_*"+ext,
	))
	if err != nil {
		t.Fatalf("glob slices: %v", err)
	}
	return matches
}

// ── Whole-file transcode ──────────────────────────────────────────────────────

// TestFFmpegPreset_TranscodeProducesPlayableOutput runs ffmpeg-transcode, the
// base-spec preset, end to end. It asserts the output keeps the source's length
// and carries both streams — a command line that dropped -c:a, or read the
// wrong input, would not.
func TestFFmpegPreset_TranscodeProducesPlayableOutput(t *testing.T) {
	requireFFmpeg(t)

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorkerAnyOS(t, ts, farmID, queueID)

	dir := t.TempDir()
	source := makeSourceVideo(t, dir)
	output := filepath.Join(dir, "transcoded.mp4")

	runPresetJob(t, ts, farmID, queueID, "ffmpeg-transcode", map[string]string{
		"SourceFile": source,
		"OutputFile": output,
	})

	assertDuration(t, output, ffmpegSourceSeconds)
	if codecs := probeCodecs(t, output); len(codecs) != 2 {
		t.Errorf("output streams = %v, want one video and one audio stream", codecs)
	}
}

// ── Segmented transcode ───────────────────────────────────────────────────────

// runSegmentPreset drives one segmented variant and applies the assertions all
// three share: the join must reproduce the full source length, and the run must
// have fanned out into ffmpegWantSlices tasks rather than transcoding once.
func runSegmentPreset(t *testing.T, name string, wantSlicesKept bool) {
	t.Helper()
	requireFFmpeg(t)

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorkerAnyOS(t, ts, farmID, queueID)

	dir := t.TempDir()
	source := makeSourceVideo(t, dir)
	output := filepath.Join(dir, "joined.mp4")

	runPresetJob(t, ts, farmID, queueID, name, map[string]string{
		"SourceFile":      source,
		"OutputFile":      output,
		"DurationSeconds": strconv.Itoa(ffmpegSourceSeconds),
		"SegmentSeconds":  strconv.Itoa(ffmpegSegmentSeconds),
	})

	// The load-bearing assertion. Each slice is ffmpegSegmentSeconds long, so a
	// join that lost one lands a full segment short — far outside the tolerance.
	assertDuration(t, output, ffmpegSourceSeconds)

	// Slice retention is a documented, per-variant promise: the shell variants
	// clean up after a successful join, the portable one deliberately does not.
	slices := sliceFiles(t, output)
	if wantSlicesKept {
		if len(slices) != ffmpegWantSlices {
			t.Errorf("kept slices = %d %v, want %d — the parameter space should "+
				"expand to one task per slice, and this variant documents that "+
				"it leaves them behind", len(slices), slices, ffmpegWantSlices)
		}
		return
	}
	if len(slices) != 0 {
		t.Errorf("leftover slices = %v, want none — this variant documents that "+
			"it removes them once the join succeeds", slices)
	}
}

// TestFFmpegPreset_PortableSegmentTranscodeJoins runs the shell-free variant.
// It is the highest-value case in this file: its concat list is rendered by the
// TEMPLATE, so the assertion covers EXPR embedded-file generation on top of the
// ffmpeg command lines, and it is the one variant that runs on every platform.
func TestFFmpegPreset_PortableSegmentTranscodeJoins(t *testing.T) {
	runSegmentPreset(t, "ffmpeg-segment-transcode-expr", true)
}

// TestFFmpegPreset_BashSegmentTranscodeJoins runs the bash-joined variant,
// whose template gates on attr.worker.tag.ffmpeg ALONE — it carries no
// attr.worker.os.family requirement, because bash is not a POSIX-only
// interpreter: git-bash puts it on Windows too. The skip below therefore keys
// on bash being on PATH rather than on GOOS, which is the preset's actual
// requirement restated.
//
// It runs on darwin as well as linux, and that is load-bearing rather than
// incidental: until scheduler.osFamily landed, a Mac worker reported GOOS
// "darwin" against a requirement that could only legally say "macos", so this
// preset validated, submitted, and then waited forever for a worker that could
// not exist. An earlier revision of this test skipped darwin FOR THAT REASON.
// Running here is the end-to-end proof that a Mac can take the work — a
// regression would show up as this test timing out rather than failing fast.
//
// The template invokes "command: bash" with the script as an argument rather
// than exec'ing the shebang script directly, since direct shebang execution is
// a POSIX kernel feature (binfmt_script) Windows has no equivalent of. On a
// Windows host this case is the only automated coverage of that invocation and
// of the script's backslash folding; it has NOT been run on a real Windows
// host, so treat a green run here as covering the POSIX half only.
func TestFFmpegPreset_BashSegmentTranscodeJoins(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("ffmpeg-segment-transcode-bash requires bash on PATH: %v", err)
	}
	runSegmentPreset(t, "ffmpeg-segment-transcode-bash", false)
}

// TestFFmpegPreset_PowerShellSegmentTranscodeJoins runs the PowerShell-joined
// variant. Its template gates on attr.worker.os.family = windows, so a Windows
// runner is the only place this preset's join script — the one that has already
// shipped two runtime-only bugs — can be executed at all.
func TestFFmpegPreset_PowerShellSegmentTranscodeJoins(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skipf("ffmpeg-segment-transcode-powershell requires a windows worker; GOOS=%s", runtime.GOOS)
	}
	runSegmentPreset(t, "ffmpeg-segment-transcode-powershell", false)
}

// ── Slice arithmetic ──────────────────────────────────────────────────────────
//
// The three cases below exist because the fixture geometry the tests above
// share — 6 seconds cut into 2-second slices — is a WHOLE MULTIPLE, and a whole
// multiple hides two mistakes that the segmented presets are specifically at
// risk of. Both are properties of the EXPR expressions rather than of ffmpeg,
// and neither is visible to the schema tests in internal/product.

// TestFFmpegPreset_SegmentCountUsesCeilingNotTruncation pins the slice-count
// expression, `ceil(Param.DurationSeconds / Param.SegmentSeconds)`.
//
// 6/2 divides exactly, so the tests above pass identically whether that
// expression ceils, floors, or truncates — the arithmetic the entire EXPR
// extension was built to enable is, at that geometry, unpinned. 5 seconds in
// 2-second slices needs ceil(2.5) = 3; truncation yields 2, which transcodes
// only the first 4 seconds and loses the tail. The duration assertion catches
// that: 4s against an expected 5s is ten times the tolerance.
//
// It runs the portable variant because that is the one that executes on every
// platform, so this arithmetic is covered wherever the suite runs.
func TestFFmpegPreset_SegmentCountUsesCeilingNotTruncation(t *testing.T) {
	requireFFmpeg(t)

	const (
		sourceSeconds  = 5
		segmentSeconds = 2
		wantSlices     = 3 // ceil(5/2); truncation would give 2
	)

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorkerAnyOS(t, ts, farmID, queueID)

	dir := t.TempDir()
	source := makeSourceVideoOfLength(t, dir, sourceSeconds)
	output := filepath.Join(dir, "joined.mp4")

	runPresetJob(t, ts, farmID, queueID, "ffmpeg-segment-transcode-expr", map[string]string{
		"SourceFile":      source,
		"OutputFile":      output,
		"DurationSeconds": strconv.Itoa(sourceSeconds),
		"SegmentSeconds":  strconv.Itoa(segmentSeconds),
	})

	assertDuration(t, output, sourceSeconds)

	// The slice count is asserted separately from the duration because the two
	// fail for different reasons: a wrong count means the range expression is
	// wrong, while a right count with a short duration means the join dropped
	// something. The final slice covers [4s, 6s) against a 5s source, so it is
	// the short-tail case as well — ffmpeg stops at EOF and writes ~1s.
	if slices := sliceFiles(t, output); len(slices) != wantSlices {
		t.Errorf("slices = %d %v, want %d — the task range should be "+
			"ceil(%d/%d)-1, so a truncating division would produce %d",
			len(slices), slices, wantSlices, sourceSeconds, segmentSeconds, wantSlices-1)
	}
}

// sliceIndexRe matches the zero-padded index in a slice filename built by
// `zfill(<index>, 5)`.
var sliceIndexRe = regexp.MustCompile(`_seg_(\d{5})\.`)

// TestFFmpegPreset_SliceNamesAreZeroPadded pins the `zfill(..., 5)` in the
// slice-path expression, which the tests above cannot see.
//
// Padding exists so that LEXICAL order is NUMERIC order. That matters because
// the bash and PowerShell variants build their concat list by globbing the
// directory and sorting by name — with unpadded indices, `_seg_10` sorts
// between `_seg_1` and `_seg_2`, and those two variants would join the slices
// out of order. Fewer than ten slices cannot distinguish the two orderings at
// all, and every other case in this file uses three.
//
// A duration assertion would NOT catch it: a mis-ordered join still contains
// every slice and still runs the full length. So this asserts the filenames the
// glob actually sorts, which is the property the ordering rests on.
func TestFFmpegPreset_SliceNamesAreZeroPadded(t *testing.T) {
	requireFFmpeg(t)

	const (
		sourceSeconds  = 11
		segmentSeconds = 1
		wantSlices     = 11 // two digits, so padding is load-bearing
	)

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorkerAnyOS(t, ts, farmID, queueID)

	dir := t.TempDir()
	source := makeSourceVideoOfLength(t, dir, sourceSeconds)
	output := filepath.Join(dir, "padded.mp4")

	runPresetJob(t, ts, farmID, queueID, "ffmpeg-segment-transcode-expr", map[string]string{
		"SourceFile":      source,
		"OutputFile":      output,
		"DurationSeconds": strconv.Itoa(sourceSeconds),
		"SegmentSeconds":  strconv.Itoa(segmentSeconds),
	})

	assertDuration(t, output, sourceSeconds)

	slices := sliceFiles(t, output)
	if len(slices) != wantSlices {
		t.Fatalf("slices = %d %v, want %d", len(slices), slices, wantSlices)
	}

	seen := map[int]bool{}
	for _, path := range slices {
		m := sliceIndexRe.FindStringSubmatch(filepath.Base(path))
		if m == nil {
			t.Errorf("slice %q has no five-digit index: zfill(index, 5) is what "+
				"makes lexical order numeric, and the bash and PowerShell joins "+
				"sort these names to order their concat list",
				filepath.Base(path))
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Errorf("slice %q: parse index %q: %v", filepath.Base(path), m[1], err)
			continue
		}
		seen[n] = true
	}
	for i := range wantSlices {
		if !seen[i] {
			t.Errorf("no slice with index %d; got %v", i, slices)
		}
	}

	// sliceFiles globs, and Glob returns sorted names — so on a correct run the
	// lexical order the shell joins depend on is already the numeric order.
	if !sort.StringsAreSorted(slices) {
		t.Errorf("glob output is not sorted: %v", slices)
	}
	if got := sliceIndexRe.FindStringSubmatch(filepath.Base(slices[0])); got != nil && got[1] != "00000" {
		t.Errorf("first slice by name is index %q, want 00000 — slices are "+
			"joined in this order", got[1])
	}
}

// TestFFmpegPreset_PortableRejectsBeyondItsCostCeiling asserts the HTTP contract
// a user actually meets when they ask the portable variant for more slices than
// its submission budget allows.
//
// internal/product/exprpresetcost_test.go already pins the ceiling at the
// submitter, but it asserts on a Go error value. What a pipeline sees is a
// status code, and the distinction matters: a 422 says the request is wrong and
// retrying is pointless, while the 503 this must NOT be says the server gave up
// under load and a retry may work. Nothing else asserts that mapping for a
// shipped preset, and no worker is needed to prove it.
func TestFFmpegPreset_PortableRejectsBeyondItsCostCeiling(t *testing.T) {
	requireFFmpeg(t)

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	// Far past the documented 400-slice ceiling, so this stays true if the
	// per-slice cost is tuned. 60-second slices keeps the count equal to the
	// minutes asked for.
	const wildlyTooManySlices = 4000

	detail := submitPresetExpecting(t, ts, farmID, queueID,
		"ffmpeg-segment-transcode-expr", map[string]string{
			"SourceFile":      filepath.Join(t.TempDir(), "unused.mp4"),
			"OutputFile":      filepath.Join(t.TempDir(), "unused-out.mp4"),
			"DurationSeconds": strconv.Itoa(wildlyTooManySlices * 60),
			"SegmentSeconds":  "60",
		}, http.StatusUnprocessableEntity)

	// The status alone is not enough: a typo in a parameter name would also be
	// a 422. This must be the cost meter refusing, not the parser.
	if !strings.Contains(detail, "operation limit") {
		t.Errorf("rejection detail = %q, want it to name the operation limit — "+
			"a 422 for any other reason means this test no longer covers the "+
			"cost ceiling", detail)
	}
}

// ── Sequence encode ───────────────────────────────────────────────────────────

// TestFFmpegPreset_SequenceEncodeNamesOutputAfterPattern runs
// ffmpeg-sequence-encode over a rendered image sequence.
//
// The output path is not a parameter: the template derives it with
// `rstrip(split(Param.SourcePattern.stem, '%')[0], '_-.') + '.mp4'`, so
// frame_%04d.png must become frame.mp4. Asserting the file exists at exactly
// that path is what pins the expression; a change that left the trailing
// separator on would write frame_.mp4 and fail here.
func TestFFmpegPreset_SequenceEncodeNamesOutputAfterPattern(t *testing.T) {
	requireFFmpeg(t)

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorkerAnyOS(t, ts, farmID, queueID)

	dir := t.TempDir()
	seqDir := filepath.Join(dir, "seq")
	if err := os.MkdirAll(seqDir, 0o755); err != nil {
		t.Fatalf("mkdir seq: %v", err)
	}
	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir out: %v", err)
	}

	const (
		frames    = 24
		frameRate = 12
	)
	runFFmpeg(
		t, "ffmpeg",
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=10",
		"-frames:v", strconv.Itoa(frames),
		filepath.Join(seqDir, "frame_%04d.png"),
	)

	runPresetJob(t, ts, farmID, queueID, "ffmpeg-sequence-encode", map[string]string{
		"SourcePattern": filepath.Join(seqDir, "frame_%04d.png"),
		"StartFrame":    "1",
		"EndFrame":      strconv.Itoa(frames),
		"FrameRate":     strconv.Itoa(frameRate),
		"OutputDir":     outDir,
	})

	assertDuration(t, filepath.Join(outDir, "frame.mp4"), float64(frames)/float64(frameRate))
}
