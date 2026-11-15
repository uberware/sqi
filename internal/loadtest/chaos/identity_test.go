// SPDX-License-Identifier: AGPL-3.0-or-later

package chaos

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/verify"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/store/storetest"
)

// injected is an attempt of task t1 to inject: the zero fields are filled in
// by injectAttempts.
type injected struct {
	id, worker string
	status     store.AttemptStatus
	message    string
	created    time.Time
	started    time.Time
	ended      *time.Time
}

// injectAttempts seeds a settled job and injects each attempt of task t1.
func injectAttempts(t *testing.T, st store.Store, attempts ...injected) {
	t.Helper()
	seedSettled(t, st, "j1", "s1", "t1")
	for i, a := range attempts {
		if a.worker == "" {
			a.worker = checkWorker
		}
		if a.status == "" {
			a.status = store.AttemptStatusSucceeded
		}
		if _, err := storetest.InjectorFor(t, st).InjectTaskAttempt(t.Context(), store.TaskAttempt{
			ID: a.id, TaskID: "t1", WorkerID: a.worker, AttemptNumber: i + 1, Status: a.status, Message: a.message,
			CreatedAt: a.created, StartedAt: a.started, EndedAt: a.ended,
		}); err != nil {
			t.Fatalf("InjectTaskAttempt %s: %v", a.id, err)
		}
	}
}

// checkResults runs the checks on ts, stopped, and returns every result.
func checkResults(t *testing.T, ts testStore, in StoreInput) []verify.Result {
	t.Helper()
	ts.stop(t)
	if in.Options.Driver == "" && in.Options.SQLitePath == "" && in.Options.PostgresDSN == "" {
		in.Options = ts.opts
	}
	in.FarmID = checkFarm
	return StoreChecks(t.Context(), in).Results
}

func resultOf(t *testing.T, results []verify.Result, name string) verify.Result {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no %s result in %+v", name, results)
	return verify.Result{}
}

func TestAttemptOrder(t *testing.T) {
	base := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		a     injected
		wants []string // evidence the result must name; none passes
	}{
		{name: "ordered", a: injected{id: "a1", created: base, started: base.Add(time.Second), ended: new(base.Add(2 * time.Second))}},
		{name: "started within a second before created", a: injected{id: "a1", created: base, started: base.Add(-900 * time.Millisecond), ended: new(base.Add(time.Second))}},
		{name: "ended within a second before started", a: injected{id: "a1", created: base, started: base.Add(5 * time.Second), ended: new(base.Add(4500 * time.Millisecond))}},
		{name: "still running", a: injected{id: "a1", status: store.AttemptStatusRunning, created: base, started: base.Add(time.Second)}},
		{
			name:  "started before created",
			a:     injected{id: "a1", created: base, started: base.Add(-2 * time.Second), ended: new(base.Add(time.Second))},
			wants: []string{"attempt a1 of task t1 on worker " + checkWorker, "started_at", "before created_at"},
		},
		{
			name:  "ended before started",
			a:     injected{id: "a1", created: base, started: base.Add(5 * time.Second), ended: new(base.Add(3 * time.Second))},
			wants: []string{"attempt a1 of task t1 on worker " + checkWorker, "ended_at", "before started_at"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newSQLiteStore(t)
			injectAttempts(t, ts.st, tc.a)
			r := resultOf(t, checkResults(t, ts, StoreInput{}), verify.NameAttemptOrder)
			if len(tc.wants) == 0 {
				wantOK(t, r)
				return
			}
			wantFail(t, r, tc.wants...)
			if len(r.Evidence) != 1 {
				t.Errorf("evidence = %q, want one entry", r.Evidence)
			}
		})
	}
}

func TestAttemptOrder_ResultsOrder(t *testing.T) {
	ts := newSQLiteStore(t)
	seedSettled(t, ts.st, "j1", "s1", "t1")
	results := checkResults(t, ts, StoreInput{})
	var names []string
	for _, r := range results {
		names = append(names, r.Name)
	}
	want := verify.NameStoreIntegrity + "," + verify.NameExecutionsRecorded + "," + verify.NameAttemptOrder
	if got := strings.Join(names, ","); got != want {
		t.Errorf("results %s, want %s", got, want)
	}
}

// registerWorker registers a worker whose last heartbeat is beat.
func registerWorker(t *testing.T, st store.Store, id string, beat time.Time) {
	t.Helper()
	if _, _, err := st.RegisterWorker(t.Context(), store.Worker{
		ID: id, FarmID: checkFarm, Hostname: id, Status: store.WorkerStatusOnline, CPUCount: 4, LastHeartbeatAt: &beat,
	}); err != nil {
		t.Fatalf("RegisterWorker %s: %v", id, err)
	}
}

