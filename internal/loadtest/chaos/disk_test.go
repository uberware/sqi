// SPDX-License-Identifier: AGPL-3.0-or-later

package chaos

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/verify"
	"github.com/uberware/sqi/internal/store"
)

func TestDiskFullCheck(t *testing.T) {
	start := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	healed := start.Add(30 * time.Second)
	noSpace := regexp.MustCompile(`(?i)no space left|disk`)
	two := 2
	// A worker that failed inside the window, then succeeded after the heal.
	failedInWindow := func(id, worker, msg string) injected {
		return injected{
			id: id, worker: worker, status: store.AttemptStatusFailed, message: msg,
			created: start.Add(5 * time.Second), started: start.Add(5 * time.Second), ended: new(start.Add(10 * time.Second)),
		}
	}
	recovered := func(id, worker string) injected {
		return injected{
			id: id, worker: worker, created: healed.Add(time.Second), started: healed.Add(2 * time.Second),
			ended: new(healed.Add(5 * time.Second)),
		}
	}
	cases := []struct {
		name     string
		win      DiskWindow
		attempts []injected
		real     []string // the real workers; w1 alone when empty
		wants    []string // evidence the result must name; none passes
		failed   int      // attempts the note must count as failed in the window
	}{
		{
			name: "clean",
			win:  DiskWindow{Start: start, Healed: healed, Reason: noSpace, MaxFailed: &two},
			attempts: []injected{
				failedInWindow("a-f1", "w1", "write failed: no space left on device"),
				recovered("a-ok", "w1"),
			},
			failed: 1,
		},
		{
			name: "no failure in the window passes and says so",
			win:  DiskWindow{Start: start, Healed: healed, Reason: noSpace, MaxFailed: &two},
			attempts: []injected{
				recovered("a-ok", "w1"),
			},
			failed: 0,
		},
		{
			name: "reason mismatch",
			win:  DiskWindow{Start: start, Healed: healed, Reason: noSpace},
			attempts: []injected{
				failedInWindow("a-f1", "w1", "exit code 1"),
				recovered("a-ok", "w1"),
			},
			wants: []string{"attempt a-f1", "task t1", "worker w1", `"exit code 1"`},
		},
		{
			name: "failure after the window is not judged",
			win:  DiskWindow{Start: start, Healed: healed, Reason: noSpace},
			attempts: []injected{
				{
					id: "a-late", worker: "w1", status: store.AttemptStatusFailed, message: "exit code 1",
					created: healed, started: healed, ended: new(healed.Add(11 * time.Second)),
				},
				recovered("a-ok", "w1"),
			},
			failed: 0,
		},
		{
			name: "failure just after the heal is in the window",
			win:  DiskWindow{Start: start, Healed: healed, Reason: noSpace},
			attempts: []injected{
				{
					id: "a-tail", worker: "w1", status: store.AttemptStatusFailed, message: "exit code 1",
					created: healed, started: healed, ended: new(healed.Add(9 * time.Second)),
				},
				recovered("a-ok", "w1"),
			},
			wants: []string{"attempt a-tail"},
		},
		{
			name: "over the bound",
			win:  DiskWindow{Start: start, Healed: healed, MaxFailed: &two},
			attempts: []injected{
				failedInWindow("a-f1", "w1", "x"), failedInWindow("a-f2", "w1", "x"), failedInWindow("a-f3", "w1", "x"),
				recovered("a-ok", "w1"),
			},
			wants: []string{"3 attempts failed in the window, bound 2"},
		},
		{
			name: "under the minimum",
			win:  DiskWindow{Start: start, Healed: healed, MinFailed: &two},
			attempts: []injected{
				failedInWindow("a-f1", "w1", "x"),
				recovered("a-ok", "w1"),
			},
			wants: []string{"1 attempts failed in the window, fewer than the minimum of 2"},
		},
		{
			name: "a fault that made nothing fail is under the minimum",
			win:  DiskWindow{Start: start, Healed: healed, MinFailed: &two},
			attempts: []injected{
				recovered("a-ok", "w1"),
			},
			wants: []string{"0 attempts failed in the window, fewer than the minimum of 2"},
		},
		{
			name: "at the minimum",
			win:  DiskWindow{Start: start, Healed: healed, MinFailed: &two},
			attempts: []injected{
				failedInWindow("a-f1", "w1", "x"), failedInWindow("a-f2", "w1", "x"),
				recovered("a-ok", "w1"),
			},
			failed: 2,
		},
		{
			name: "no reason and no bound judge only the recovery",
			win:  DiskWindow{Start: start, Healed: healed},
			attempts: []injected{
				failedInWindow("a-f1", "w1", "anything"), failedInWindow("a-f2", "w1", "anything"), failedInWindow("a-f3", "w1", "anything"),
				recovered("a-ok", "w1"),
			},
			failed: 3,
		},
		{
			name: "a worker given work after the heal that succeeded none of it",
			win:  DiskWindow{Start: start, Healed: healed},
			attempts: []injected{
				recovered("a-ok", "w1"),
				{
					id: "a-after", worker: "w2", status: store.AttemptStatusFailed, message: "x",
					created: healed.Add(time.Second), started: healed.Add(time.Second), ended: new(healed.Add(20 * time.Second)),
				},
			},
			real:  []string{"w1", "w2"},
			wants: []string{"worker w2 was given work after the disk was freed and succeeded none of it"},
		},
		{
			// The scheduler can hand every task after the heal to the other
			// worker; one given nothing has shown nothing either way.
			name: "a worker given nothing after the heal is not judged",
			win:  DiskWindow{Start: start, Healed: healed},
			attempts: []injected{
				recovered("a-ok", "w1"),
				{id: "a-before", worker: "w2", created: start, started: start, ended: new(start.Add(time.Second))},
			},
			real: []string{"w1", "w2"},
		},
		{
			name: "no real worker succeeded after the heal",
			win:  DiskWindow{Start: start, Healed: healed},
			attempts: []injected{
				{id: "a-before", worker: "w1", created: start, started: start, ended: new(start.Add(time.Second))},
			},
			wants: []string{"no real worker succeeded an attempt started after the disk was freed"},
		},
		{
			name:  "never healed",
			win:   DiskWindow{Start: start},
			wants: []string{"never healed"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newSQLiteStore(t)
			registerWorker(t, ts.st, "w2", start)
			injectAttempts(t, ts.st, tc.attempts...)
			workers := tc.real
			if len(workers) == 0 {
				workers = []string{"w1"}
			}
			results := checkResults(t, ts, StoreInput{RealIDs: workers, DiskFaults: []DiskWindow{tc.win}})
			r := resultOf(t, results, verify.NameDiskFull)
			if len(tc.wants) == 0 {
				wantDiskOK(t, r, tc.failed)
				return
			}
			wantFail(t, r, tc.wants...)
		})
	}
}

