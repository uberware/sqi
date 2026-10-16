// SPDX-License-Identifier: AGPL-3.0-or-later

package product_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// The tile preset's Assemble step lists every tile of a frame in an EXPR
// comprehension, so its submission cost grows with TilesX x TilesY. Measured
// through the real Submitter at 16 x 16 (the largest grid the parameters allow):
// 536 operations, 16,448 live bytes, 38 positions and 512 retained bytes -- well
// inside the stock limits, but the 256-element list is above the CONFIGURABLE
// memory floor (openjd.expr_memory_limit's minimum, 4,096 bytes), where the
// largest square grid that still submits is 7 x 7. The output path's length does
// not move that figure: the list's elements depend on the task's frame, so at
// submission they are unresolved placeholders of fixed size.
//
// These constants are what the preset's tier caveat states; the tests below
// pin them.
const (
	tileGridMax         = 16
	tileGridAtTheFloors = 7
)

func submitTileGrid(t *testing.T, lim openjd.ExprLimits, n int) error {
	t.Helper()
	st := fake.New()
	farmID, queueID := seedFarmQueue(t, st)
	_, err := openjd.NewSubmitterWithOptions(st, openjd.SubmitterOptions{ExprLimits: lim}).Submit(
		t.Context(), perFileTemplate(t, "arnold-kick-tile-render"), store.TemplateFormatYAML,
		openjd.SubmitOptions{FarmID: farmID, QueueID: queueID, Parameters: map[string]string{
			"SceneFile": "/mnt/show/shot.####.ass", "OutputFile": "/mnt/show/images/beauty.####.exr",
			"Frames": "1", "Width": "1920", "Height": "1080",
			"TilesX": strconv.Itoa(n), "TilesY": strconv.Itoa(n),
		}},
	)
	return err
}

// TestArnoldKickTileRender_GridCostAtTheLimits pins where the tile grid sits
// against the server's EXPR limits: the largest grid submits under the stock
// limits, and under the four configurable floors the documented grid submits
// while the next one up fails on the memory limit.
func TestArnoldKickTileRender_GridCostAtTheLimits(t *testing.T) {
	t.Run("largest grid at the stock limits", func(t *testing.T) {
		if err := submitTileGrid(t, openjd.ExprLimits{}, tileGridMax); err != nil {
			t.Fatalf("%dx%d must submit under the stock limits: %v", tileGridMax, tileGridMax, err)
		}
	})
	floors := openjd.ExprLimits{
		SubmissionOperations:  openjd.MinExprSubmissionOperations,
		SubmissionMemoryBytes: openjd.MinExprSubmissionMemoryBytes,
		TemplatePositions:     openjd.MinExprTemplatePositions,
		TemplateRetainedBytes: openjd.MinExprTemplateRetainedBytes,
	}
	t.Run("documented grid at the floors", func(t *testing.T) {
		if err := submitTileGrid(t, floors, tileGridAtTheFloors); err != nil {
			t.Fatalf("%dx%d must submit at the configurable floors: %v", tileGridAtTheFloors, tileGridAtTheFloors, err)
		}
	})
	t.Run("one larger at the floors", func(t *testing.T) {
		err := submitTileGrid(t, floors, tileGridAtTheFloors+1)
		if err == nil {
			t.Fatalf("%dx%d submitted at the floors; the documented ceiling is stale", tileGridAtTheFloors+1, tileGridAtTheFloors+1)
		}
		if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok || !strings.Contains(err.Error(), "memory limit exceeded") {
			t.Errorf("want a memory-limit validation error, got %T: %v", err, err)
		}
	})
}

// TestArnoldKickTileRender_CaveatStatesTheGridCost keeps the tier caveat in
// step with the constants above.
func TestArnoldKickTileRender_CaveatStatesTheGridCost(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "presets", "validation-tiers.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	i := strings.Index(body, "- name: arnold-kick-tile-render")
	if i < 0 {
		t.Fatal("no registry entry for arnold-kick-tile-render")
	}
	entry := body[i:]
	if j := strings.Index(entry[1:], "\n  - name: "); j >= 0 {
		entry = entry[:j+1]
	}
	for _, want := range []string{
		strconv.Itoa(tileGridMax) + " x " + strconv.Itoa(tileGridMax),
		strconv.Itoa(tileGridAtTheFloors) + " x " + strconv.Itoa(tileGridAtTheFloors),
	} {
		if !strings.Contains(entry, want) {
			t.Errorf("arnold-kick-tile-render's caveat does not state the %s grid figure", want)
		}
	}
}
