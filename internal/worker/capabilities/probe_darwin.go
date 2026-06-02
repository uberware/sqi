// SPDX-License-Identifier: AGPL-3.0-only

//go:build darwin

package capabilities

import (
	"encoding/binary"
	"syscall"

	"golang.org/x/sys/unix"
)

// OSVersion queries the kern.osproductversion sysctl, which returns the
// human-readable macOS version string, e.g. "14.5". Returns an empty string
// on failure.
func (*defaultProbe) OSVersion() string {
	return darwinOSVersion()
}

// RAMMb queries the hw.memsize sysctl, which returns total physical RAM as a
// uint64 (bytes). Returns 0 on failure.
func (*defaultProbe) RAMMb() int {
	return darwinRAMMb()
}

// GPUInfo returns a zero GPUInfo on macOS. Detecting GPU details without
// spawning an external process (e.g. system_profiler) requires CGo and IOKit,
// which is deferred to a future phase.
func (*defaultProbe) GPUInfo() GPUInfo {
	return GPUInfo{}
}

// ── helpers ────────────────────────────────────────────────────────────────────

func darwinOSVersion() string {
	s, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		return ""
	}
	return s
}

func darwinRAMMb() int {
	// hw.memsize is an 8-byte little-endian uint64 on all Apple silicon and
	// Intel Mac hardware.
	raw, err := unix.SysctlRaw("hw.memsize")
	if err != nil || len(raw) < 8 {
		return 0
	}
	mb := binary.LittleEndian.Uint64(raw) / (1024 * 1024)
	if mb > uint64(^uint(0)>>1) {
		return int(^uint(0) >> 1)
	}
	return int(mb)
}
