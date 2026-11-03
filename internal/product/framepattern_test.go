// SPDX-License-Identifier: AGPL-3.0-or-later

package product_test

import (
	"maps"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// framePatternCase is a shipped preset's frame-pattern parameter:
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
	// extraBad lists patterns this preset rejects beyond the ones every preset
	// rejects.
	extraBad []badPattern
}

// badPattern is a frame pattern a preset must reject, under a short subtest
// label.
type badPattern struct {
	label   string
	pattern string
}

// atSigns are patterns holding a literal @. oiiotool expands @ as a wildcard
// the way it does #, in a folder or in the file name, so oiiotool-process
// rejects it.
var atSigns = []badPattern{
	{"folder-at", "/mnt/take@2/shot.####.exr"},
	{"name-at", "/mnt/s/shot@2x.####.exr"},
}

// braces are patterns holding a { or }. usdrecord formats the output path with
// str.format and oiiotool evaluates {...} in a file argument as an expression,
// so both presets reject a brace anywhere.
var braces = []badPattern{
	{"folder-brace", "/mnt/take{2}/shot.####.exr"},
	{"name-brace", "/mnt/s/shot{x}.####.exr"},
}

// oiiotoolBad is what oiiotool-process rejects beyond the shared rules.
var oiiotoolBad = append(append([]badPattern{}, atSigns...), braces...)

// framePatternCases lists the presets checked by
// TestFramePatternPresets_RejectABadPattern.
var framePatternCases = []framePatternCase{
	{
		preset: "usdrecord-render", param: "OutputFile", label: "Output Image Pattern",
		params:   map[string]string{"SceneFile": "/mnt/s/stage.usd", "FramesPerTask": "1"},
		extraBad: braces,
	},
	{
		preset: "oiiotool-process", param: "InputFile", label: "Input Sequence Pattern",
		params:   map[string]string{"OutputFile": "/mnt/o/out.####.png", "FramesPerTask": "1"},
		extraBad: oiiotoolBad,
	},
	{
		preset: "oiiotool-process", param: "OutputFile", label: "Output Sequence Pattern",
		params:   map[string]string{"InputFile": "/mnt/i/in.####.exr", "FramesPerTask": "1"},
		extraBad: oiiotoolBad,
	},
}

// TestFramePatternPresets_RejectABadPattern asserts a frame pattern with no run
// of #, or with two runs, or with a # in its folder, is rejected at submission
// with the parameter's label in the message, and a valid one expands to two
// tasks. oiiotool expands a # in any argument, folders included, and usdrecord
// reads a # anywhere in the path as a second frame placeholder, so both presets
// reject a # in the folder.
func TestFramePatternPresets_RejectABadPattern(t *testing.T) {
	for _, fc := range framePatternCases {
		t.Run(fc.preset+"/"+fc.param, func(t *testing.T) {
			tmpl := presetTemplate(t, fc.preset)
			submit := func(pattern string) (*openjd.SubmitResult, error) {
				params := map[string]string{"Frames": "1-2", fc.param: pattern}
				maps.Copy(params, fc.params)
				return submitPreset(t, tmpl, params)
			}
			bad := append([]badPattern{
				{"no-run", "/mnt/s/shot.0001.exr"},
				{"two-runs", "/mnt/s/sh##ot.####.exr"},
				{"folder-hash", "/mnt/take#2/shot.####.exr"},
			}, fc.extraBad...)
			for _, b := range bad {
				t.Run(b.label, func(t *testing.T) {
					_, err := submit(b.pattern)
					assertRejectedPattern(t, err, fc.label, badPatternMessage)
				})
			}
			t.Run("good", func(t *testing.T) {
				res, err := submit("/mnt/s/shot.####.exr")
				if err != nil {
					t.Fatalf("a valid pattern was rejected: %v", err)
				}
				if len(res.Tasks) != 2 {
					t.Errorf("expanded to %d tasks, want 2", len(res.Tasks))
				}
			})
		})
	}
}
