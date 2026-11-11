// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import "time"

// Fault is one disruption injected during the run. Exactly one of Kill, Term,
// Stall, Partition, Blackhole, Latency, Flap and PowerCut is set.
type Fault struct {
	At Duration `yaml:"at"`
	// Duration is how long a stall, partition, blackhole, latency or flap lasts
	// before it heals.
	Duration Duration `yaml:"duration"`
	// RestartAfter is how long after a kill or term the process is started
	// again; 0 leaves a worker down. A server must be restarted.
	RestartAfter Duration     `yaml:"restart_after"`
	Kill         *ProcTarget  `yaml:"kill,omitempty"`
	Term         *ProcTarget  `yaml:"term,omitempty"`
	Stall        *ProcTarget  `yaml:"stall,omitempty"`
	Partition    *NetTarget   `yaml:"partition,omitempty"`
	Blackhole    *NetTarget   `yaml:"blackhole,omitempty"`
	Latency      *LatencySpec `yaml:"latency,omitempty"`
	Flap         *FlapSpec    `yaml:"flap,omitempty"`
	PowerCut     *PowerCut    `yaml:"power_cut,omitempty"`
}

// ProcTarget names the server or some of a group's real workers.
type ProcTarget struct {
	Server bool   `yaml:"server"`
	Group  string `yaml:"group"`
	Real   int    `yaml:"real"` // how many of the group's real workers; 0 = all of them
}

// NetTarget names the worker groups whose connection to the server is hit.
type NetTarget struct {
	Groups []string `yaml:"groups"` // empty = every group
}

// LatencySpec slows the connections of the named groups.
type LatencySpec struct {
	Groups      []string `yaml:"groups"`
	Delay       Duration `yaml:"delay"`
	Jitter      Duration `yaml:"jitter"`
	BytesPerSec int      `yaml:"bytes_per_sec"` // 0 = unlimited
}

// FlapSpec cuts and restores the connections of the named groups in turn.
type FlapSpec struct {
	Groups []string `yaml:"groups"`
	Period Duration `yaml:"period"` // cut for Period/2, open for Period/2
}

// PowerCut takes the server and the real workers down together.
type PowerCut struct {
	Order string   `yaml:"order"` // "server-first" or "workers-first"
	Down  Duration `yaml:"down"`  // everything down this long
	Gap   Duration `yaml:"gap"`   // between the first and second side coming back
}

// The power cut orders.
const (
	PowerCutServerFirst  = "server-first"
	PowerCutWorkersFirst = "workers-first"
)

// Expect declares what a chaos run is allowed to find.
type Expect struct {
	MaxReworkAttempts *int             `yaml:"max_rework_attempts,omitempty"` // nil: report only
	KnownViolations   []KnownViolation `yaml:"known_violations,omitempty"`
}

// The kinds of known violation.
const (
	KnownMust = "must"
	KnownMay  = "may"
)

// KnownViolation is a finding the scenario expects, recorded against a tracker
// item.
type KnownViolation struct {
	Check   string   `yaml:"check"` // an invariant name, or "rework"
	Kind    string   `yaml:"kind"`  // "must" (default) or "may"
	OS      []string `yaml:"os"`    // GOOS values it applies on; empty = every OS
	Reason  string   `yaml:"reason"`
	Tracker string   `yaml:"tracker"` // e.g. "H13f1"
}

// ReworkCheck names the rework budget in a KnownViolation.
const ReworkCheck = "rework"

// CheckNames lists the invariant checks a known violation may name. The verify
// package cannot be imported here, so a test there keeps this list equal to
// verify.Names().
var CheckNames = []string{
	"terminal",
	"outcomes",
	"single-attempt",
	"logs",
	"workers-converge",
	"caps",
	"submission-atomic",
	"executions-recorded",
	"store-integrity",
	"no-orphans",
	"single-writer",
}

// RealTask is how the no-op command behaves for this class on a real worker.
// Nil keeps today's no-op (print one line, exit 0).
type RealTask struct {
	Duration    Duration `yaml:"duration"`     // how long the main process runs
	LogEvery    Duration `yaml:"log_every"`    // main process prints a line this often; 0 = once
	ChattyChild bool     `yaml:"chatty_child"` // a child that prints to stdout every 200ms for Duration
	SilentChild bool     `yaml:"silent_child"` // a child with no stdio that writes the output file for Duration
}

// The fault kinds.
const (
	FaultKill      = "kill"
	FaultTerm      = "term"
	FaultStall     = "stall"
	FaultPartition = "partition"
	FaultBlackhole = "blackhole"
	FaultLatency   = "latency"
	FaultFlap      = "flap"
	FaultPowerCut  = "power_cut"
)

// Kind returns "kill", "term", "stall", "partition", "blackhole", "latency",
// "flap" or "power_cut", and "" when none or more than one is set.
func (f Fault) Kind() string {
	set := f.kinds()
	if len(set) != 1 {
		return ""
	}
	return set[0]
}

// kinds lists every kind the fault sets.
func (f Fault) kinds() []string {
	var set []string
	for _, k := range []struct {
		name string
		set  bool
	}{
		{FaultKill, f.Kill != nil},
		{FaultTerm, f.Term != nil},
		{FaultStall, f.Stall != nil},
		{FaultPartition, f.Partition != nil},
		{FaultBlackhole, f.Blackhole != nil},
		{FaultLatency, f.Latency != nil},
		{FaultFlap, f.Flap != nil},
		{FaultPowerCut, f.PowerCut != nil},
	} {
		if k.set {
			set = append(set, k.name)
		}
	}
	return set
}

// End is At plus everything the fault does before it has healed: Duration,
// RestartAfter, and for a power cut Down plus Gap.
func (f Fault) End() time.Duration {
	end := f.At.Duration + f.Duration.Duration + f.RestartAfter.Duration
	if f.PowerCut != nil {
		end += f.PowerCut.Down.Duration + f.PowerCut.Gap.Duration
	}
	return end
}

// hasServerFault reports whether the fault takes the server down or stops it.
func (f Fault) hasServerFault() bool {
	return f.PowerCut != nil ||
		(f.Kill != nil && f.Kill.Server) ||
		(f.Term != nil && f.Term.Server) ||
		(f.Stall != nil && f.Stall.Server)
}

// HasNetworkFault reports whether any fault needs lanes.
func (s *Scenario) HasNetworkFault() bool {
	for _, f := range s.Faults {
		if f.Partition != nil || f.Blackhole != nil || f.Latency != nil || f.Flap != nil {
			return true
		}
	}
	return false
}
