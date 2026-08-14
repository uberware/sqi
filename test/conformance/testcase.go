// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

import (
	"path/filepath"
	"strings"

	"github.com/uberware/sqi/internal/fsutil"
)

// TestCase is one conformance fixture, described by its filename.
//
// The naming convention is <spec-section>--<description>[.invalid][.test].yaml.
// The section prefix is optional: some extension fixtures (for example
// TASK_CHUNKING/jobs/contiguous-even.test.yaml) carry no "--" separator.
type TestCase struct {
	// Path is the fixture's path relative to the 2023-09 suite root.
	Path string
	// Section is the spec section under test ("1.1", "expr2.2.4"), or "" when
	// the filename carries no section prefix.
	Section string
	// Description is the human-readable remainder of the filename.
	Description string
	// Invalid reports whether the implementation is expected to REJECT this
	// fixture (filename carries ".invalid").
	Invalid bool
	// IsJobTest reports whether this is a job-execution test (".test"), which
	// requires a live session runtime and is out of scope for template
	// validation.
	IsJobTest bool
}

// IsFixtureFile reports whether a directory entry's base name is a conformance
// fixture the walker should collect.
//
// A ".yaml" suffix alone is not enough: AppleDouble companions keep the
// extension (see [fsutil.IsAppleDouble]), and no fixture in the suite begins
// with a dot, so leading-dot names are excluded outright — which also covers
// ".DS_Store" and any other OS metadata.
func IsFixtureFile(name string) bool {
	return strings.HasSuffix(name, ".yaml") &&
		!strings.HasPrefix(name, ".") &&
		!fsutil.IsAppleDouble(name)
}

// ParseTestCase derives a TestCase from a fixture path relative to the suite
// root. It never fails: an unrecognized filename yields a TestCase with an
// empty Section and the whole stem as Description, which the caller reports
// like any other test rather than silently dropping.
func ParseTestCase(path string) TestCase {
	tc := TestCase{Path: path}

	stem := strings.TrimSuffix(filepath.Base(path), ".yaml")

	// Strip known suffixes from the right. Order matters: ".invalid.test"
	// appears in that order, so ".test" comes off first.
	if s, ok := strings.CutSuffix(stem, ".test"); ok {
		tc.IsJobTest = true
		stem = s
	}
	if s, ok := strings.CutSuffix(stem, ".invalid"); ok {
		tc.Invalid = true
		stem = s
	}

	if section, desc, ok := strings.Cut(stem, "--"); ok {
		tc.Section = section
		tc.Description = desc
	} else {
		tc.Description = stem
	}

	return tc
}
