// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// preset_oiiotool_test.go - executes presets/sqi/oiiotool-process.yaml through
// a real sqi-server and a real sqi-worker against a real oiiotool, then asserts
// on the files it writes.
//
// The argv goldens show that the preset hands oiiotool `@` runs, and Tier 3
// shows the worker delivers them. Only a real oiiotool shows that a run of `@`
// in a pattern is read as that many digits, that --frames takes the chunk's
// first and last frame, and that the operations list reaches it as separate
// arguments.
//
// Untagged, like preset_ffmpeg_test.go, so the CI `test` job runs it. It uses
// the oiiotool on PATH, or else Houdini's bundled hoiiotool through the
// worker's command default, and skips when neither exists, which means A LOCAL
// PASS PROVES NOTHING ON ITS OWN: look for the
// "--- PASS: TestPresetOIIOTool_ProcessesASequence" line.

import (
	"context"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestPresetOIIOTool_ProcessesASequence runs oiiotool-process over a twelve
// frame sequence the test writes with oiiotool itself. The input folder has a
// space in its name. Each subtest asserts on the exact set of files written,
// which shows that the pattern's padding was honored, that no frame outside
// the range was written, and that each task covered only its own chunk.
func TestPresetOIIOTool_ProcessesASequence(t *testing.T) {
	const caseName = "TestPresetOIIOTool_ProcessesASequence"
	trackOutcome(t, caseName)
	exe, ok := locateDCC(t, "oiiotool")
	if !ok {
		skipOutcome(t, caseName, "oiiotool is not installed: no \"oiiotool\" on PATH and none found by the "+
			"worker's detector (Houdini's hoiiotool). Install it (apt-get install openimageio-tools / "+
			"brew install openimageio) or put it on PATH to run this case")
	}

	in := filepath.Join(t.TempDir(), "in dir")
	if err := os.MkdirAll(in, 0o755); err != nil {
		t.Fatal(err)
	}
	// oiiotool reads @ as one digit each, so this writes in.0001.png to
	// in.0012.png.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, exe, "--frames", "1-12", "--pattern", "constant:color=1,0,0", "8x8", "3",
		"-o", filepath.Join(in, "in.@@@@.png")).CombinedOutput(); err != nil {
		t.Fatalf("generating the input sequence: %v\n%s", err, out)
	}

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorkerAnyOS(t, ts, farmID, queueID)

	// oiiotool does not create the folder it writes into (measured), so the
	// output folders exist before the jobs run.
	mkOut := func(t *testing.T, name string) string {
		t.Helper()
		out := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		return out
	}

	t.Run("convert", func(t *testing.T) {
		out := mkOut(t, "out")
		job := runDCCJob(t, ts, farmID, queueID, "oiiotool-process", map[string]string{
			"InputFile": filepath.Join(in, "in.####.png"), "OutputFile": filepath.Join(out, "conv.####.jpg"),
			"Frames": "1-12", "FramesPerTask": "10",
		})
		assertTaskCount(t, ts, job, 2)
		assertExactFiles(t, out, frameNames("conv.%04d.jpg", seq(1, 12)...))
	})

	t.Run("operations", func(t *testing.T) {
		out := mkOut(t, "small")
		job := runDCCJob(t, ts, farmID, queueID, "oiiotool-process", map[string]string{
			"InputFile": filepath.Join(in, "in.####.png"), "OutputFile": filepath.Join(out, "small.###.png"),
			"Frames": "2-3", "Operations": `["--resize", "4x4"]`,
		})
		assertTaskCount(t, ts, job, 1)
		names := []string{"small.002.png", "small.003.png"}
		assertExactFiles(t, out, names)
		for _, name := range names {
			f, err := os.Open(filepath.Join(out, name))
			if err != nil {
				t.Errorf("open %s: %v", name, err)
				continue
			}
			cfg, err := png.DecodeConfig(f)
			_ = f.Close()
			if err != nil {
				t.Errorf("%s is not a PNG: %v", name, err)
			} else if cfg.Width != 4 || cfg.Height != 4 {
				t.Errorf("%s is %dx%d, want 4x4", name, cfg.Width, cfg.Height)
			}
		}
	})
}
