// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// KnownOSes are the GOOS values a known violation may be limited to, and the
// ones a chaos matrix is listed for.
var KnownOSes = []string{"linux", "darwin", "windows"}

// faultKindList names the fault kinds in the error for a fault that sets none
// or several.
var faultKindList = strings.Join(faultKinds[:], ", ")

func (v *validator) faults() {
	if len(v.s.Faults) == 0 {
		return
	}
	if v.target == TargetAttach {
		v.failf("faults", "fault injection needs a managed target")
		return
	}
	groups := map[string]int{}
	for _, g := range v.s.Workers {
		groups[g.Name] = g.Real
	}
	firstServerFault := -1
	for i := range v.s.Faults {
		f := &v.s.Faults[i]
		v.fault(fmt.Sprintf("faults[%d]", i), f, groups)
		if firstServerFault < 0 && f.hasServerFault() {
			firstServerFault = i
		}
	}
	if firstServerFault >= 0 {
		const why = "must be 0 when faults[%d] takes the server down; UI clients do not survive a server outage"
		if v.s.Clients.WebSocket.Count != 0 {
			v.failf("clients.websocket.count", why, firstServerFault)
		}
		if v.s.Clients.RESTPollers.Count != 0 {
			v.failf("clients.rest_pollers.count", why, firstServerFault)
		}
	}
}

func (v *validator) fault(path string, f *Fault, groups map[string]int) {
	kinds := f.kinds()
	if len(kinds) != 1 {
		v.failf(path, "set exactly one of %s", faultKindList)
	}
	v.faultTiming(path, f)
	switch {
	case f.Kill != nil:
		v.procTarget(path+".kill", f.Kill, groups)
	case f.Term != nil:
		v.procTarget(path+".term", f.Term, groups)
	case f.Stall != nil:
		v.procTarget(path+".stall", f.Stall, groups)
	}
	v.faultRestart(path, f)
	v.faultNetwork(path, f, groups)
	v.newKinds(path, f, groups)
	if f.PowerCut != nil {
		v.powerCut(path+".power_cut", f.PowerCut)
	}
}

// faultTiming checks when a fault starts and that it heals within the run.
func (v *validator) faultTiming(path string, f *Fault) {
	if f.At.Duration < 0 {
		v.failf(path+".at", "must not be negative")
	}
	run := v.s.Duration.Duration
	if run <= 0 || f.At.Duration < 0 || f.End() <= run {
		return
	}
	where := path + ".at"
	switch {
	case f.PowerCut != nil:
		where = path + ".power_cut.down"
	case f.Duration.Duration > 0:
		where = path + ".duration"
	case f.RestartAfter.Duration > 0:
		where = path + ".restart_after"
	}
	v.failf(where, "the fault only heals at %s, after the scenario's duration %s", f.End(), v.s.Duration)
}

// faultRestart checks restart_after and duration, which only some kinds use.
func (v *validator) faultRestart(path string, f *Fault) {
	killOrTerm := f.Kill != nil || f.Term != nil
	switch {
	case f.RestartAfter.Duration < 0:
		v.failf(path+".restart_after", "must not be negative")
	case f.RestartAfter.Duration > 0 && !killOrTerm:
		v.failf(path+".restart_after", "applies to kill and term only")
	}
	switch {
	case killOrTerm || f.PowerCut != nil:
		if f.Duration.Duration != 0 {
			v.failf(path+".duration", "does not apply to %s; use restart_after or the power cut's down", f.Kind())
		}
	case f.Duration.Duration <= 0 && f.Kind() != "":
		v.failf(path+".duration", "must be positive for %s", f.Kind())
	}
	for _, p := range []*ProcTarget{f.Kill, f.Term} {
		if p != nil && p.Server && f.RestartAfter.Duration <= 0 {
			v.failf(path+".restart_after", "must be positive: a server that is killed or terminated must be restarted")
		}
	}
}

func (v *validator) procTarget(path string, p *ProcTarget, groups map[string]int) {
	if p.Server == (p.Group != "") {
		v.failf(path, "exactly one of server and group must be set")
		return
	}
	if p.Server {
		if p.Real != 0 {
			v.failf(path+".real", "applies to a group, not the server")
		}
		return
	}
	nReal, ok := groups[p.Group]
	switch {
	case !ok:
		v.failf(path+".group", "%q is not in workers", p.Group)
	case nReal == 0:
		v.failf(path+".group", "%q has no real workers", p.Group)
	case p.Real < 0:
		v.failf(path+".real", "must not be negative")
	case p.Real > nReal:
		v.failf(path+".real", "%d is more than the %d real workers in group %q", p.Real, nReal, p.Group)
	}
}

