// SPDX-License-Identifier: AGPL-3.0-or-later

// Package load holds the Go-test entry point of the load and soak harness
// (internal/loadtest, cmd/sqi-load): it runs the built-in smoke scenario end
// to end against a real sqi-server process with simulated workers.
//
// This file carries no build tag. The tests are behind the "load" tag, and a
// package whose files are all excluded by build constraints is a build error
// for the wildcard patterns that `make test-integration`
// (./test/...) and the lint targets expand; this file keeps the package
// well-formed when the tag is absent.
//
// Run it with `make load-smoke`. Longer runs, soaks and sweeps are driven by
// `go run ./cmd/sqi-load` (see the load, soak and load-sweep Make targets).
package load
