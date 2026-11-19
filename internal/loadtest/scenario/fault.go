// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"slices"
	"time"
)

// Fault is one disruption injected during the run. Exactly one of Kill, Term,
// Stall, Partition, Blackhole, Latency, Flap, PowerCut, Database, DiskFull,
// FDLimit and Revoke is set.
type Fault struct {
	At Duration `yaml:"at"`
	// Duration is how long a stall, partition, blackhole, latency, flap,
	// database, disk_full, fd_limit or revoke fault lasts before it heals.
	Duration Duration `yaml:"duration"`
	// RestartAfter is how long after a kill or term the process is started
	// again; 0 leaves a worker down. A server must be restarted.
	RestartAfter Duration       `yaml:"restart_after"`
	Kill         *ProcTarget    `yaml:"kill,omitempty"`
	Term         *ProcTarget    `yaml:"term,omitempty"`
	Stall        *ProcTarget    `yaml:"stall,omitempty"`
	Partition    *NetTarget     `yaml:"partition,omitempty"`
	Blackhole    *NetTarget     `yaml:"blackhole,omitempty"`
	Latency      *LatencySpec   `yaml:"latency,omitempty"`
	Flap         *FlapSpec      `yaml:"flap,omitempty"`
	PowerCut     *PowerCut      `yaml:"power_cut,omitempty"`
	Database     *DatabaseFault `yaml:"database,omitempty"`
	DiskFull     *DiskFull      `yaml:"disk_full,omitempty"`
	FDLimit      *FDLimit       `yaml:"fd_limit,omitempty"`
	Revoke       *ProcTarget    `yaml:"revoke,omitempty"` // a group's real workers; server is an error
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

// DatabaseFault stalls the store (SQLite: an outside write lock; PostgreSQL:
// the server's connections stop forwarding) or, on PostgreSQL only, drops the
// server's connections and refuses new ones.
type DatabaseFault struct {
	Mode string `yaml:"mode"` // "stall" or "drop"
}

// DiskFull fills a size-limited volume with ballast for Duration.
type DiskFull struct {
	Volume        string `yaml:"volume"`         // "server" (data directory) or "workers" (real workers' session directories)
	HeadroomBytes int64  `yaml:"headroom_bytes"` // free space left; 0 fills it
	Inodes        bool   `yaml:"inodes"`         // also use up the free inodes
	// Reason, on a workers volume, is an RE2 pattern every attempt that fails
	// on a real worker inside the window must match; "" checks none.
	Reason string `yaml:"reason"`
	// MaxFailed, on a workers volume, bounds the attempts that fail on real
	// workers inside the window; nil is unbounded.
	MaxFailed *int `yaml:"max_failed,omitempty"`
}

// FDLimit lowers sqi-server's soft RLIMIT_NOFILE for Duration so that it can
// open Headroom more files, then restores it. Linux only.
type FDLimit struct {
	Headroom int `yaml:"headroom"`
}

// The database fault modes and disk_full volumes.
const (
	DatabaseStall = "stall"
	DatabaseDrop  = "drop"
	VolumeServer  = "server"
	VolumeWorkers = "workers"
)

// The power cut orders.
const (
	PowerCutServerFirst  = "server-first"
	PowerCutWorkersFirst = "workers-first"
)

// Expect declares what a chaos run is allowed to find.
type Expect struct {
	MaxReworkAttempts *int `yaml:"max_rework_attempts,omitempty"` // nil: report only
	// MaxReclaims bounds the attempts the server took back from a worker (the
	// reclaims check); nil: report only.
	MaxReclaims     *int             `yaml:"max_reclaims,omitempty"`
	KnownViolations []KnownViolation `yaml:"known_violations,omitempty"`
}

// The kinds of known violation.
const (
	KnownMust = "must"
	KnownMay  = "may"
)

// KnownViolation is a finding the scenario expects, recorded against a tracker
// item.
type KnownViolation struct {
	Check string   `yaml:"check"` // an invariant name, or "rework"
	Kind  string   `yaml:"kind"`  // "must" (default) or "may"
	OS    []string `yaml:"os"`    // GOOS values it applies on; empty = every OS
	// MaxAttempts is a rework entry's ceiling: rework above the scenario's
	// max_rework_attempts and at or under this is the defect firing, and more
	// than this fails. Required on a rework entry, an error on any other.
	MaxAttempts *int   `yaml:"max_attempts,omitempty"`
	Reason      string `yaml:"reason"`
	Tracker     string `yaml:"tracker"` // e.g. "H99x"
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
	"reclaims",
	"revoked",
	"skew-warning",
	"attempt-order",
	"disk-full",
}

// RealTask is how the no-op command behaves for this class on a real worker.
// Nil keeps the default no-op (print one line, exit 0).
type RealTask struct {
	Duration    Duration `yaml:"duration"`     // how long the main process runs
	LogEvery    Duration `yaml:"log_every"`    // main process prints a line this often; 0 = once
	ChattyChild bool     `yaml:"chatty_child"` // a child that prints to stdout every 200ms for Duration
	SilentChild bool     `yaml:"silent_child"` // a child with no stdio that writes the output file for Duration
	// ScratchBytes is how many bytes the task writes into its working
	// directory, spread over Duration.
	ScratchBytes int64 `yaml:"scratch_bytes"`
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
	FaultDatabase  = "database"
	FaultDiskFull  = "disk_full"
	FaultFDLimit   = "fd_limit"
	FaultRevoke    = "revoke"
)

// Kind returns the name of the one kind the fault sets (see faultKinds), and
// "" when none or more than one is set.
func (f Fault) Kind() string {
	set := f.kinds()
	if len(set) != 1 {
		return ""
	}
	return set[0]
}

// faultKinds lists the fault kinds in the order kinds reports them.
var faultKinds = [...]string{
	FaultKill, FaultTerm, FaultStall, FaultPartition, FaultBlackhole, FaultLatency, FaultFlap, FaultPowerCut,
	FaultDatabase, FaultDiskFull, FaultFDLimit, FaultRevoke,
}

// kinds lists every kind the fault sets.
func (f Fault) kinds() []string {
	var set []string
	for i, on := range [len(faultKinds)]bool{
		f.Kill != nil, f.Term != nil, f.Stall != nil, f.Partition != nil,
		f.Blackhole != nil, f.Latency != nil, f.Flap != nil, f.PowerCut != nil,
		f.Database != nil, f.DiskFull != nil, f.FDLimit != nil, f.Revoke != nil,
	} {
		if on {
			set = append(set, faultKinds[i])
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

// hasServerFault reports whether the fault takes the server down, stops it, or
// leaves it refusing connections (a full data volume, a lowered file limit).
func (f Fault) hasServerFault() bool {
	return f.PowerCut != nil || f.FDLimit != nil ||
		(f.DiskFull != nil && f.DiskFull.Volume == VolumeServer) ||
		(f.Kill != nil && f.Kill.Server) ||
		(f.Term != nil && f.Term.Server) ||
		(f.Stall != nil && f.Stall.Server)
}

// NetGroups returns the worker groups a network fault (partition, blackhole,
// latency or flap) acts on, empty for every group. ok is false for any other
// kind.
func (f Fault) NetGroups() (groups []string, ok bool) {
	_, groups, ok = f.netSpec()
	return groups, ok
}

// netSpec returns a network fault's kind and groups.
func (f Fault) netSpec() (kind string, groups []string, ok bool) {
	switch {
	case f.Partition != nil:
		return FaultPartition, f.Partition.Groups, true
	case f.Blackhole != nil:
		return FaultBlackhole, f.Blackhole.Groups, true
	case f.Latency != nil:
		return FaultLatency, f.Latency.Groups, true
	case f.Flap != nil:
		return FaultFlap, f.Flap.Groups, true
	}
	return "", nil, false
}

// HasNetworkFault reports whether any fault needs lanes.
func (s *Scenario) HasNetworkFault() bool {
	return slices.ContainsFunc(s.Faults, func(f Fault) bool {
		_, ok := f.NetGroups()
		return ok
	})
}

// HasRealTask reports whether any job class runs a real task.
func (s *Scenario) HasRealTask() bool {
	return slices.ContainsFunc(s.Jobs, func(j JobClass) bool { return j.RealTask != nil })
}

// ChaosEnv is where a chaos scenario would run.
type ChaosEnv struct {
	GOOS      string // runtime.GOOS of the test process
	Store     string // "sqlite" or "postgres"
	Container bool   // size-limited volumes are mounted (make chaos-container)
}

// chaosSkipRules are ChaosSkipReason's rules in the order they apply.
var chaosSkipRules = [...]func(f Fault, env ChaosEnv) string{
	func(f Fault, env ChaosEnv) string {
		if f.Stall != nil && env.GOOS == "windows" {
			return "a stall fault cannot suspend a process on windows"
		}
		return ""
	},
	func(f Fault, env ChaosEnv) string {
		if f.FDLimit != nil && env.GOOS != "linux" {
			return "an fd_limit fault needs linux (RLIMIT_NOFILE), not " + env.GOOS
		}
		return ""
	},
	func(f Fault, env ChaosEnv) string {
		if f.DiskFull != nil && env.GOOS != "linux" {
			return "a disk_full fault needs linux (a Linux container volume), not " + env.GOOS
		}
		return ""
	},
	func(f Fault, env ChaosEnv) string {
		if f.DiskFull != nil && !env.Container {
			return "a disk_full fault needs the size-limited volumes of make chaos-container"
		}
		return ""
	},
	func(f Fault, env ChaosEnv) string {
		if f.Database != nil && f.Database.Mode == DatabaseDrop && env.Store != "postgres" {
			return "a database drop fault needs the postgres store, not " + env.Store
		}
		return ""
	},
}

// ChaosSkipReason says why s cannot run in env, "" when it can. The rules apply
// in this order, each over every fault, and the first to match gives the
// reason: a stall fault on windows; an fd_limit fault off linux; a disk_full
// fault off linux; a disk_full fault without env.Container; a database drop
// without env.Store == "postgres".
func (s *Scenario) ChaosSkipReason(env ChaosEnv) string {
	for _, rule := range chaosSkipRules {
		for _, f := range s.Faults {
			if why := rule(f, env); why != "" {
				return why
			}
		}
	}
	return ""
}
