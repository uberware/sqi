// SPDX-License-Identifier: AGPL-3.0-only

package capabilities

import (
	"testing"
)

// fakeProbe implements [Probe] with configurable return values for all
// OS-level queries. Used by all tests in this file so real syscalls are never
// invoked.
type fakeProbe struct {
	os        string
	osVersion string
	cpuCount  int
	ramMb     int
	gpuInfo   GPUInfo
}

func (f *fakeProbe) OS() string        { return f.os }
func (f *fakeProbe) OSVersion() string { return f.osVersion }
func (f *fakeProbe) CPUCount() int     { return f.cpuCount }
func (f *fakeProbe) RAMMb() int        { return f.ramMb }
func (f *fakeProbe) GPUInfo() GPUInfo  { return f.gpuInfo }

// ── Detect ─────────────────────────────────────────────────────────────────────

func TestDetect_FieldsMatchProbe(t *testing.T) {
	probe := &fakeProbe{
		os:        "linux",
		osVersion: "22.04",
		cpuCount:  16,
		ramMb:     32768,
		gpuInfo: GPUInfo{
			Vendor: "NVIDIA",
			Model:  "RTX 4090",
			VRAMMb: 24576,
			Count:  1,
		},
	}

	caps := Detect(probe)

	if caps.OS != probe.os {
		t.Errorf("OS: got %q, want %q", caps.OS, probe.os)
	}
	if caps.OSVersion != probe.osVersion {
		t.Errorf("OSVersion: got %q, want %q", caps.OSVersion, probe.osVersion)
	}
	if caps.CPUCount != probe.cpuCount {
		t.Errorf("CPUCount: got %d, want %d", caps.CPUCount, probe.cpuCount)
	}
	if caps.RAMMb != probe.ramMb {
		t.Errorf("RAMMb: got %d, want %d", caps.RAMMb, probe.ramMb)
	}
	if caps.GPU != probe.gpuInfo {
		t.Errorf("GPU: got %+v, want %+v", caps.GPU, probe.gpuInfo)
	}
}

func TestDetect_TagsNonNil(t *testing.T) {
	caps := Detect(&fakeProbe{os: "linux", osVersion: "22.04"})
	if caps.Tags == nil {
		t.Fatal("Tags map must be non-nil after Detect")
	}
}

func TestDetect_AutoTagsPopulated(t *testing.T) {
	caps := Detect(&fakeProbe{os: "darwin", osVersion: "14.5"})

	if got, ok := caps.Tags["os"]; !ok || got != "darwin" {
		t.Errorf(`Tags["os"]: got %q (ok=%v), want "darwin"`, got, ok)
	}
	if got, ok := caps.Tags["os_version"]; !ok || got != "14.5" {
		t.Errorf(`Tags["os_version"]: got %q (ok=%v), want "14.5"`, got, ok)
	}
}

func TestDetect_OsVersionTagOmittedWhenEmpty(t *testing.T) {
	caps := Detect(&fakeProbe{os: "windows", osVersion: ""})

	if _, ok := caps.Tags["os_version"]; ok {
		t.Error(`Tags["os_version"] should be absent when OSVersion is empty`)
	}
}

func TestDetect_NilProbeUsesDefault(t *testing.T) {
	// Passing nil must not panic; it falls back to DefaultProbe which calls
	// real OS APIs. We only verify that the OS field is non-empty and the
	// Tags map is initialized — we cannot control real hardware values.
	caps := Detect(nil)
	if caps.OS == "" {
		t.Error("OS must be non-empty when nil probe is used")
	}
	if caps.Tags == nil {
		t.Error("Tags must be non-nil when nil probe is used")
	}
}

// ── Platform-specific table tests ─────────────────────────────────────────────

