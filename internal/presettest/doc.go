// SPDX-License-Identifier: AGPL-3.0-or-later

// Package presettest is the preset validation harness.
//
// It answers one question about a shipped preset: for a stated set of job
// parameters, what exactly does each task run? [Capture] answers it by driving
// the production pipeline (submit, build the assignment, resolve it as a worker
// does), and [Render] turns the answer into a reviewable golden file.
//
// The harness models nothing. Every step is production code reached through its
// own package (internal/openjd, internal/scheduler, internal/worker/executor),
// because a harness that recomputes what it asserts agrees with itself rather
// than with the server. internal/scheduler/seam.go and
// internal/worker/executor/seam.go exist to expose that code to it.
//
// A Tier-1 golden proves the template expands and resolves to a specific,
// reviewed command line. It does not prove that command line is what the
// vendor's application wants: that is documentation-derived, per preset, and
// presets/validation-tiers.yaml carries a caveat saying so.
package presettest
