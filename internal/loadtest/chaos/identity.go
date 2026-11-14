// SPDX-License-Identifier: AGPL-3.0-or-later

package chaos

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/loadtest/verify"

	"github.com/uberware/sqi/internal/store"
)

const (
	// orderTolerance is how far apart two timestamps of an attempt may be out
	// of order: the worker's and the server's clocks stamp them.
	orderTolerance = time.Second
	// revokeGrace is how long after a revoke a worker may still heartbeat or be
	// given an attempt before the check names it: the revoke reaches the broker
	// and the server's own bookkeeping asynchronously.
	revokeGrace = 5 * time.Second
	// skewWarning is the scheduler's message for a heartbeat whose worker
	// timestamp is more than heartbeat_timeout from its receipt.
	skewWarning = "scheduler: worker heartbeat delayed or skewed"
	// maxLogLine is the longest server log line the skew-warning check reads.
	maxLogLine = 16 << 20
)

// RevokedWorker is a real worker whose credential a revoke fault removed, and
// when.
type RevokedWorker struct {
	ID string
	At time.Time
}

// identityChecks runs attempt-order, then the revoked, skew-warning and
// disk-full checks the input asks for, over the farm's attempts.
func (f *farmView) identityChecks(ctx context.Context, in StoreInput) []verify.Result {
	var order, revoked, skew, disk verify.Evidence
	attempts, err := f.allAttempts(ctx)
	if err != nil {
		order.AddUnmeasured(err.Error())
		revoked.AddUnmeasured(err.Error())
		disk.AddUnmeasured(err.Error())
	} else {
		order.AddAll(attemptOrder(attempts))
		v, u := revokedViolations(ctx, f.st, in.Revoked, attempts)
		revoked.AddAll(v)
		for _, s := range u {
			revoked.AddUnmeasured(s)
		}
		diskCheck(&disk, in, attempts)
	}
	out := []verify.Result{order.Result(verify.NameAttemptOrder)}
	if len(in.Revoked) > 0 {
		out = append(out, revoked.Result(verify.NameRevoked))
	}
	if len(in.SkewedGroups) > 0 {
		v, u := skewViolations(in.SkewedGroups, in.ServerLog)
		skew.AddAll(v)
		if u != "" {
			skew.AddUnmeasured(u)
		}
		out = append(out, skew.Result(verify.NameSkewWarning))
	}
	if len(in.DiskFaults) > 0 || in.DiskErr != nil {
		out = append(out, disk.Result(verify.NameDiskFull))
	}
	return out
}

// diskCheck adds the disk-full violations of every window to ev.
func diskCheck(ev *verify.Evidence, in StoreInput, attempts []store.TaskAttempt) {
	switch {
	case in.DiskErr != nil:
		ev.AddUnmeasured(in.DiskErr.Error())
	case len(in.DiskFaults) > 0 && len(in.RealIDs) == 0:
		ev.AddUnmeasured("the run has no real worker to judge")
	default:
		for _, w := range in.DiskFaults {
			v, note := diskViolations(w, in.RealIDs, attempts)
			ev.AddAll(v)
			if note != "" {
				ev.Note(note)
			}
		}
	}
}

// allAttempts reads every attempt of the farm's tasks.
func (f *farmView) allAttempts(ctx context.Context) ([]store.TaskAttempt, error) {
	var out []store.TaskAttempt
	for _, j := range f.jobs {
		for _, t := range f.tasks[j.ID] {
			a, err := f.taskAttempts(ctx, t.ID)
			if err != nil {
				return nil, fmt.Errorf("list the attempts of task %s: %w", t.ID, err)
			}
			out = append(out, a...)
		}
	}
	return out, nil
}

// attemptOrder checks that every attempt's timestamps are in order: created,
// then started, then ended, each within orderTolerance.
func attemptOrder(attempts []store.TaskAttempt) []string {
	var out []string
	for _, a := range attempts {
		if a.StartedAt.Before(a.CreatedAt.Add(-orderTolerance)) {
			out = append(out, fmt.Sprintf("attempt %s of task %s on worker %s: started_at %s before created_at %s",
				a.ID, a.TaskID, a.WorkerID, isoStamp(a.StartedAt), isoStamp(a.CreatedAt)))
		}
		if a.EndedAt != nil && a.EndedAt.Before(a.StartedAt.Add(-orderTolerance)) {
			out = append(out, fmt.Sprintf("attempt %s of task %s on worker %s: ended_at %s before started_at %s",
				a.ID, a.TaskID, a.WorkerID, isoStamp(*a.EndedAt), isoStamp(a.StartedAt)))
		}
	}
	return out
}

func isoStamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// revokedViolations checks that no revoked worker heartbeat, or was given an
// attempt, more than revokeGrace after its credential was revoked.
func revokedViolations(ctx context.Context, st store.Store, revoked []RevokedWorker, attempts []store.TaskAttempt) (violations, unmeasured []string) {
	for _, r := range revoked {
		limit := r.At.Add(revokeGrace)
		w, err := st.GetWorker(ctx, r.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			unmeasured = append(unmeasured, fmt.Sprintf("read revoked worker %s: %v", r.ID, err))
		case w.LastHeartbeatAt != nil && w.LastHeartbeatAt.After(limit):
			violations = append(violations, fmt.Sprintf("worker %s heartbeat at %s, %s after its credential was revoked at %s",
				r.ID, isoStamp(*w.LastHeartbeatAt), w.LastHeartbeatAt.Sub(r.At).Round(time.Millisecond), isoStamp(r.At)))
		}
		for _, a := range attempts {
			if a.WorkerID == r.ID && a.CreatedAt.After(limit) {
				violations = append(violations, fmt.Sprintf("attempt %s of task %s on worker %s created at %s, %s after its credential was revoked at %s",
					a.ID, a.TaskID, a.WorkerID, isoStamp(a.CreatedAt), a.CreatedAt.Sub(r.At).Round(time.Millisecond), isoStamp(r.At)))
			}
		}
	}
	return violations, unmeasured
}

// logWorkerID extracts the worker_id attribute of a server log line, in the
// text form (worker_id=x) or the JSON form ("worker_id":"x").
var logWorkerID = regexp.MustCompile(`worker_id(?:=|"\s*:\s*")"?([^\s",}]+)`)

// skewWarned returns the IDs of the workers the server log holds a skew
// warning for.
func skewWarned(path string) (map[string]bool, error) {
	if path == "" {
		return nil, errors.New("the target has no server log")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, maxLogLine)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, skewWarning) {
			continue
		}
		if m := logWorkerID.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	return out, sc.Err()
}

// skewViolations checks that the server warned about at least one worker of
// every group whose clock was skewed.
func skewViolations(groups map[string][]string, logPath string) (violations []string, unmeasured string) {
	warned, err := skewWarned(logPath)
	if err != nil {
		return nil, fmt.Sprintf("read the server log %q: %v", logPath, err)
	}
	for _, g := range slices.Sorted(maps.Keys(groups)) {
		if !slices.ContainsFunc(groups[g], func(id string) bool { return warned[id] }) {
			violations = append(violations, "no skew warning for group "+g)
		}
	}
	return violations, ""
}
