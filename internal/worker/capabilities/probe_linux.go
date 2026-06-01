// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package capabilities

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// OSVersion reads /etc/os-release and returns the VERSION_ID value (e.g.
// "22.04" for Ubuntu 22.04). Falls back to the VERSION field, then to an
// empty string if the file cannot be read or the fields are absent.
func (*defaultProbe) OSVersion() string {
	return linuxOSVersion()
}

// RAMMb reads /proc/meminfo and returns MemTotal in mebibytes. Returns 0 on
// any read or parse error.
func (*defaultProbe) RAMMb() int {
	return linuxRAMMb()
}

// GPUInfo attempts to detect NVIDIA GPUs via the kernel driver's sysfs tree at
// /proc/driver/nvidia/gpus/. Returns a zero GPUInfo when no NVIDIA GPUs are
// found, when the driver is not loaded, or when reads fail.
func (*defaultProbe) GPUInfo() GPUInfo {
	return linuxGPUInfo()
}

// ── helpers ────────────────────────────────────────────────────────────────────

func linuxOSVersion() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return ""
	}
	defer f.Close()

	var versionID, version string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		switch k {
		case "VERSION_ID":
			versionID = v
		case "VERSION":
			version = v
		}
	}
	if versionID != "" {
		return versionID
	}
	return version
}

func linuxRAMMb() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		// Format: "MemTotal:       16384000 kB"
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return int(kb / 1024)
	}
	return 0
}

func linuxGPUInfo() GPUInfo {
	const nvidiaGPUDir = "/proc/driver/nvidia/gpus"
	entries, err := os.ReadDir(nvidiaGPUDir)
	if err != nil || len(entries) == 0 {
		return GPUInfo{}
	}

	gpu := GPUInfo{
		Vendor: "NVIDIA",
		Count:  len(entries),
	}

	// Attempt to parse the first GPU's information file for Model and Memory.
	infoPath := filepath.Join(nvidiaGPUDir, entries[0].Name(), "information")
	data, err := os.ReadFile(infoPath)
	if err != nil {
		return gpu
	}

	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := sc.Text()
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "Model":
			gpu.Model = v
		case "GPU Memory":
			// Format: "4096 MB" or similar.
			fields := strings.Fields(v)
			if len(fields) >= 1 {
				if mb, err := strconv.Atoi(fields[0]); err == nil {
					gpu.VRAMMb = mb
				}
			}
		}
	}
	return gpu
}
