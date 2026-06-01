// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package capabilities

import (
	"fmt"
	"math"
	"syscall"
	"unsafe"
)

// OSVersion returns a Windows version string of the form
// "major.minor.build" (e.g. "10.0.22621") derived from RtlGetVersion so that
// the result is not subject to application-compatibility shims.
func (*defaultProbe) OSVersion() string {
	return windowsOSVersion()
}

// RAMMb calls GlobalMemoryStatusEx and returns total physical RAM in
// mebibytes. Returns 0 on failure.
func (*defaultProbe) RAMMb() int {
	return windowsRAMMb()
}

// GPUInfo returns a zero GPUInfo on Windows. Full GPU detection via WMI or
// Direct3D is deferred to a future phase.
func (*defaultProbe) GPUInfo() GPUInfo {
	return GPUInfo{}
}

// ── helpers ────────────────────────────────────────────────────────────────────

// rtlOSVersionInfoW mirrors the Win32 RTL_OSVERSIONINFOW structure.
type rtlOSVersionInfoW struct {
	OSVersionInfoSize uint32
	MajorVersion      uint32
	MinorVersion      uint32
	BuildNumber       uint32
	PlatformID        uint32
	CSDVersion        [128]uint16
}

func windowsOSVersion() string {
	ntdll, err := syscall.LoadDLL("ntdll.dll")
	if err != nil {
		return ""
	}
	rtlGetVersion, err := ntdll.FindProc("RtlGetVersion")
	if err != nil {
		return ""
	}
	var info rtlOSVersionInfoW
	info.OSVersionInfoSize = uint32(unsafe.Sizeof(info))
	// RtlGetVersion returns an NTSTATUS (0 = STATUS_SUCCESS). The third return
	// value is the Win32 last-error, which is not meaningful for this call;
	// success is indicated solely by the NTSTATUS in r1.
	ret, _, _ := rtlGetVersion.Call(uintptr(unsafe.Pointer(&info))) //nolint:errcheck // success indicated by NTSTATUS r1, not lastErr
	if ret != 0 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", info.MajorVersion, info.MinorVersion, info.BuildNumber)
}

// memoryStatusEx mirrors MEMORYSTATUSEX.
type memoryStatusEx struct {
	dwLength                uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

func windowsRAMMb() int {
	kernel32, err := syscall.LoadDLL("kernel32.dll")
	if err != nil {
		return 0
	}
	globalMemoryStatusEx, err := kernel32.FindProc("GlobalMemoryStatusEx")
	if err != nil {
		return 0
	}
	var ms memoryStatusEx
	ms.dwLength = uint32(unsafe.Sizeof(ms))
	// GlobalMemoryStatusEx returns non-zero on success. The third return value
	// is the Win32 last-error; success/failure is indicated solely by r1.
	ret, _, _ := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))) //nolint:errcheck // success indicated by bool r1, not lastErr
	if ret == 0 {
		return 0
	}
	mb := ms.ullTotalPhys / (1024 * 1024)
	if mb > math.MaxInt {
		return math.MaxInt
	}
	return int(mb)
}
