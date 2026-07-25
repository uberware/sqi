// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

import (
	"path/filepath"

	"github.com/uberware/sqi/internal/openjd"
)

// State is whether a fixture's result counts toward the score.
type State int

const (
	// StateLive means sqi implements the fixture's extension (or it is a base
	// spec fixture), so pass/fail is a meaningful signal.
	StateLive State = iota
	// StateNotApplicable means the fixture's extension is not in sqi's
	// registry. Its result is reported separately and NEVER counted as a pass.
	//
	// This distinction is load-bearing. sqi rejects templates declaring an
	// unregistered extension, which is correct behavior — but it means every
	// ".invalid" fixture for such an extension is rejected for the wrong
	// reason and would score as a pass under naive pass/fail classification.
	// All 209 EXPR template fixtures would report green before a single line
	// of EXPR existed.
	StateNotApplicable
)

func (s State) String() string {
	switch s {
	case StateLive:
		return "live"
	case StateNotApplicable:
		return "n/a"
	default:
		return "unknown"
	}
}

// ExtensionFor returns the extension directory a fixture belongs to, given its
// path relative to the suite root: "base", "EXPR", "TASK_CHUNKING", and so on.
//
// The layout is <extension>/<kind>/<file>.yaml, so the extension is the
// grandparent directory.
func ExtensionFor(path string) string {
	return filepath.Base(filepath.Dir(filepath.Dir(path)))
}

// Classify reports whether fixtures for the given extension directory should
// be scored. "base" is always live; any other name is live only when it is
// registered in internal/openjd's extension registry.
func Classify(extension string) State {
	if extension == "base" {
		return StateLive
	}
	if _, ok := openjd.LookupExtension(extension); ok {
		return StateLive
	}
	return StateNotApplicable
}
