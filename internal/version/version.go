// SPDX-License-Identifier: AGPL-3.0-or-later

// Package version holds build-time metadata injected by the release toolchain.
//
// Variables are set via -ldflags at build time (see Makefile and
// .goreleaser.yaml). Local development builds fall back to "dev" / "unknown".
//
// Example ldflags (from the Makefile):
//
//	-X github.com/uberware/sqi/internal/version.Version=v0.1.0
//	-X github.com/uberware/sqi/internal/version.Commit=abc1234
//	-X github.com/uberware/sqi/internal/version.BuildDate=2026-05-29T12:00:00Z
//	-X github.com/uberware/sqi/internal/version.GoVersion=go1.26.3
package version

// Version is the semantic version tag, e.g. "v0.1.0" or "dev".
var Version = "dev"

// Commit is the short git commit hash of the build.
var Commit = "unknown"

// BuildDate is the UTC timestamp of the build in RFC 3339 format.
var BuildDate = "unknown"

// GoVersion is the Go toolchain version used to build the binary.
var GoVersion = "unknown"
