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
	// spec fixture) and the document kind the fixture exercises, so pass/fail
	// is a meaningful signal.
	StateLive State = iota
	// StateNotApplicable means either the fixture's extension is not in sqi's
	// registry, or the fixture is a document kind sqi does not implement
	// (standalone "environment-2023-09" templates, env_templates). Its result
	// is reported separately and never counted as a pass.
	//
	// This prevents two kinds of false pass:
	//
	//  1. sqi rejects templates declaring an unregistered extension, so every
	//     ".invalid" fixture for such an extension is rejected for the wrong
	//     reason and would score as a pass.
	//  2. sqi rejects every env_templates fixture on "/specificationVersion:
	//     unsupported version", regardless of the fixture's own defect, so a
	//     ".invalid" env_templates fixture would score as a pass the same way.
	//     base/env_templates alone has 24 such fixtures.
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

// KindFor returns the template kind a fixture belongs to, given its path
// relative to the suite root: "job_templates" or "env_templates".
//
// The layout is <extension>/<kind>/<file>.yaml, so the kind is the parent
// directory.
func KindFor(path string) string {
	return filepath.Base(filepath.Dir(path))
}

// Classify reports whether fixtures for the given extension directory and
// template kind ("job_templates" or "env_templates") should be scored.
//
// kind is checked first: sqi does not implement standalone
// environment-2023-09 templates, so every env_templates fixture, whatever its
// extension, is not applicable. Otherwise "base" is always live, and any other
// extension is live only when it is in internal/openjd's extension registry
// and marked openjd.StatusSupported. A registered extension that is in
// progress is not live: validateExtensions rejects every such template on its
// status, so scoring its fixtures would report a false failure for every
// valid one and a false pass for every ".invalid" one.
func Classify(extension, kind string) State {
	if kind == "env_templates" {
		return StateNotApplicable
	}
	if extension == "base" {
		return StateLive
	}
	if ext, ok := openjd.LookupExtension(extension); ok && ext.Status == openjd.StatusSupported {
		return StateLive
	}
	return StateNotApplicable
}