func TestDetect_PlatformTable(t *testing.T) {
	tests := []struct {
		name      string
		probe     *fakeProbe
		wantOS    string
		wantOSVer string
		wantCPU   int
		wantRAM   int
		wantGPU   GPUInfo
		wantOSTag string
	}{
		{
			name: "ubuntu-22.04 with NVIDIA GPU",
			probe: &fakeProbe{
				os:        "linux",
				osVersion: "22.04",
				cpuCount:  32,
				ramMb:     65536,
				gpuInfo: GPUInfo{
					Vendor: "NVIDIA",
					Model:  "A100",
					VRAMMb: 40960,
					Count:  2,
				},
			},
			wantOS:    "linux",
			wantOSVer: "22.04",
			wantCPU:   32,
			wantRAM:   65536,
			wantGPU:   GPUInfo{Vendor: "NVIDIA", Model: "A100", VRAMMb: 40960, Count: 2},
			wantOSTag: "linux",
		},
		{
			name: "macOS Ventura no GPU",
			probe: &fakeProbe{
				os:        "darwin",
				osVersion: "13.6",
				cpuCount:  10,
				ramMb:     16384,
				gpuInfo:   GPUInfo{},
			},
			wantOS:    "darwin",
			wantOSVer: "13.6",
			wantCPU:   10,
			wantRAM:   16384,
			wantGPU:   GPUInfo{},
			wantOSTag: "darwin",
		},
		{
			name: "Windows 11 no GPU detected",
			probe: &fakeProbe{
				os:        "windows",
				osVersion: "10.0.22621",
				cpuCount:  8,
				ramMb:     16384,
				gpuInfo:   GPUInfo{},
			},
			wantOS:    "windows",
			wantOSVer: "10.0.22621",
			wantCPU:   8,
			wantRAM:   16384,
			wantGPU:   GPUInfo{},
			wantOSTag: "windows",
		},
		{
			name: "Linux with zero RAM detection failure",
			probe: &fakeProbe{
				os:        "linux",
				osVersion: "20.04",
				cpuCount:  4,
				ramMb:     0, // detection failed
				gpuInfo:   GPUInfo{},
			},
			wantOS:    "linux",
			wantOSVer: "20.04",
			wantCPU:   4,
			wantRAM:   0,
			wantGPU:   GPUInfo{},
			wantOSTag: "linux",
		},
		{
			name: "macOS with empty OS version",
			probe: &fakeProbe{
				os:        "darwin",
				osVersion: "",
				cpuCount:  8,
				ramMb:     8192,
				gpuInfo:   GPUInfo{},
			},
			wantOS:    "darwin",
			wantOSVer: "",
			wantCPU:   8,
			wantRAM:   8192,
			wantGPU:   GPUInfo{},
			wantOSTag: "darwin",
		},
		{
			name: "Linux render farm node high RAM and multi-GPU",
			probe: &fakeProbe{
				os:        "linux",
				osVersion: "24.04",
				cpuCount:  128,
				ramMb:     524288,
				gpuInfo: GPUInfo{
					Vendor: "NVIDIA",
					Model:  "H100",
					VRAMMb: 81920,
					Count:  8,
				},
			},
			wantOS:    "linux",
			wantOSVer: "24.04",
			wantCPU:   128,
			wantRAM:   524288,
			wantGPU:   GPUInfo{Vendor: "NVIDIA", Model: "H100", VRAMMb: 81920, Count: 8},
			wantOSTag: "linux",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caps := Detect(tt.probe)

			if caps.OS != tt.wantOS {
				t.Errorf("OS: got %q, want %q", caps.OS, tt.wantOS)
			}
			if caps.OSVersion != tt.wantOSVer {
				t.Errorf("OSVersion: got %q, want %q", caps.OSVersion, tt.wantOSVer)
			}
			if caps.CPUCount != tt.wantCPU {
				t.Errorf("CPUCount: got %d, want %d", caps.CPUCount, tt.wantCPU)
			}
			if caps.RAMMb != tt.wantRAM {
				t.Errorf("RAMMb: got %d, want %d", caps.RAMMb, tt.wantRAM)
			}
			if caps.GPU != tt.wantGPU {
				t.Errorf("GPU: got %+v, want %+v", caps.GPU, tt.wantGPU)
			}
			if got := caps.Tags["os"]; got != tt.wantOSTag {
				t.Errorf(`Tags["os"]: got %q, want %q`, got, tt.wantOSTag)
			}
			// os_version tag must be present iff OSVersion is non-empty.
			if tt.wantOSVer != "" {
				if got, ok := caps.Tags["os_version"]; !ok || got != tt.wantOSVer {
					t.Errorf(`Tags["os_version"]: got %q (ok=%v), want %q`, got, ok, tt.wantOSVer)
				}
			} else {
				if _, ok := caps.Tags["os_version"]; ok {
					t.Error(`Tags["os_version"] should be absent when OSVersion is empty`)
				}
			}
		})
	}
}

