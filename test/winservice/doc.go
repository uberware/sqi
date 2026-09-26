// SPDX-License-Identifier: AGPL-3.0-or-later

// Package winservice drives sqi-server and sqi-worker through the real
// Windows Service Control Manager. The tests carry the `winservice` build tag,
// need an elevated shell on a real Windows host, and are run by
// `make test-service-windows` and the winservice-integration CI job, which
// asserts every test PASSED by name — a skip here verifies nothing.
package winservice