// faultNetwork checks the kinds that act on the connections.
func (v *validator) faultNetwork(path string, f *Fault, groups map[string]int) {
	kind, names, ok := f.netSpec()
	if !ok {
		return
	}
	v.netGroups(path+"."+kind+".groups", names, groups)
	switch kind {
	case FaultLatency:
		v.latency(path+".latency", f.Latency)
	case FaultFlap:
		if p := f.Flap.Period.Duration; p <= 0 || (f.Duration.Duration > 0 && p > f.Duration.Duration) {
			v.failf(path+".flap.period", "%s must be positive and no longer than duration %s", f.Flap.Period, f.Duration)
		}
	}
}

// maxClockSkew is the largest offset a simulated worker's clock may carry.
const maxClockSkew = 48 * time.Hour

// newKinds checks the database, disk_full, fd_limit and revoke kinds.
func (v *validator) newKinds(path string, f *Fault, groups map[string]int) {
	if f.Database != nil {
		if m := f.Database.Mode; m != DatabaseStall && m != DatabaseDrop {
			v.failf(path+".database.mode", "%q is not one of %s, %s", m, DatabaseStall, DatabaseDrop)
		}
	}
	if f.DiskFull != nil {
		v.diskFull(path+".disk_full", f.DiskFull)
	}
	if f.FDLimit != nil && f.FDLimit.Headroom <= 0 {
		v.failf(path+".fd_limit.headroom", "must be positive")
	}
	if f.Revoke != nil {
		v.revoke(path+".revoke", f.Revoke, groups)
	}
}

// diskMinFailed checks a disk_full fault's min_failed: at least 1, since 0
// is no minimum, and no more than max_failed.
func (v *validator) diskMinFailed(path string, d *DiskFull) {
	switch {
	case d.MinFailed == nil:
	case *d.MinFailed < 1:
		v.failf(path+".min_failed", "must be at least 1")
	case d.MaxFailed != nil && *d.MinFailed > *d.MaxFailed:
		v.failf(path+".min_failed", "%d is more than max_failed (%d)", *d.MinFailed, *d.MaxFailed)
	}
}

func (v *validator) diskFull(path string, d *DiskFull) {
	switch d.Volume {
	case VolumeServer, VolumeWorkers:
	default:
		v.failf(path+".volume", "%q is not one of %s, %s", d.Volume, VolumeServer, VolumeWorkers)
	}
	if d.HeadroomBytes < 0 {
		v.failf(path+".headroom_bytes", "must not be negative")
	}
	if d.Reason != "" {
		if _, err := regexp.Compile(d.Reason); err != nil {
			v.failf(path+".reason", "does not compile: %v", err)
		}
	}
	if d.MaxFailed != nil && *d.MaxFailed < 0 {
		v.failf(path+".max_failed", "must not be negative")
	}
	v.diskMinFailed(path, d)
	if d.Volume != VolumeWorkers {
		if d.Reason != "" {
			v.failf(path+".reason", "applies to a workers volume only")
		}
		if d.MaxFailed != nil {
			v.failf(path+".max_failed", "applies to a workers volume only")
		}
		if d.MinFailed != nil {
			v.failf(path+".min_failed", "applies to a workers volume only")
		}
		return
	}
	if !slices.ContainsFunc(v.s.Workers, func(g WorkerGroup) bool { return g.Real > 0 }) {
		v.failf(path+".volume", "a workers volume needs a group with real workers")
	}
}

func (v *validator) revoke(path string, p *ProcTarget, groups map[string]int) {
	if p.Server {
		v.failf(path, "revoke applies to a group's real workers")
	} else {
		v.procTarget(path, p, groups)
	}
	if !v.s.Server.BrokerAuth {
		v.failf(path, "revoke needs server.broker_auth: with broker auth off there is no credential to revoke")
	}
}

// clockSkew checks a group's clock_skew.
func (v *validator) clockSkew(path string, w *WorkerGroup) {
	skew := w.ClockSkew.Duration
	if skew > maxClockSkew || skew < -maxClockSkew {
		v.failf(path+".clock_skew", "%s is more than %s from the server's clock", w.ClockSkew, maxClockSkew)
	}
	if skew != 0 && w.Real > 0 {
		v.failf(path+".clock_skew", "applies to simulated workers only; group %q has real workers", w.Name)
	}
}

func (v *validator) netGroups(path string, names []string, groups map[string]int) {
	for i, n := range names {
		if _, ok := groups[n]; !ok {
			v.failf(fmt.Sprintf("%s[%d]", path, i), "%q is not in workers", n)
		}
	}
}

