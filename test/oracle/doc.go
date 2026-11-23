// SPDX-License-Identifier: AGPL-3.0-or-later

// Package oracle differential-tests sqi's Go EXPR evaluator
// (internal/openjd/expr) against the implementation the OpenJD specification
// names as its reference, in the Recommended Library Interface section.
//
// This file has no build tag. Every other file here is behind the "oracle"
// tag, and a package whose files are all excluded by build constraints is a
// build error for the wildcard patterns that `make test-integration`
// (./test/...) and the lint targets expand; this file keeps the package
// well-formed when the tag is absent.
//
// # What the oracle is
//
// The reference is the openjd.expr namespace of the openjd-model Python
// package. That namespace re-exports a compiled Rust crate (openjd-expr, in
// OpenJobDescription/openjd-rs), so it is used as a black box through
// scripts/expr-oracle.py: expressions in, values or errors out.
//
// # What it shows
//
// It shows that two independent implementations, written from the same
// specification in different languages, agree on a corpus of expressions.
// That catches a misreading of the specification applied consistently, which
// tests of a single implementation cannot see.
//
// It does not show conformance; the official conformance suite does that (see
// test/conformance and `make test-conformance`). The oracle helps explain why
// a fixture disagrees, and it covers expressions no fixture contains.
//
// # The reference is not the authority
//
// openjd-expr is on the 0.x line, where minor versions may make breaking
// changes. The specification outranks it. A divergence is a question to
// investigate, not a verdict against sqi, and some baselined entries record
// the reference being wrong. Resolve disagreements by reading
// third_party/openjd-specifications, not by matching the reference.
//
// # Skips
//
// The test skips when no interpreter can import the package, and the Makefile
// target exits 0 in that case. A local run that skips has checked nothing:
// confirm the test ran, with GOFLAGS=-count=1 and a --- PASS line.
package oracle
