// SPDX-License-Identifier: AGPL-3.0-or-later

package chaos

import (
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// diskTail is how long after the disk is freed an attempt may still end in
// the window: a task that began writing before the heal can fail after it.
const diskTail = 10 * time.Second

// DiskWindow is one workers-volume disk_full fault that fired.
type DiskWindow struct {
	Start time.Time
	// Healed is zero when the volume was never freed.
	Healed time.Time
	// Reason, when set, is what every attempt failing inside the window must
	// match; nil checks none.
	Reason *regexp.Regexp
	// MinFailed, when set, is the fewest attempts that must fail inside the
	// window: fewer means the fault did not do what the scenario injects it
	// for.
	MinFailed *int
	// MaxFailed, when set, bounds the attempts that fail inside the window.
	MaxFailed *int
}

// diskViolations checks one disk_full window over the attempts of the real
// workers: each failure inside it matches the reason, the failures number
// within the bounds, some real worker succeeded an attempt that started after
// the heal, and every real worker given an attempt after the heal succeeded
// one. A worker given nothing after the heal is not judged: the scheduler can
// hand every task then to another worker. The note says how many attempts
// failed inside the window, so a pass shows whether the fault caused any
// failure; it is empty when the window was not judged.
func diskViolations(w DiskWindow, realIDs []string, attempts []store.TaskAttempt) (violations []string, note string) {
	if w.Healed.IsZero() {
		return []string{fmt.Sprintf("the disk_full fault at %s never healed, so the workers' recovery was not judged", isoStamp(w.Start))}, ""
	}
	var out []string
	end := w.Healed.Add(diskTail)
	failed := 0
	for _, a := range attempts {
		if !slices.Contains(realIDs, a.WorkerID) {
			continue
		}
		if failedIn(a, w.Start, end) {
			failed++
			if w.Reason != nil && !w.Reason.MatchString(a.Message) {
				out = append(out, fmt.Sprintf("attempt %s of task %s on worker %s failed with %q, which does not match %s",
					a.ID, a.TaskID, a.WorkerID, a.Message, w.Reason))
			}
		}
	}
	if w.MinFailed != nil && failed < *w.MinFailed {
		out = append(out, fmt.Sprintf("%d attempts failed in the window, fewer than the minimum of %d: the fault did not make the tasks fail", failed, *w.MinFailed))
	}
	if w.MaxFailed != nil && failed > *w.MaxFailed {
		out = append(out, fmt.Sprintf("%d attempts failed in the window, bound %d", failed, *w.MaxFailed))
	}
	out = append(out, recoveryViolations(w.Healed, realIDs, attempts)...)
	note = fmt.Sprintf("%d attempts failed in the window [%s, %s]", failed, isoStamp(w.Start), isoStamp(end))
	return out, note
}

// recoveryViolations checks that the real workers went on after the disk was
// freed at healed: some real worker succeeded an attempt that started after
// it, and every real worker given an attempt after it succeeded one.
func recoveryViolations(healed time.Time, realIDs []string, attempts []store.TaskAttempt) []string {
	recovered := map[string]bool{}
	givenWork := map[string]bool{}
	for _, a := range attempts {
		if !slices.Contains(realIDs, a.WorkerID) {
			continue
		}
		if a.CreatedAt.After(healed) {
			givenWork[a.WorkerID] = true
		}
		if a.Status == store.AttemptStatusSucceeded && a.StartedAt.After(healed) {
			recovered[a.WorkerID] = true
		}
	}
	var out []string
	if len(recovered) == 0 {
		out = append(out, "no real worker succeeded an attempt started after the disk was freed")
	}
	for _, id := range realIDs {
		if givenWork[id] && !recovered[id] {
			out = append(out, fmt.Sprintf("worker %s was given work after the disk was freed and succeeded none of it", id))
		}
	}
	return out
}

// failedIn reports whether a failed attempt ended in [start, end].
func failedIn(a store.TaskAttempt, start, end time.Time) bool {
	return a.Status == store.AttemptStatusFailed && a.EndedAt != nil && !a.EndedAt.Before(start) && !a.EndedAt.After(end)
}