func TestDiskFullCheck_OnlyRealWorkersAreJudged(t *testing.T) {
	start := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	healed := start.Add(30 * time.Second)
	ts := newSQLiteStore(t)
	registerWorker(t, ts.st, "w2", start)
	injectAttempts(
		t, ts.st,
		// A simulated worker's failure in the window is none of the check's business.
		injected{
			id: "a-sim", worker: checkWorker, status: store.AttemptStatusFailed, message: "exit code 1",
			created: start, started: start, ended: new(start.Add(time.Second)),
		},
		injected{id: "a-ok", worker: "w2", created: healed, started: healed.Add(time.Second), ended: new(healed.Add(2 * time.Second))},
	)
	bound := 0
	results := checkResults(t, ts, StoreInput{
		RealIDs:    []string{"w2"},
		DiskFaults: []DiskWindow{{Start: start, Healed: healed, Reason: regexp.MustCompile(`space`), MaxFailed: &bound}},
	})
	wantDiskOK(t, resultOf(t, results, verify.NameDiskFull), 0)
}

// wantDiskOK requires r to pass with exactly one note, which counts failed
// attempts in the window.
func wantDiskOK(t *testing.T, r verify.Result, failed int) {
	t.Helper()
	want := fmt.Sprintf("%d attempts failed in the window [", failed)
	if !r.OK || len(r.Evidence) != 1 || !strings.HasPrefix(r.Evidence[0], want) {
		t.Errorf("%s = OK %v, evidence %q; want a pass with the note %q...", r.Name, r.OK, r.Evidence, want)
	}
}

func TestDiskFullCheck_Unmeasured(t *testing.T) {
	ts := newSQLiteStore(t)
	seedSettled(t, ts.st, "j1", "s1", "t1")
	w := DiskWindow{Start: time.Now(), Healed: time.Now()}

	t.Run("real worker IDs unreadable", func(t *testing.T) {
		results := checkResults(t, ts, StoreInput{DiskFaults: []DiskWindow{w}, DiskErr: errors.New("real worker 0 has not started")})
		r := resultOf(t, results, verify.NameDiskFull)
		wantFail(t, r, "real worker 0 has not started")
		if !r.Unmeasured {
			t.Errorf("disk-full = %+v, want unmeasured", r)
		}
	})
	t.Run("no real worker", func(t *testing.T) {
		r := resultOf(t, checkResults(t, newSQLiteStore(t), StoreInput{DiskFaults: []DiskWindow{w}}), verify.NameDiskFull)
		if r.OK || !r.Unmeasured {
			t.Errorf("disk-full = %+v, want failed and unmeasured", r)
		}
	})
}

func TestDiskFullCheck_AbsentWithoutDiskFaults(t *testing.T) {
	ts := newSQLiteStore(t)
	for _, r := range checkResults(t, ts, StoreInput{RealIDs: []string{"w1"}}) {
		if r.Name == verify.NameDiskFull {
			t.Errorf("a disk-full result without disk faults: %+v", r)
		}
	}
}

func TestStoreChecks_ResultsOrder(t *testing.T) {
	ts := newSQLiteStore(t)
	seedSettled(t, ts.st, "j1", "s1", "t1")
	bound := 0
	now := time.Now()
	results := checkResults(t, ts, StoreInput{
		MaxReclaims:  &bound,
		Revoked:      []RevokedWorker{{ID: "w1", At: now}},
		SkewedGroups: map[string][]string{"g": {"x"}},
		ServerLog:    "none.log",
		RealIDs:      []string{"w1"},
		DiskFaults:   []DiskWindow{{Start: now, Healed: now}},
	})
	var got []string
	for _, r := range results {
		got = append(got, r.Name)
	}
	want := []string{
		verify.NameStoreIntegrity, verify.NameExecutionsRecorded, verify.NameAttemptOrder,
		verify.NameRevoked, verify.NameSkewWarning, verify.NameDiskFull, verify.NameReclaims,
	}
	if len(got) != len(want) {
		t.Fatalf("results %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("results %v, want %v", got, want)
		}
	}
}
