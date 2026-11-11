// SPDX-License-Identifier: AGPL-3.0-or-later

package chaos

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/loadtest/marker"
	"github.com/uberware/sqi/internal/loadtest/target"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

// The marker checks' names, as verify.Names and scenario.CheckNames list them.
const (
	NameNoOrphans    = "no-orphans"
	NameSingleWriter = "single-writer"
)

// OrphanGrace is how long a process may outlive the worker that started it
// before no-orphans counts it: the worker's own shutdown and the operating
// system's cleanup take a moment.
const OrphanGrace = 10 * time.Second

// MarkerChecks returns [no-orphans, single-writer], from what the real tasks'
// processes wrote. It is called with the simulated fleet and the real workers
// still up, after the drain.
//
// no-orphans: no process was seen alive more than OrphanGrace after the
// incarnation that started it ended. An incarnation still running is not
// judged. skipped, the exec files the read could not parse, is noted in the
// evidence and fails nothing.
//
// single-writer: per task key, no two execs wrote the key's output over
// overlapping spans of time, the span being from an exec's first line to its
// last.
func MarkerChecks(execs []marker.Exec, skipped int, outputs map[string][]marker.OutputLine, incs []target.Incarnation) []verify.Result {
	return []verify.Result{
		noOrphans(execs, skipped, incs),
		singleWriter(outputs),
	}
}

func noOrphans(execs []marker.Exec, skipped int, incs []target.Incarnation) verify.Result {
	ended := map[string]time.Time{}
	for _, inc := range incs {
		if !inc.Ended.IsZero() {
			ended[inc.ID] = inc.Ended
		}
	}
	ordered := slices.SortedFunc(slices.Values(execs), func(a, b marker.Exec) int {
		if c := cmp.Compare(a.Incarnation, b.Incarnation); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	ev := &evidence{}
	for _, e := range ordered {
		end, ok := ended[e.Incarnation]
		if !ok {
			continue
		}
		if late := e.LastSeen.Sub(end); late > OrphanGrace {
			ev.add(fmt.Sprintf("exec %s (%s, task %s, pid %d) of incarnation %s was last seen %s after its worker ended (grace %s)",
				e.ID, e.Role, e.Key, e.PID, e.Incarnation, late.Round(time.Millisecond), OrphanGrace))
		}
	}
	if skipped > 0 {
		ev.note(fmt.Sprintf("%d marker files unreadable", skipped))
	}
	return ev.result(NameNoOrphans)
}

// span is the interval from one exec's first line of a key's output to its
// last.
type span struct {
	execID      string
	first, last time.Time
}

func singleWriter(outputs map[string][]marker.OutputLine) verify.Result {
	ev := &evidence{}
	for _, key := range slices.Sorted(maps.Keys(outputs)) {
		spans := writerSpans(outputs[key])
		var open span // the span reaching furthest so far
		for i, s := range spans {
			if i > 0 && !s.first.After(open.last) {
				ev.add(fmt.Sprintf("task %s: execs %s [%s, %s] and %s [%s, %s] wrote its output at the same time",
					key, open.execID, stamp(open.first), stamp(open.last), s.execID, stamp(s.first), stamp(s.last)))
			}
			if i == 0 || s.last.After(open.last) {
				open = s
			}
		}
	}
	return ev.result(NameSingleWriter)
}

// writerSpans groups lines by exec and returns each exec's span, earliest
// first.
func writerSpans(lines []marker.OutputLine) []span {
	byExec := map[string]*span{}
	for _, l := range lines {
		s, ok := byExec[l.ExecID]
		if !ok {
			byExec[l.ExecID] = &span{execID: l.ExecID, first: l.At, last: l.At}
			continue
		}
		if l.At.Before(s.first) {
			s.first = l.At
		}
		if l.At.After(s.last) {
			s.last = l.At
		}
	}
	out := make([]span, 0, len(byExec))
	for _, s := range byExec {
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, b span) int {
		if c := a.first.Compare(b.first); c != 0 {
			return c
		}
		return cmp.Compare(a.execID, b.execID)
	})
	return out
}

func stamp(t time.Time) string { return t.UTC().Format("15:04:05.000") }
