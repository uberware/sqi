// SPDX-License-Identifier: AGPL-3.0-or-later

// Package chaos holds the Go-test entry point of the chaos suite: it runs
// every built-in chaos-* scenario (internal/loadtest/scenario/library) end to
// end against a real sqi-server, with simulated workers and real sqi-worker
// processes, while the scenario's faults kill, stall and cut them, and fails
// on any invariant the run breaks that the scenario does not expect.
//
// This file carries no build tag. The tests are behind the "chaos" tag, and a
// package whose files are all excluded by build constraints is a build error
// for the wildcard patterns that `make test-integration` (./test/...) and the
// lint targets expand; this file keeps the package well-formed when the tag
// is absent.
//
// Run it with `make chaos` (SCENARIO=chaos-... for one) or `make chaos-smoke`.
// SQI_CHAOS_STORE=postgres runs the scenarios on PostgreSQL, which needs
// SQI_TEST_POSTGRES_DSN; SQI_LOAD_OUT names the directory the runs are
// written to.
package chaos
