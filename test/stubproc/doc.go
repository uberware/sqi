// SPDX-License-Identifier: AGPL-3.0-or-later

// Command stubproc is a stand-in for any vendor CLI a preset invokes.
//
// It records the argv, working directory and stdin it was called with, and can
// be told to fail, hang, create an output file, print a line, or emit OpenJD
// progress lines. Those settings come from SQI_STUB_* environment variables,
// or, with SQI_STUB_ENV_FROM_ARG, from a sidecar file named by an argument, so
// each invocation under one worker can be configured differently.
// Installed on a worker's PATH under a preset's real command name (Render,
// ffmpeg, kick, aerender), it lets a test run a preset end to end with no
// license and no vendor install. This is Tier 3 of the preset validation
// harness.
//
// It lives under test/ rather than cmd/ so that it never appears in a release
// archive. Nothing in the server or worker imports it.
//
// Stdin is recorded because some CLIs (modo_cl, for one) are driven by
// commands piped in rather than by argv, so a test of them asserts on stdin.
package main
