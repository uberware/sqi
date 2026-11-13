// SPDX-License-Identifier: AGPL-3.0-or-later

package chaos

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/marker"
	"github.com/uberware/sqi/internal/loadtest/target"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

var markerT0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func markerResult(t *testing.T, results []verify.Result, name string) verify.Result {
	t.Helper()
	var names []string
	for _, r := range results {
		names = append(names, r.Name)
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result %q among %v", name, names)
	return verify.Result{}
}

func TestMarkerChecks_NamesAndOrder(t *testing.T) {
	results := MarkerChecks(nil, 0, nil, nil)
	var names []string
	for _, r := range results {
		names = append(names, r.Name)
		if !r.OK {
			t.Errorf("%s failed on empty input: %q", r.Name, r.Evidence)
		}
		if !slices.Contains(verify.Names(), r.Name) {
			t.Errorf("%q is not in verify.Names()", r.Name)
		}
	}
	if want := []string{verify.NameNoOrphans, verify.NameSingleWriter}; !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
}

func TestNoOrphans_LiveAfterIncarnationDiedFails(t *testing.T) {
	incs := []target.Incarnation{{ID: "0.0", Worker: 0, Started: markerT0, Ended: markerT0.Add(time.Minute)}}
	execs := []marker.Exec{
		{ID: "e-silent", Key: "k1", Incarnation: "0.0", Role: marker.RoleSilent, PID: 4242, Start: markerT0, LastSeen: markerT0.Add(time.Minute + 30*time.Second)},
	}
	r := markerResult(t, MarkerChecks(execs, 0, nil, incs), verify.NameNoOrphans)
	if r.OK {
		t.Fatal("no-orphans passed, want it to fail")
	}
	joined := strings.Join(r.Evidence, "\n")
	for _, want := range []string{"e-silent", "silent", "k1", "4242", "0.0", "30s"} {
		if !strings.Contains(joined, want) {
			t.Errorf("evidence %q does not name %q", r.Evidence, want)
		}
	}
}

func TestNoOrphans_EndedWithinGracePasses(t *testing.T) {
	incs := []target.Incarnation{{ID: "0.0", Started: markerT0, Ended: markerT0.Add(time.Minute)}}
	execs := []marker.Exec{
		{ID: "e1", Key: "k1", Incarnation: "0.0", Role: marker.RoleSilent, LastSeen: markerT0.Add(time.Minute + 5*time.Second)},
		{ID: "e2", Key: "k1", Incarnation: "0.0", Role: marker.RoleMain, LastSeen: markerT0.Add(time.Minute + OrphanGrace)},
	}
	r := markerResult(t, MarkerChecks(execs, 0, nil, incs), verify.NameNoOrphans)
	if !r.OK {
		t.Errorf("no-orphans failed: %q", r.Evidence)
	}
}

func TestNoOrphans_RunningIncarnationIgnored(t *testing.T) {
	incs := []target.Incarnation{{ID: "0.0", Started: markerT0}}
	execs := []marker.Exec{
		{ID: "e1", Key: "k1", Incarnation: "0.0", Role: marker.RoleSilent, LastSeen: markerT0.Add(24 * time.Hour)},
		{ID: "e2", Key: "k1", Incarnation: "9.9", Role: marker.RoleSilent, LastSeen: markerT0.Add(24 * time.Hour)},
	}
	r := markerResult(t, MarkerChecks(execs, 0, nil, incs), verify.NameNoOrphans)
	if !r.OK {
		t.Errorf("no-orphans failed: %q", r.Evidence)
	}
}

func TestNoOrphans_SkippedFilesAreANoteNotAFailure(t *testing.T) {
	r := markerResult(t, MarkerChecks(nil, 3, nil, nil), verify.NameNoOrphans)
	if !r.OK {
		t.Fatalf("no-orphans failed: %q", r.Evidence)
	}
	if !strings.Contains(strings.Join(r.Evidence, "\n"), "3 marker files unreadable") {
		t.Errorf("evidence %q does not note the 3 unreadable files", r.Evidence)
	}
}

func lines(id string, offsets ...time.Duration) []marker.OutputLine {
	var out []marker.OutputLine
	for _, o := range offsets {
		out = append(out, marker.OutputLine{ExecID: id, At: markerT0.Add(o)})
	}
	return out
}

func TestSingleWriter_OverlapFails(t *testing.T) {
	// e1 writes at 0s, 2s; e2 writes at 1s, 3s: the intervals [0,2] and [1,3] overlap.
	var ls []marker.OutputLine
	ls = append(ls, lines("e1", 0, 2*time.Second)...)
	ls = append(ls, lines("e2", time.Second, 3*time.Second)...)
	ok := lines("e3", 0)
	r := markerResult(t, MarkerChecks(nil, 0, map[string][]marker.OutputLine{"key-a": ls, "key-b": ok}, nil), verify.NameSingleWriter)
	if r.OK {
		t.Fatal("single-writer passed, want it to fail")
	}
	joined := strings.Join(r.Evidence, "\n")
	for _, want := range []string{"key-a", "e1", "e2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("evidence %q does not name %q", r.Evidence, want)
		}
	}
	if strings.Contains(joined, "key-b") {
		t.Errorf("evidence %q names the clean key-b", r.Evidence)
	}
}

func TestSingleWriter_NestedIntervalFails(t *testing.T) {
	var ls []marker.OutputLine
	ls = append(ls, lines("e1", 0, 10*time.Second)...)
	ls = append(ls, lines("e2", 5*time.Second)...)
	r := markerResult(t, MarkerChecks(nil, 0, map[string][]marker.OutputLine{"k": ls}, nil), verify.NameSingleWriter)
	if r.OK {
		t.Fatal("single-writer passed, want it to fail")
	}
}

func TestSingleWriter_SequentialPasses(t *testing.T) {
	var ls []marker.OutputLine
	ls = append(ls, lines("e2", 3*time.Second, 4*time.Second)...)
	ls = append(ls, lines("e1", 0, time.Second)...)
	ls = append(ls, lines("e1", 2*time.Second)...)
	r := markerResult(t, MarkerChecks(nil, 0, map[string][]marker.OutputLine{"k": ls}, nil), verify.NameSingleWriter)
	if !r.OK {
		t.Errorf("single-writer failed: %q", r.Evidence)
	}
}
