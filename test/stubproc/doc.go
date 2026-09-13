// SPDX-License-Identifier: AGPL-3.0-or-later

// Command stubproc is a stand-in for any vendor CLI a preset invokes.
//
// It records the argv, working directory and stdin it was called with, and can
// be told to fail, hang, create an output file, or emit OpenJD progress lines.
// Installed on a worker's PATH under a preset's real command name (Render,
// ffmpeg, kick, pwsh), it turns "does this preset run end to end?" into a test
// that needs no license and no vendor install — Tier 3 of the preset validation
// harness.
//
// It lives under test/ and NOT under cmd/ on purpose: it must never appear in a
// release archive. Nothing in the server or worker imports it.
//
// Recording stdin is not incidental. Some real CLIs (modo_cl, for one) are
// driven by piping commands in rather than by argv, which makes an argv
// snapshot the wrong surface for them; a recorded stdin gives that shape
// something to assert on.
package main