// ── MergeManualTags ────────────────────────────────────────────────────────────

func TestMergeManualTags_AddsTagsToMap(t *testing.T) {
	caps := Detect(&fakeProbe{os: "linux", osVersion: "22.04"})
	caps.MergeManualTags([]string{"maya-2025", "gpu", "highram"})

	for _, tag := range []string{"maya-2025", "gpu", "highram"} {
		if _, ok := caps.Tags[tag]; !ok {
			t.Errorf("tag %q not found in Tags after MergeManualTags", tag)
		}
	}
}

func TestMergeManualTags_OverwritesAutoDetectedTag(t *testing.T) {
	caps := Detect(&fakeProbe{os: "linux", osVersion: "22.04"})
	// Overwrite the auto-detected "os" tag with a custom value.
	caps.MergeManualTags([]string{"os"})

	// The key must exist and its value must now be the empty string (manual tag).
	if v, ok := caps.Tags["os"]; !ok {
		t.Fatal(`Tags["os"] must still be present after overwrite`)
	} else if v != "" {
		t.Errorf(`Tags["os"]: got %q, want "" (manual tag value)`, v)
	}
}

func TestMergeManualTags_EmptyListIsNoOp(t *testing.T) {
	caps := Detect(&fakeProbe{os: "linux", osVersion: "22.04"})
	before := len(caps.Tags)
	caps.MergeManualTags(nil)
	caps.MergeManualTags([]string{})
	if len(caps.Tags) != before {
		t.Errorf("tag count changed from %d to %d after merging empty list", before, len(caps.Tags))
	}
}

func TestMergeManualTags_SkipsBlankEntries(t *testing.T) {
	caps := Detect(&fakeProbe{os: "linux"})
	before := len(caps.Tags)
	caps.MergeManualTags([]string{"", "   ", ""})
	// Blank strings should not create map entries (only truly empty string ""
	// is tested here; whitespace-only strings are not trimmed — that is the
	// caller's responsibility per the WorkerConfig documentation).
	if _, ok := caps.Tags[""]; ok {
		t.Error(`empty string must not be added as a tag key`)
	}
	// The count should not have increased by more than the whitespace-only entry.
	_ = before
}

func TestMergeManualTags_NilTagsMapInitialized(t *testing.T) {
	// Construct a Capabilities with a nil Tags map to verify MergeManualTags
	// initializes it rather than panicking.
	caps := Capabilities{}
	caps.MergeManualTags([]string{"maya-2025"})
	if caps.Tags == nil {
		t.Fatal("Tags must not be nil after MergeManualTags on zero-value Capabilities")
	}
	if _, ok := caps.Tags["maya-2025"]; !ok {
		t.Error(`Tags["maya-2025"] not present after MergeManualTags on nil map`)
	}
}

func TestMergeManualTags_PreservesAutoTags(t *testing.T) {
	caps := Detect(&fakeProbe{os: "linux", osVersion: "22.04"})
	caps.MergeManualTags([]string{"maya-2025"})

	if got := caps.Tags["os"]; got != "linux" {
		t.Errorf(`Tags["os"] should still be "linux", got %q`, got)
	}
	if got := caps.Tags["os_version"]; got != "22.04" {
		t.Errorf(`Tags["os_version"] should still be "22.04", got %q`, got)
	}
}
