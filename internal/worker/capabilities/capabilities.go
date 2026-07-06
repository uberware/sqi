// SPDX-License-Identifier: AGPL-3.0-or-later

// Package capabilities provides auto-detection and manual override of
// sqi-worker host capabilities reported to sqi-server at registration time.
//
// # Design
//
// Hardware queries are abstracted behind the [Probe] interface so tests can
// inject controlled return values without touching real OS APIs.  The default
// production implementation ([DefaultProbe]) calls the real syscalls via
// platform-specific files (probe_linux.go, probe_darwin.go, probe_windows.go).
//
// # Usage
//
//	caps := capabilities.Detect(nil)               // nil → DefaultProbe
//	caps.MergeManualTags(cfg.Worker.CapabilityTags)
//	// caps is now ready to embed in a protocol.RegisterMsg
package capabilities

import (
	"fmt"
	"strings"
)

// GPUInfo describes GPU hardware available on a worker host.
// It mirrors [protocol.GPUInfo] so callers can convert directly; defined here
// to avoid an import cycle between the capabilities and protocol packages.
type GPUInfo struct {
	Vendor string
	Model  string
	// VRAMMb is the total VRAM of the first / primary GPU in mebibytes.
	VRAMMb int
	Count  int
}

// Capabilities holds the merged set of auto-detected and manually overridden
// worker host capabilities.
type Capabilities struct {
	// OS is the operating system name as reported by runtime.GOOS:
	// "linux", "darwin", or "windows".
	OS string

	// OSVersion is the human-readable OS release string, e.g. "22.04" on
	// Ubuntu or "14.5" on macOS.  Empty if detection fails.
	OSVersion string

	// CPUCount is the number of logical (hardware-thread) CPUs available.
	CPUCount int

	// RAMMb is total installed physical RAM in mebibytes.
	// 0 if detection fails.
	RAMMb int

	// GPU describes the GPU(s) installed on the host.  Zero value means no
	// GPU was detected or detection is not supported on this platform.
	GPU GPUInfo

	// Tags holds arbitrary string capability tags merged from auto-detection
	// and any manual overrides supplied via WorkerConfig.Worker.CapabilityTags.
	// The map value is the empty string for presence-only, list-style tags
	// (e.g. "maya-2025") and the right-hand side of the entry for key=value
	// tags (e.g. "maya=true" → Tags["maya"] = "true"). See [MergeManualTags].
	//
	// Auto-detected entries seeded by [Detect]:
	//   "os"         → Capabilities.OS
	//   "os_version" → Capabilities.OSVersion  (omitted when empty)
	Tags map[string]string
}

// Probe abstracts OS-level hardware queries so tests can inject fake values.
// All methods must be safe to call concurrently.
type Probe interface {
	// OS returns the operating system identifier (runtime.GOOS equivalent).
	OS() string
	// OSVersion returns a human-readable OS release string. Returns an empty
	// string when version detection is not supported or fails.
	OSVersion() string
	// CPUCount returns the number of logical CPUs.
	CPUCount() int
	// RAMMb returns total physical RAM in mebibytes. Returns 0 on failure.
	RAMMb() int
	// GPUInfo returns GPU hardware information. Returns a zero GPUInfo when no
	// GPU is detected or detection is not supported.
	GPUInfo() GPUInfo
}

// Detect auto-detects host capabilities using p. If p is nil, DefaultProbe is
// used. The returned Capabilities.Tags map is always non-nil and pre-populated
// with the "os" tag and, when non-empty, the "os_version" tag.
func Detect(p Probe) Capabilities {
	if p == nil {
		p = newDefaultProbe()
	}
	c := Capabilities{
		OS:        p.OS(),
		OSVersion: p.OSVersion(),
		CPUCount:  p.CPUCount(),
		RAMMb:     p.RAMMb(),
		GPU:       p.GPUInfo(),
		Tags:      make(map[string]string),
	}
	c.Tags["os"] = c.OS
	if c.OSVersion != "" {
		c.Tags["os_version"] = c.OSVersion
	}
	return c
}

// ValidateTagKeys returns an error if any two capability tag keys collide
// case-insensitively. Tag keys are the trailing segment of an OpenJD capability
// name (attr.worker.tag.<key>), which the spec defines as case-insensitive, so
// two keys differing only in case are the same capability and would match
// ambiguously. Rejecting them at registration keeps the advertised tag set
// unambiguous.
func ValidateTagKeys(tags map[string]string) error {
	seen := make(map[string]string, len(tags))
	for k := range tags {
		lk := strings.ToLower(k)
		if prev, ok := seen[lk]; ok {
			return fmt.Errorf(
				"capability tag keys %q and %q differ only in case; tag keys are case-insensitive",
				prev, k,
			)
		}
		seen[lk] = k
	}
	return nil
}

// MergeManualTags merges manual capability tags from
// WorkerConfig.Worker.CapabilityTags into c.Tags. Each entry is either:
//
//   - a bare tag, stored as a map key with an empty value, signaling presence
//     (e.g. "maya-2025", "gpu", "highram"); or
//   - a "key=value" pair, split on the first "=" (e.g. "maya=true" sets
//     Tags["maya"] = "true"; a value may itself contain "=", e.g. "a=b=c"
//     sets Tags["a"] = "b=c"; a trailing "=" with nothing after it, e.g.
//     "maya=", sets Tags["maya"] = "", same as the presence-only form).
//
// An entry with an empty key (starting with "=", e.g. "=true") is skipped
// rather than stored verbatim, since an empty tag key can never be matched
// by an OpenJD attr.worker.tag.<key> requirement.
//
// Manual tags always overwrite any auto-detected tag with the same key, so
// operators can suppress or replace an auto-detected value if needed.
func (c *Capabilities) MergeManualTags(tags []string) {
	if c.Tags == nil {
		c.Tags = make(map[string]string)
	}
	for _, t := range tags {
		if t == "" {
			continue
		}
		if key, value, found := strings.Cut(t, "="); found {
			if key == "" {
				continue
			}
			c.Tags[key] = value
			continue
		}
		c.Tags[t] = ""
	}
}