func TestRevokedCheck(t *testing.T) {
	revoked := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		beat     time.Time
		attempts []injected
		wants    []string
	}{
		{name: "heartbeat 3 s after", beat: revoked.Add(3 * time.Second)},
		{
			name: "attempt created before the revoke",
			beat: revoked.Add(-time.Second),
			attempts: []injected{{
				id: "a1", worker: "w-rev", created: revoked.Add(-time.Minute), started: revoked.Add(-time.Minute),
				ended: new(revoked.Add(10 * time.Second)),
			}},
		},
		{name: "heartbeat 10 s after", beat: revoked.Add(10 * time.Second), wants: []string{"worker w-rev", "heartbeat"}},
		{
			name: "attempt created 20 s after",
			beat: revoked.Add(time.Second),
			attempts: []injected{{
				id: "a-late", worker: "w-rev", created: revoked.Add(20 * time.Second), started: revoked.Add(21 * time.Second),
				ended: new(revoked.Add(22 * time.Second)),
			}},
			wants: []string{"attempt a-late", "task t1", "worker w-rev"},
		},
		{
			name: "another worker's attempt after the revoke",
			beat: revoked.Add(time.Second),
			attempts: []injected{{
				id: "a-other", created: revoked.Add(20 * time.Second), started: revoked.Add(21 * time.Second),
				ended: new(revoked.Add(22 * time.Second)),
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newSQLiteStore(t)
			registerWorker(t, ts.st, "w-rev", tc.beat)
			injectAttempts(t, ts.st, tc.attempts...)
			results := checkResults(t, ts, StoreInput{Revoked: []RevokedWorker{{ID: "w-rev", At: revoked}}})
			r := resultOf(t, results, verify.NameRevoked)
			if len(tc.wants) == 0 {
				wantOK(t, r)
				return
			}
			wantFail(t, r, tc.wants...)
		})
	}
}

func TestRevokedCheck_AbsentWithoutRevokedWorkers(t *testing.T) {
	ts := newSQLiteStore(t)
	for _, r := range checkResults(t, ts, StoreInput{}) {
		if r.Name == verify.NameRevoked {
			t.Errorf("a revoked result without revoked workers: %+v", r)
		}
	}
}

func TestRevokedCheck_UnmeasuredWhenTheStoreCannotBeOpened(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.db")
	results := checkResults(t, testStore{}, StoreInput{
		Options: backend.Options{Driver: backend.DriverSQLite, SQLitePath: missing},
		Revoked: []RevokedWorker{{ID: "w-rev", At: time.Now()}},
	})
	for _, name := range []string{verify.NameAttemptOrder, verify.NameRevoked} {
		if r := resultOf(t, results, name); r.OK || !r.Unmeasured {
			t.Errorf("%s = %+v, want failed and unmeasured", name, r)
		}
	}
}

func TestSkewWarning(t *testing.T) {
	log := filepath.Join(t.TempDir(), "server.log")
	lines := []string{
		`time=2026-11-12T09:00:00Z level=INFO msg="scheduler: tick" worker_id=sim-b1`,
		// A different message about worker sim-b1 does not count.
		`time=2026-11-12T09:00:01Z level=WARN msg="scheduler: worker heartbeat lists more attempts than a worker can hold" worker_id=sim-b1`,
		`time=2026-11-12T09:00:02Z level=WARN msg="scheduler: worker heartbeat delayed or skewed: more than heartbeat_timeout" worker_id=sim-a2 worker_at=2026-11-12T08:00:00Z`,
		// An ID that only starts with a group's ID is another worker.
		`time=2026-11-12T09:00:03Z level=WARN msg="scheduler: worker heartbeat delayed or skewed: more" worker_id=sim-b10`,
	}
	if err := os.WriteFile(log, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	groups := map[string][]string{"rack-a": {"sim-a1", "sim-a2"}, "rack-b": {"sim-b1"}}
	ts := newSQLiteStore(t)
	r := resultOf(t, checkResults(t, ts, StoreInput{SkewedGroups: groups, ServerLog: log}), verify.NameSkewWarning)
	wantFail(t, r, "no skew warning for group rack-b")
	if len(r.Evidence) != 1 || strings.Contains(r.Evidence[0], "rack-a") {
		t.Errorf("evidence = %q, want only rack-b named", r.Evidence)
	}

	t.Run("every group warned", func(t *testing.T) {
		more := lines[2] + "\n" + `time=2026-11-12T09:00:04Z level=WARN msg="scheduler: worker heartbeat delayed or skewed: more" worker_id=sim-b1` + "\n"
		if err := os.WriteFile(log, []byte(more), 0o600); err != nil {
			t.Fatal(err)
		}
		wantOK(t, resultOf(t, checkResults(t, newSQLiteStore(t), StoreInput{SkewedGroups: groups, ServerLog: log}), verify.NameSkewWarning))
	})

	t.Run("json log", func(t *testing.T) {
		js := `{"time":"2026-11-12T09:00:00Z","level":"WARN","msg":"scheduler: worker heartbeat delayed or skewed: more","worker_id":"sim-a1","worker_at":"x"}` + "\n" +
			`{"time":"2026-11-12T09:00:00Z","level":"WARN","msg":"scheduler: worker heartbeat delayed or skewed: more","worker_id":"sim-b1"}` + "\n"
		if err := os.WriteFile(log, []byte(js), 0o600); err != nil {
			t.Fatal(err)
		}
		wantOK(t, resultOf(t, checkResults(t, newSQLiteStore(t), StoreInput{SkewedGroups: groups, ServerLog: log}), verify.NameSkewWarning))
	})

	t.Run("log missing", func(t *testing.T) {
		r := resultOf(t, checkResults(t, newSQLiteStore(t), StoreInput{SkewedGroups: groups, ServerLog: filepath.Join(t.TempDir(), "none.log")}), verify.NameSkewWarning)
		if r.OK || !r.Unmeasured {
			t.Errorf("skew-warning = %+v, want failed and unmeasured", r)
		}
	})
}

func TestSkewWarning_AbsentWithoutSkewedGroups(t *testing.T) {
	ts := newSQLiteStore(t)
	for _, r := range checkResults(t, ts, StoreInput{ServerLog: "ignored"}) {
		if r.Name == verify.NameSkewWarning {
			t.Errorf("a skew-warning result without skewed groups: %+v", r)
		}
	}
}
