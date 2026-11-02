// SPDX-License-Identifier: AGPL-3.0-or-later

package product_test

import (
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// framePatternCase is a shipped preset with an output frame-pattern parameter:
// a path whose file name holds exactly one run of #, the run's length being the
// frame padding.
type framePatternCase struct {
	preset string
	// param is the parameter the pattern is submitted as.
	param string
	// label is the parameter's label, which the rejection message starts with.
	label string
	// params holds every other job parameter the preset requires.
	params map[string]string
	// goodTasks is the task count a valid pattern over Frames "1-2" expands to.
	goodTasks int
}

// framePatternCases lists the presets checked by
// TestFramePatternPresets_RejectABadPattern.
var framePatternCases = []framePatternCase{
	{
		preset: "usdrecord-render", param: "OutputFile", label: "Output Image Pattern",
		params:    map[string]string{"SceneFile": "/mnt/s/stage.usd", "FramesPerTask": "1"},
		goodTasks: 2,
	},
}

// TestFramePatternPresets_RejectABadPattern asserts a frame pattern with no run
// of #, or with two runs, is rejected at submission with the parameter's label
// in the message, and a valid one expands.
func TestFramePatternPresets_RejectABadPattern(t *testing.T) {
	for _, fc := range framePatternCases {
		t.Run(fc.preset+"/"+fc.param, func(t *testing.T) {
			tmpl := presetTemplate(t, fc.preset)
			submit := func(pattern string) (*openjd.SubmitResult, error) {
				params := map[string]string{"Frames": "1-2", fc.param: pattern}
				maps.Copy(params, fc.params)
				st := fake.New()
				farmID, queueID := seedFarmQueue(t, st)
				return openjd.NewSubmitter(st).Submit(t.Context(), tmpl, store.TemplateFormatYAML, openjd.SubmitOptions{
					FarmID: farmID, QueueID: queueID, Parameters: params,
				})
			}
			for _, bad := range []string{"/mnt/s/shot.0001.exr", "/mnt/s/sh##ot.####.exr"} {
				t.Run(bad, func(t *testing.T) {
					_, err := submit(bad)
					if err == nil {
						t.Fatalf("pattern %q was accepted; it must be rejected at submission", bad)
					}
					if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
						t.Fatalf("want a *openjd.SubmitValidationError, got %T: %v", err, err)
					}
					if !strings.Contains(err.Error(), fc.label) || !strings.Contains(err.Error(), badPatternMessage) {
						t.Errorf("error does not name %q and the rule %q: %v", fc.label, badPatternMessage, err)
					}
				})
			}
			t.Run("good pattern", func(t *testing.T) {
				res, err := submit("/mnt/s/shot.####.exr")
				if err != nil {
					t.Fatalf("a valid pattern was rejected: %v", err)
				}
				if len(res.Tasks) != fc.goodTasks {
					t.Errorf("expanded to %d tasks, want %d", len(res.Tasks), fc.goodTasks)
				}
			})
		})
	}
}
