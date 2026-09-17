// SPDX-License-Identifier: AGPL-3.0-or-later

// Package record is the wire shape of one stubproc invocation record.
//
// It is a package of its own so the writer (test/stubproc, a main package) and
// the reader (test/integration's tier-3 test) share one declaration: a second
// copy of this struct would be free to drift from the JSON actually written.
package record

// Record is one invocation of the stub, as written to SQI_STUB_RECORD: one JSON
// object per line, appended.
type Record struct {
	// Command is the basename the stub was invoked as ("Render", "ffmpeg").
	Command string `json:"command"`
	// Args are the arguments after argv[0].
	Args []string `json:"args"`
	// Cwd is the working directory at invocation.
	Cwd string `json:"cwd"`
	// Stdin is what was piped in, truncated to stdinLimit bytes.
	Stdin string `json:"stdin,omitempty"`
}
