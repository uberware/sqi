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
	// spec fixture) AND the document kind the fixture exercises, so pass/fail
	// is a meaningful signal.
	StateLive State = iota
	// StateNotApplicable means either the fixture's extension is not in sqi's
	// registry, or the fixture is a document kind sqi does not implement at
	// all (standalone "environment-2023-09" templates — env_templates). Its
	// result is reported separately and NEVER counted as a pass.
	//
	// This distinction is load-bearing, and it now guards against two
	// distinct false-green failure modes:
	//
	//  1. sqi rejects templates declaring an unregistered extension, which is
	//     correct behavior — but it means every ".invalid" fixture for such
	//     an extension is rejected for the wrong reason and would score as a
	//     pass under naive pass/fail classification. All 209 EXPR template
	//     fixtures would report green before a single line of EXPR existed.
	//  2. sqi does not implement standalone environment-2023-09 templates at
	//     all: every env_templates fixture is rejected on
	//     "/specificationVersion: unsupported version", regardless of the
	//     fixture's own defect. A ".invalid" env_templates fixture is
	//     therefore rejected for the wrong reason too, and would score as a
	//     pass the same way an unregistered-extension fixture does — this is
	//     the same bug class, just keyed on document kind instead of
	//     extension. base/env_templates alone had 24 fixtures scoring as
	//     false passes this way before Classify took kind into account.
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
// kind is checked first and unconditionally: sqi does not implement
// standalone environment-2023-09 templates at all, so every env_templates
// fixture — regardless of extension, including "base" and EXPR — is not
// applicable. Only once kind clears that gate does extension matter: "base" is
// always live, and any other extension is live only when it is registered in
// internal/openjd's extension registry AND marked openjd.StatusSupported.
// A registered-but-in-progress extension does not count as live: validateExtensions
// rejects every such template on the status gate alone, so scoring its fixtures
// through this path would report a false failure for every valid one and a
// false pass for every ".invalid" one, instead of the honest "not applicable".
// EXPR was exactly that case until sub-project H2 marked it supported; it is
// live now, and EXPR/job_templates is scored by TestConformance_Templates like
// every other live directory.
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
