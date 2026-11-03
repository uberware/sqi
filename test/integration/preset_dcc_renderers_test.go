// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// preset_dcc_renderers_test.go — renders prman-render and 3delight-render with
// the real renderers, through a real sqi-server and sqi-worker, from scene files
// the test writes as text, and asserts on the frames they write. Like the other
// TestPresetDCC_ cases they skip when the renderer is absent, so a local pass
// proves nothing unless SQI_TEST_REQUIRE_APPS lists renderman or 3delight.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ribScene is a RenderMan RIB file rendering a lit sphere to one 64x36 PNG.
const ribScene = `##RenderMan RIB
version 3.04
Display "%s" "png" "rgba"
Format 64 36 1
Hider "raytrace" "int maxsamples" [4]
Integrator "PxrPathTracer" "pt"
Projection "perspective" "fov" [40]
Translate 0 0 5
WorldBegin
  Light "PxrDomeLight" "dome" "float intensity" [1]
  Bxdf "PxrDiffuse" "d" "color diffuseColor" [0.9 0.3 0.2]
  Sphere 1 -1 1 360
WorldEnd
`

// nsiScene is a 3Delight NSI stream rendering a quad to one 64x36 PNG. The
// RenderControl lines start the render and wait for it: renderdl renders
// nothing for a stream without them.
const nsiScene = `Create "mesh1" "mesh"
SetAttribute "mesh1" "nvertices" "int" 1 4 "P" "point" 4 [ -1 -1 0  -1 1 0  1 1 0  1 -1 0 ]
Connect "mesh1" "" ".root" "objects"
Create "cam_xform" "transform"
SetAttribute "cam_xform" "transformationmatrix" "doublematrix" 1 [ 1 0 0 0  0 1 0 0  0 0 1 0  0 0 5 1 ]
Connect "cam_xform" "" ".root" "objects"
Create "cam" "perspectivecamera"
SetAttribute "cam" "fov" "float" 1 40
Connect "cam" "" "cam_xform" "objects"
Create "screen1" "screen"
SetAttribute "screen1" "resolution" "int[2]" 1 [ 64 36 ]
Connect "screen1" "" "cam" "screens"
Create "layer1" "outputlayer"
SetAttribute "layer1" "variablename" "string" 1 "Ci" "scalarformat" "string" 1 "uint8" "layertype" "string" 1 "color" "withalpha" "int" 1 1
Connect "layer1" "" "screen1" "outputlayers"
Create "driver1" "outputdriver"
SetAttribute "driver1" "drivername" "string" 1 "png" "imagefilename" "string" 1 "%s"
Connect "driver1" "" "layer1" "outputdrivers"
%s`

// nsiStart is the RenderControl pair that makes renderdl render a stream.
const nsiStart = `RenderControl "action" "string" 1 "start"
RenderControl "action" "string" 1 "wait"
`

// rendererCase is one job of a per-file renderer preset: the frames submitted,
// the frames whose scene files exist, and the PNGs the job must leave.
type rendererCase struct {
	name   string
	frames string
	files  []int
	want   []string
}

// rendererCases are the frame ranges both renderer tests submit. Each scene
// file names its own output, frame.<frame>.png at four digits.
var rendererCases = []rendererCase{
	{"one frame per task", "1-3", []int{1, 2, 3}, frameNames("frame.%04d.png", 1, 2, 3)},
	{"stepped", "2-6:2", []int{2, 4, 6}, frameNames("frame.%04d.png", 2, 4, 6)},
	{"negative", "-1", []int{-1}, []string{"frame.-001.png"}},
}

