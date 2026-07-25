// SPDX-License-Identifier: AGPL-3.0-or-later

// Package conformance runs the official OpenJD conformance test suite
// (https://github.com/OpenJobDescription/openjd-specifications) against
// sqi's internal/openjd implementation.
//
// The suite is supplied by a pinned git submodule under third_party/ rather
// than vendored, because it is licensed CC BY-ND 4.0: verbatim redistribution
// is permitted but modified copies are not, and this repo's mandatory SPDX
// header would make every vendored fixture a derivative work.
//
// # File layout
//
// Everything except suite_test.go is untagged, so the harness's own unit tests
// run under `make test` and count toward coverage. Only suite_test.go — the
// file that walks the real fixture tree — carries the `conformance` build tag,
// because only it needs the submodule present.
package conformance
