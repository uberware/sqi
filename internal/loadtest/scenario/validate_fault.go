// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// knownOSes are the GOOS values a known violation may be limited to.
var knownOSes = []string{"linux", "darwin", "windows"}

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
	if killOrTerm || f.PowerCut != nil {
		if f.Duration.Duration != 0 {
			v.failf(path+".duration", "does not apply to %s; use restart_after or the power cut's down", f.Kind())
		}
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
	switch {
	case f.Stall != nil, f.Partition != nil, f.Blackhole != nil, f.Latency != nil, f.Flap != nil:
		if f.Duration.Duration <= 0 {
			v.failf(path+".duration", "must be positive for %s", f.Kind())
		}
	}
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

func (v *validator) expect() {
	e := v.s.Expect
	if v.target == TargetAttach && (e.MaxReworkAttempts != nil || len(e.KnownViolations) > 0) {
		v.failf("expect", "needs a managed target")
		return
	}
	if e.MaxReworkAttempts != nil && *e.MaxReworkAttempts < 0 {
		v.failf("expect.max_rework_attempts", "must not be negative")
	}
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
			if !slices.Contains(knownOSes, os) {
				v.failf(fmt.Sprintf("%s.os[%d]", path, j), "%q is not one of %s", os, strings.Join(knownOSes, ", "))
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
