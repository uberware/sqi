// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux && !darwin && !windows

package capabilities

// OSVersion returns an empty string on platforms without explicit support.
func (*defaultProbe) OSVersion() string { return "" }

// RAMMb returns 0 on platforms without explicit support.
func (*defaultProbe) RAMMb() int { return 0 }

// GPUInfo returns a zero GPUInfo on platforms without explicit support.
func (*defaultProbe) GPUInfo() GPUInfo { return GPUInfo{} }
