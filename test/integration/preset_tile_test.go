// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

import (
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
	"github.com/uberware/sqi/internal/store"
)

// The arnold-kick-tile-render preset splits each frame into a TilesX x TilesY
// grid of kick render regions and assembles them in a dependent step. These
// tests drive the SHIPPED preset through presettest.Capture -- the real
// submission and worker resolution path -- at shapes the two Tier-1 goldens do
// not cover, because the region arithmetic is the one place a tile preset can
// silently lose pixels.

const tilePreset = "arnold-kick-tile-render"

func captureTiles(t *testing.T, params map[string]string) (presettest.Snapshot, error) {
	t.Helper()
	tmpl, err := presettest.PresetTemplate(presettest.SourcePresets, tilePreset)
	if err != nil {
		t.Fatalf("PresetTemplate: %v", err)
	}
	base := map[string]string{
		"SceneFile":  "/mnt/show/shot.####.ass",
		"OutputFile": "/mnt/out/beauty.####.exr",
		"Frames":     "1",
	}
	maps.Copy(base, params)
	return presettest.Capture(t.Context(), tmpl, presettest.Options{Params: base, Format: store.TemplateFormatYAML})
}

func tileStep(t *testing.T, snap presettest.Snapshot, name string) presettest.StepSnapshot {
	t.Helper()
	for _, s := range snap.Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no step %q in snapshot", name)
	return presettest.StepSnapshot{}
}

// renderRegion returns the four -rg bounds of one Render task.
func renderRegion(t *testing.T, task presettest.TaskSnapshot) [4]int {
	t.Helper()
	i := slices.Index(task.Args, "-rg")
	if i < 0 || i+4 >= len(task.Args) {
		t.Fatalf("task %s has no complete -rg region: %v", task.Name, task.Args)
	}
	var r [4]int
	for j := range r {
		v, err := strconv.Atoi(task.Args[i+1+j])
		if err != nil {
			t.Fatalf("task %s -rg value %q: %v", task.Name, task.Args[i+1+j], err)
		}
		r[j] = v
	}
	return r
}

// assertPartition checks that spans, sorted, tile [0, size-1] exactly: the
// first starts at 0, the last ends at size-1, each starts one past the
// previous one's end, and none is empty.
func assertPartition(t *testing.T, axis string, spans [][2]int, size int) {
	t.Helper()
	slices.SortFunc(spans, func(a, b [2]int) int { return a[0] - b[0] })
	next := 0
	for _, s := range spans {
		if s[0] != next {
			t.Errorf("%s: span %v starts at %d, want %d (gap or overlap)", axis, s, s[0], next)
		}
		if s[1] < s[0] {
			t.Errorf("%s: span %v is empty", axis, s)
		}
		next = s[1] + 1
	}
	if next != size {
		t.Errorf("%s: spans end at %d, want %d", axis, next-1, size-1)
	}
}

// assertRegionsPartition checks that the Render step's regions tile a
// width x height image exactly: one task per tile, no region rendered twice, and
// the column and row spans each covering their axis with no gap or overlap.
func assertRegionsPartition(t *testing.T, render presettest.StepSnapshot, width, height, tilesX, tilesY int) {
	t.Helper()
	if want := tilesX * tilesY; len(render.Tasks) != want {
		t.Fatalf("Render has %d tasks, want %d", len(render.Tasks), want)
	}
	xs := map[[2]int]bool{}
	ys := map[[2]int]bool{}
	regions := map[[4]int]bool{}
	for _, task := range render.Tasks {
		r := renderRegion(t, task)
		xs[[2]int{r[0], r[2]}] = true
		ys[[2]int{r[1], r[3]}] = true
		regions[r] = true
	}
	if len(regions) != len(render.Tasks) {
		t.Errorf("%d distinct regions across %d tasks: some region is rendered twice", len(regions), len(render.Tasks))
	}
	assertPartition(t, "x", slices.Collect(maps.Keys(xs)), width)
	assertPartition(t, "y", slices.Collect(maps.Keys(ys)), height)
}

// TestArnoldKickTileRender_RegionsPartitionTheImage pins the region
// arithmetic: at every resolution, divisible by the tile count or not, the
// tiles cover every pixel exactly once. The 16x16 maximum is covered by
// TestArnoldKickTileRender_MaxTilesResolve.
func TestArnoldKickTileRender_RegionsPartitionTheImage(t *testing.T) {
	for _, tc := range []struct{ size, tiles int }{
		{1, 1}, {7, 2}, {7, 7}, {1000, 3},
	} {
		name := strconv.Itoa(tc.size) + "px-" + strconv.Itoa(tc.tiles) + "tiles"
		t.Run(name, func(t *testing.T) {
			size, tiles := strconv.Itoa(tc.size), strconv.Itoa(tc.tiles)
			snap, err := captureTiles(t, map[string]string{
				"Width": size, "Height": size, "TilesX": tiles, "TilesY": tiles,
			})
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			assertRegionsPartition(t, tileStep(t, snap, "Render"), tc.size, tc.size, tc.tiles, tc.tiles)
		})
	}
}

// TestArnoldKickTileRender_RejectsMoreTilesThanPixels pins the submission-time
// refusal of a grid finer than the image, which would otherwise produce
// empty regions.
func TestArnoldKickTileRender_RejectsMoreTilesThanPixels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params map[string]string
		want   string
	}{
		{"x", map[string]string{"Width": "4", "Height": "4", "TilesX": "5", "TilesY": "1"}, "Tiles X must not exceed Width"},
		{"y", map[string]string{"Width": "4", "Height": "4", "TilesX": "1", "TilesY": "5"}, "Tiles Y must not exceed Height"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := captureTiles(t, tc.params)
			if err == nil {
				t.Fatal("submission accepted a tile count larger than the resolution")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not say %q: %v", tc.want, err)
			}
		})
	}
}

// TestArnoldKickTileRender_MaxTilesResolve submits the largest grid the
// parameters allow (16 x 16) and checks that both steps resolve under the
// stock EXPR limits, that the 256 regions partition a non-square image, and
// that the assembly receives every tile.
func TestArnoldKickTileRender_MaxTilesResolve(t *testing.T) {
	snap, err := captureTiles(t, map[string]string{
		"Width": "1920", "Height": "1080", "TilesX": "16", "TilesY": "16",
	})
	if err != nil {
		t.Fatalf("Capture at 16x16: %v", err)
	}
	assertRegionsPartition(t, tileStep(t, snap, "Render"), 1920, 1080, 16, 16)
	assemble := tileStep(t, snap, "Assemble")
	if len(assemble.Tasks) != 1 {
		t.Fatalf("Assemble has %d tasks, want 1", len(assemble.Tasks))
	}
	// A worker renders PATH parameters in its own host's form (\mnt\out\...
	// on Windows), so compare slash-normalized.
	args := make([]string, len(assemble.Tasks[0].Args))
	for i, a := range assemble.Tasks[0].Args {
		args[i] = filepath.ToSlash(a)
	}
	if len(args) != 260 {
		t.Fatalf("Assemble has %d args, want 260 (256 tiles + 4)", len(args))
	}
	if got, want := args[256:], []string{"--paste:mergeroi=1:all=1", "+0+0", "-o", "/mnt/out/beauty.0001.exr"}; !slices.Equal(got, want) {
		t.Errorf("Assemble trailing args = %v, want %v", got, want)
	}
	for i, a := range args[:256] {
		if !strings.HasPrefix(a, "/mnt/out/beauty.0001_tile_") || !strings.HasSuffix(a, ".exr") {
			t.Errorf("Assemble arg %d = %q, want a tile path", i, a)
		}
	}
}
