// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import "runtime"

// defaultProbe is the production implementation of [Probe]. It calls real OS
// APIs via platform-specific helper functions defined in probe_linux.go,
// probe_darwin.go, and probe_windows.go.
type defaultProbe struct{}

// newDefaultProbe returns the production Probe implementation.
func newDefaultProbe() Probe {
	return &defaultProbe{}
}

// OS returns runtime.GOOS ("linux", "darwin", "windows", ...).
func (*defaultProbe) OS() string {
	return runtime.GOOS
}

// Arch returns runtime.GOARCH ("amd64", "arm64", ...). Reported verbatim; the
// server maps it onto OpenJD's attr.worker.cpu.arch vocabulary.
func (*defaultProbe) Arch() string {
	return runtime.GOARCH
}

// CPUCount returns runtime.NumCPU(), the number of logical CPUs usable by the
// current process.
func (*defaultProbe) CPUCount() int {
	return runtime.NumCPU()
}

// OSVersion, RAMMb, and GPUInfo are implemented in the platform-specific files.
