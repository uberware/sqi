// SPDX-License-Identifier: AGPL-3.0-or-later

// Package oracle differential-tests sqi's Go EXPR evaluator
// (internal/openjd/expr) against the implementation the OpenJD specification
// names as its reference, in the Recommended Library Interface section.
//
// This file carries no build tag on purpose. Every other file here is behind
// the "oracle" tag, and a package whose files are ALL excluded by build
// constraints is a build error for the wildcard patterns that `make
// test-integration` (./test/...) and the lint targets expand; this file keeps
// the package well-formed when the tag is absent.
//
// # What the oracle is
//
// The reference lives in the openjd.expr namespace of the openjd-model Python
// package — but it is not a Python implementation. That namespace is a thin
// re-export layer over a compiled Rust crate (openjd-expr, in
// OpenJobDescription/openjd-rs), so there is no source to read and it is used
// strictly as a black box: expressions in, values or errors out, through
// scripts/expr-oracle.py.
//
// # What it proves, and what it does not
//
// It proves that two independent implementations, written from the same
// specification in different languages, agree on a corpus of expressions. That
// catches a class of defect no single-implementation test can: a
// misreading of the spec that is applied consistently, and so looks correct
// from the inside. sqi's own plans were wrong against this spec repeatedly, and
// every one of those was a consistent misreading.
//
// It does NOT prove conformance. The official conformance suite does that, and
// it is already wired up — see test/conformance and `make test-conformance`.
// This target is supplementary: it explains WHY a fixture disagrees, and it
// reaches expressions no fixture happens to contain.
//
// # The reference is not the authority
//
// openjd-expr is Beta: on the 0.x line, where breaking changes are permitted
// in minor version bumps. The specification outranks it. A divergence is a
// question to investigate, not a verdict against sqi — and at least one
// baselined entry records the reference being wrong. Resolve disagreements by
// reading third_party/openjd-specifications, not by matching the reference.
//
// # A skip proves nothing
//
// The test skips when no interpreter has the package importable, and the
// Makefile target exits 0 in that case, exactly as `make test-isolation` does
// for a missing Docker. A local run that prints nothing has verified nothing:
// confirm the test actually ran, with GOFLAGS=-count=1 and a --- PASS line.
package oracle
