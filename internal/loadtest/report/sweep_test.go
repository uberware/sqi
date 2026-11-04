// SPDX-License-Identifier: AGPL-3.0-or-later

package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/verify"
)

func sweepPoints(valid map[int]bool, failing map[int]bool, p95ms ...int) []SweepPoint {
	var pts []SweepPoint
	for i, ms := range p95ms {
		r := baseRecord()
		r.Histograms[HistFirstLease] = hist(time.Duration(ms) * time.Millisecond)
		r.Counters["tasks.terminal"] = int64(1000 * (i + 1))
		r.Invariants = []verify.Result{{Name: "terminal", OK: !failing[i], Evidence: nil}}
		if valid != nil && !valid[i] {
			r.Valid, r.InvalidReason = false, "generator saturated"
		}
		pts = append(pts, SweepPoint{Value: []string{"500", "1000", "2000", "5000", "10000"}[i], Rec: r})
	}
	return pts
}

func TestKnee(t *testing.T) {
	allValid := map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true}
	for _, tc := range []struct {
		name       string
		pts        []SweepPoint
		wantIndex  int
		wantReason string
	}{
		{"p95 doubles", sweepPoints(allValid, nil, 100, 110, 180, 250, 900), 3, "p95 2.5× baseline"},
		{"exactly twice is not a knee", sweepPoints(allValid, nil, 100, 150, 200), -1, ""},
		{"invalid run", sweepPoints(map[int]bool{0: true, 1: true, 2: false, 3: true, 4: true}, nil, 100, 110, 120, 130, 140), 2, "INVALID"},
		{"invariant failure", sweepPoints(allValid, map[int]bool{1: true}, 100, 110, 120), 1, "terminal"},
		{"no knee", sweepPoints(allValid, nil, 100, 110, 120, 130, 140), -1, ""},
		{"empty", nil, -1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx, reason := Knee(tc.pts)
			if idx != tc.wantIndex {
				t.Fatalf("index = %d (%s), want %d", idx, reason, tc.wantIndex)
			}
			if !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("reason = %q, want it to contain %q", reason, tc.wantReason)
			}
			if tc.wantIndex == -1 && reason == "" {
				t.Fatal("a missing knee still gets a reason")
			}
		})
	}
}

func TestKnee_MissingHistogramSkipsLatencyRule(t *testing.T) {
	pts := sweepPoints(map[int]bool{0: true, 1: true, 2: true}, nil, 100, 110, 900)
	delete(pts[1].Rec.Histograms, HistFirstLease)
	idx, _ := Knee(pts)
	if idx != 2 {
		t.Fatalf("index = %d, want 2", idx)
	}
	// No baseline latency at all: the latency rule cannot fire.
	delete(pts[0].Rec.Histograms, HistFirstLease)
	if idx, _ := Knee(pts); idx != -1 {
		t.Fatalf("index = %d, want -1", idx)
	}
}

func TestWriteSweep(t *testing.T) {
	allValid := map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true}
	pts := sweepPoints(allValid, nil, 100, 110, 180, 250, 900)
	dir := t.TempDir()
	if err := WriteSweep(dir, "workers[0].count", pts); err != nil {
		t.Fatal(err)
	}
	csv, err := os.ReadFile(filepath.Join(dir, "sweep.csv"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(csv)), "\n")
	if len(lines) != 6 {
		t.Fatalf("csv has %d lines:\n%s", len(lines), csv)
	}
	if want := "workers[0].count,throughput_per_s,first_lease_p50_s,first_lease_p95_s,first_lease_p99_s,valid,invariants_ok"; strings.TrimSpace(lines[0]) != want {
		t.Errorf("header = %q", lines[0])
	}
	if !strings.HasPrefix(lines[4], "5000,") || !strings.HasSuffix(strings.TrimSpace(lines[4]), ",true,true") {
		t.Errorf("row 3 = %q", lines[4])
	}
	md, err := os.ReadFile(filepath.Join(dir, "sweep.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"workers[0].count", "Knee", "5000", "p95 2.5× baseline", "250ms"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("sweep.md lacks %q:\n%s", want, md)
		}
	}

	// No knee.
	dir2 := t.TempDir()
	if err := WriteSweep(dir2, "workers[0].count", sweepPoints(allValid, nil, 100, 101)); err != nil {
		t.Fatal(err)
	}
	md2, err := os.ReadFile(filepath.Join(dir2, "sweep.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md2), "No knee") {
		t.Errorf("sweep.md = %s", md2)
	}
}