func (v *validator) latency(path string, l *LatencySpec) {
	if l.Delay.Duration < 0 || l.Jitter.Duration < 0 {
		v.failf(path, "delay and jitter must not be negative")
	}
	if l.BytesPerSec < 0 {
		v.failf(path+".bytes_per_sec", "must not be negative")
	}
	if l.Delay.Duration <= 0 && l.BytesPerSec <= 0 {
		v.failf(path, "does nothing; set delay or bytes_per_sec")
	}
	if l.Jitter.Duration > l.Delay.Duration {
		v.failf(path+".jitter", "%s must not exceed delay %s", l.Jitter, l.Delay)
	}
}

func (v *validator) powerCut(path string, p *PowerCut) {
	if p.Order != PowerCutServerFirst && p.Order != PowerCutWorkersFirst {
		v.failf(path+".order", "%q is not one of %s, %s", p.Order, PowerCutServerFirst, PowerCutWorkersFirst)
	}
	if p.Down.Duration <= 0 {
		v.failf(path+".down", "must be positive")
	}
	if p.Gap.Duration < 0 {
		v.failf(path+".gap", "must not be negative")
	}
}

// expectBounds checks the expect section's bounds.
func (v *validator) expectBounds(e Expect) {
	if e.MaxReworkAttempts != nil && *e.MaxReworkAttempts < 0 {
		v.failf("expect.max_rework_attempts", "must not be negative")
	}
	if e.MaxReclaims != nil && *e.MaxReclaims < 0 {
		v.failf("expect.max_reclaims", "must not be negative")
	}
}

func (v *validator) expect() {
	e := v.s.Expect
	if v.target == TargetAttach && (e.MaxReworkAttempts != nil || e.MaxReclaims != nil || len(e.KnownViolations) > 0) {
		v.failf("expect", "needs a managed target")
		return
	}
	v.expectBounds(e)
	for i, k := range e.KnownViolations {
		path := fmt.Sprintf("expect.known_violations[%d]", i)
		if k.Check != ReworkCheck && !slices.Contains(CheckNames, k.Check) {
			v.failf(path+".check", "%q is not one of %s, or %q", k.Check, strings.Join(CheckNames, ", "), ReworkCheck)
		}
		switch k.Kind {
		case "", KnownMust, KnownMay:
		default:
			v.failf(path+".kind", "%q is not one of %s, %s", k.Kind, KnownMust, KnownMay)
		}
		v.knownCeiling(path, k)
		if strings.TrimSpace(k.Reason) == "" {
			v.failf(path+".reason", "is required")
		}
		if strings.TrimSpace(k.Tracker) == "" {
			v.failf(path+".tracker", "is required")
		}
		for j, os := range k.OS {
			if !slices.Contains(KnownOSes, os) {
				v.failf(fmt.Sprintf("%s.os[%d]", path, j), "%q is not one of %s", os, strings.Join(KnownOSes, ", "))
			}
		}
	}
}

func (v *validator) realTasks() {
	for i := range v.s.Jobs {
		rt := v.s.Jobs[i].RealTask
		if rt == nil {
			continue
		}
		path := fmt.Sprintf("jobs[%d].real_task", i)
		if v.target == TargetAttach {
			v.failf(path, "needs a managed target")
			continue
		}
		if rt.Duration.Duration <= time.Duration(0) {
			v.failf(path+".duration", "must be positive")
		}
		if rt.LogEvery.Duration < 0 {
			v.failf(path+".log_every", "must not be negative")
		}
		if rt.ScratchBytes < 0 {
			v.failf(path+".scratch_bytes", "must not be negative")
		}
	}
}

// knownCeiling checks an entry's max_attempts: a rework entry carries one
// above the scenario's max_rework_attempts, so the entry never excuses
// unbounded rework, and an entry for any other check carries none.
func (v *validator) knownCeiling(path string, k KnownViolation) {
	if k.Check != ReworkCheck {
		if k.MaxAttempts != nil {
			v.failf(path+".max_attempts", "applies only to a %q entry, not %q", ReworkCheck, k.Check)
		}
		return
	}
	bound := v.s.Expect.MaxReworkAttempts
	switch {
	case bound == nil:
		v.failf("expect.max_rework_attempts", "is required when known_violations names %q, as the bound its ceiling is above", ReworkCheck)
	case k.MaxAttempts == nil:
		v.failf(path+".max_attempts", "is required on a %q entry: the most rework it excuses, above max_rework_attempts (%d)", ReworkCheck, *bound)
	case *k.MaxAttempts <= *bound:
		v.failf(path+".max_attempts", "%d must be greater than max_rework_attempts (%d)", *k.MaxAttempts, *bound)
	}
}
