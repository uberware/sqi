// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// preset_progress_test.go — checks each shipped preset's SQI_LOG_PROGRESS
// block against output captured from the real application.
//
// The fixtures under testdata/progress are the application's stdout and stderr,
// stored verbatim apart from the scratch path, which is replaced by <SCRATCH>.
// Leading "#" lines record the command and the application's version line and
// are not fed to the matcher. A pattern is only as good as the output it was
// written against: these rows are what keeps a pattern that matches nothing (a
// version that changed its line format, or a pattern written from the manual)
// from shipping.
//
// NOT tagged `integration`, like preset_ffmpeg_test.go, so the CI `test` job
// runs it. It needs no application: the fixtures stand in for it.

import (
	"bufio"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/openjd/logprogress"
	"github.com/uberware/sqi/internal/product"
)

// presetLogProgress returns the SQI_LOG_PROGRESS block of the shipped preset's
// only step that declares one, read from the preset file itself.
func presetLogProgress(t *testing.T, preset string) *openjd.LogProgress {
	t.Helper()
	data, err := os.ReadFile(presetPath(t, preset))
	if err != nil {
		t.Fatalf("read preset %s: %v", preset, err)
	}
	p, err := product.ParseDefinition(data, product.ValidateOptions{EnforceLimits: true})
	if err != nil {
		t.Fatalf("ParseDefinition(%s): %v", preset, err)
	}
	tmpl, err := openjd.Parse([]byte(p.Template), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("openjd.Parse(%s): %v", preset, err)
	}
	var found *openjd.LogProgress
	for _, s := range tmpl.Steps {
		if s.LogProgress == nil {
			continue
		}
		if found != nil {
			t.Fatalf("%s declares SQI_LOG_PROGRESS on more than one step", preset)
		}
		found = s.LogProgress
	}
	if found == nil {
		t.Fatalf("%s declares no SQI_LOG_PROGRESS block", preset)
	}
	return found
}

// progressFixtureLines returns a fixture's output lines, without its leading
// "#" comment lines.
func progressFixtureLines(t *testing.T, name string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "progress", name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var lines []string
	header := true
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if header && strings.HasPrefix(line, "#") {
			continue
		}
		header = false
		lines = append(lines, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 {
		t.Fatalf("fixture %s holds no output", name)
	}
	return lines
}

// TestPresetProgress_Fixtures feeds each fixture to its preset's own
// SQI_LOG_PROGRESS block and checks the distinct progress values it reports, in
// order, and that some status text contains the expected words.
func TestPresetProgress_Fixtures(t *testing.T) {
	// Frames 1-3 in one task: each frame starts at index/3, and the sample
	// counter moves within it, so 1/8 of a frame adds 4 points.
	blenderWant := []int{0, 4, 33, 37, 66, 70, 100}
	// A tiled Cycles frame prints a per-tile sample count, which restarts with
	// every tile, so the tile count measures the frame instead. Blender never
	// prints the last tile as rendered (5/6 is its final count).
	blenderTiledWant := []int{0, 5, 11, 16, 22, 27, 33, 38, 44, 50, 55, 61, 66, 72, 77, 83, 88, 94}
	cases := []struct {
		preset             string
		fixture            string
		frames             string
		wantProgress       []int
		wantStatusContains string
	}{
		{"blender-batch-render", "blender-5.2.2-cycles.txt", "1-3", blenderWant, "Sample 1/8"},
		{"blender-batch-render", "blender-5.2.2-eevee.txt", "1-3", blenderWant, "Rendering 1 / 8 samples"},
		{"blender-batch-render", "blender-5.2.2-cycles-tiled.txt", "1-3", blenderTiledWant, "Rendered 1/6 Tiles, Sample 1/8"},
		{"blender-view-layer-render", "blender-5.2.2-cycles.txt", "1-3", blenderWant, "Sample 1/8"},
		{"blender-view-layer-render", "blender-5.2.2-eevee.txt", "1-3", blenderWant, "Rendering 1 / 8 samples"},
		{"blender-view-layer-render", "blender-5.2.2-cycles-tiled.txt", "1-3", blenderTiledWant, "Rendered 1/6 Tiles, Sample 1/8"},
		// Nuke prints "Frame N (i of n)" once per frame, after every Write node
		// has written it, so the bar moves once per frame however many Writes
		// the script renders.
		{"nuke-write-render", "nuke-17.1v2-write.txt", "1-3", []int{33, 66, 100}, "Frame 1 (1 of 3)"},
		{"nuke-script-render", "nuke-17.1v2-script.txt", "1-3", []int{33, 66, 100}, "Frame 1 (1 of 3)"},
	}
	for _, tc := range cases {
		t.Run(tc.preset+"/"+tc.fixture, func(t *testing.T) {
			lp := presetLogProgress(t, tc.preset)
			m, err := logprogress.Compile(lp.Progress, lp.Status, tc.frames)
			if err != nil {
				t.Fatalf("logprogress.Compile: %v", err)
			}
			var progress []int
			var statuses []string
			for _, line := range progressFixtureLines(t, tc.fixture) {
				u := m.Observe(line)
				if u.HasProgress && (len(progress) == 0 || progress[len(progress)-1] != u.Progress) {
					progress = append(progress, u.Progress)
				}
				if u.HasStatus {
					statuses = append(statuses, u.Status)
				}
			}
			if !slices.Equal(progress, tc.wantProgress) {
				t.Errorf("distinct progress = %v, want %v", progress, tc.wantProgress)
			}
			if !slices.ContainsFunc(statuses, func(s string) bool { return strings.Contains(s, tc.wantStatusContains) }) {
				t.Errorf("no status contains %q; statuses seen: %q", tc.wantStatusContains, statuses)
			}
		})
	}
}

// watchedTask is the slice of GET /api/v1/tasks/{id} watchTaskProgress reads.
type watchedTask struct {
	Status     string `json:"status"`
	Progress   *int   `json:"progress"`
	StatusText string `json:"status_text"`
}

// getWatchedTask fetches a task's status and progress fields.
func getWatchedTask(t *testing.T, ts *testServer, taskID string) watchedTask {
	t.Helper()
	var resp watchedTask
	mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/tasks/"+taskID), nil, "", http.StatusOK, &resp)
	return resp
}

// watchTaskProgress polls a task until it is terminal and returns the distinct
// progress values it reported, in the order seen. It fails the test when the
// task is not terminal within timeout.
func watchTaskProgress(t *testing.T, ts *testServer, taskID string, timeout time.Duration) []int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var seen []int
	for {
		got := getWatchedTask(t, ts, taskID)
		if got.Progress != nil && (len(seen) == 0 || seen[len(seen)-1] != *got.Progress) {
			seen = append(seen, *got.Progress)
		}
		switch got.Status {
		case "succeeded", "failed", "canceled":
			return seen
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s not terminal after %s: status %q, progress seen %v", taskID, timeout, got.Status, seen)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