// writeScenes writes one scene file per frame into dir as shot.<frame>.<ext>,
// each rendering to out/frame.<frame>.png, and returns the preset's Scene File
// Pattern for them. It does not create out.
func writeScenes(t *testing.T, dir, out, ext string, frames []int, scene func(png string) string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range frames {
		png := filepath.ToSlash(filepath.Join(out, fmt.Sprintf("frame.%04d.png", f)))
		path := filepath.Join(dir, fmt.Sprintf("shot.%04d.%s", f, ext))
		if err := os.WriteFile(path, []byte(scene(png)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "shot.####."+ext)
}

// runRendererCases runs rendererCases through preset, each in its own folder
// under root, and checks each job leaves exactly its frames at 64x36. With
// createOut false the output folder does not exist when the job starts, so the
// renderer must create it.
func runRendererCases(t *testing.T, ts *testServer, farmID, queueID, preset, root, ext string, createOut bool, scene func(png string) string) {
	t.Helper()
	for _, tc := range rendererCases {
		t.Run(tc.name, func(t *testing.T) {
			stem := strings.ReplaceAll(tc.name, " ", "_")
			out := filepath.Join(root, "renders", stem)
			if createOut {
				if err := os.MkdirAll(out, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			pattern := writeScenes(t, filepath.Join(root, "scenes", stem), out, ext, tc.files, scene)
			job := runDCCJob(t, ts, farmID, queueID, preset, map[string]string{"SceneFile": pattern, "Frames": tc.frames})
			assertTaskCount(t, ts, job, len(tc.files))
			assertExactFiles(t, out, tc.want)
			assertPNGSize(t, out, tc.want, 64, 36)
		})
	}
}

// TestPresetDCC_RenderMan_PrmanRender runs prman-render under RenderMan's own
// prman, found through the renderman detector's command default, on RIB files
// the test writes into a folder whose name has a space. The frames on disk
// show that each task renders its own frame's file, that a stepped range
// renders no file it leaves out, and that frame -1 reaches the file
// shot.-001.rib. A frame whose file is missing, or whose output folder does
// not exist, fails the job with prman's own message: prman does not create
// the folder. prman needs RMANTREE to find its license, as a worker does.
func TestPresetDCC_RenderMan_PrmanRender(t *testing.T) {
	const caseName = "TestPresetDCC_RenderMan_PrmanRender"
	trackOutcome(t, caseName)
	requireDCC(t, caseName, "RenderMan", "prman")
	if os.Getenv("RMANTREE") == "" {
		skipOutcome(t, caseName, "RMANTREE is not set: prman needs it to find its license. "+
			"RenderMan's installer sets it for the whole machine; start the test from a shell opened after the install")
	}

	root := filepath.Join(t.TempDir(), "rman shot")
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorkerAnyOS(t, ts, farmID, queueID)

	rib := func(png string) string { return fmt.Sprintf(ribScene, png) }
	runRendererCases(t, ts, farmID, queueID, "prman-render", root, "rib", true, rib)

	failures := []struct {
		name, message string
		files         []int
	}{
		{"missing file fails", "Can't open RIB file", nil},
		{"missing output folder fails", "Can't open display image", []int{1}},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			stem := strings.ReplaceAll(tc.name, " ", "_")
			pattern := writeScenes(t, filepath.Join(root, "scenes", stem), filepath.Join(root, "renders", stem), "rib", tc.files, rib)
			report := runDCCJobExpectingFailure(t, ts, farmID, queueID, "prman-render", map[string]string{"SceneFile": pattern, "Frames": "1"})
			if !strings.Contains(report, tc.message) {
				t.Errorf("failed task report does not show prman's %q:\n%s", tc.message, report)
			}
		})
	}
}

// TestPresetDCC_3Delight_Render runs 3delight-render under 3Delight's own
// renderdl, found through the threedelight detector's command default, on NSI
// files the test writes into a folder whose name has a space. The frames on
// disk show what TestPresetDCC_RenderMan_PrmanRender's do, and that renderdl
// creates an output folder that does not exist. renderdl exits 0 on
// a file it cannot open and on a stream that never starts a render, so the
// failure cases show the preset's log checks failing both jobs.
func TestPresetDCC_3Delight_Render(t *testing.T) {
	const caseName = "TestPresetDCC_3Delight_Render"
	trackOutcome(t, caseName)
	requireDCC(t, caseName, "3Delight", "renderdl")

	root := filepath.Join(t.TempDir(), "3dl shot")
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorkerAnyOS(t, ts, farmID, queueID)

	runRendererCases(t, ts, farmID, queueID, "3delight-render", root, "nsi", false,
		func(png string) string { return fmt.Sprintf(nsiScene, png, nsiStart) })

	failures := []struct {
		name, reason string
		files        []int
		scene        func(png string) string
	}{
		{"missing file fails", "matched failOn", nil, nil},
		{"no render control fails", "never printed", []int{1}, func(png string) string { return fmt.Sprintf(nsiScene, png, "") }},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			stem := strings.ReplaceAll(tc.name, " ", "_")
			pattern := writeScenes(t, filepath.Join(root, "scenes", stem), filepath.Join(root, "renders", stem), "nsi", tc.files, tc.scene)
			report := runDCCJobExpectingFailure(t, ts, farmID, queueID, "3delight-render", map[string]string{"SceneFile": pattern, "Frames": "1"})
			if !strings.Contains(report, tc.reason) {
				t.Errorf("failed task report does not say %q:\n%s", tc.reason, report)
			}
		})
	}
}
