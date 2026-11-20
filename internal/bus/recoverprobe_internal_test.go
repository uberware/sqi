// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProbeWritable(t *testing.T) {
	dir := t.TempDir()
	if err := probeWritable(dir); err != nil {
		t.Fatalf("probeWritable(temp dir) = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(dir, probeFileName)); !os.IsNotExist(err) {
		t.Errorf("the probe file is left behind (stat: %v)", err)
	}

	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := probeWritable(file); err == nil {
		t.Error("probeWritable(a regular file) = nil, want an error")
	}
}

// fds returns the descriptor numbers from..to-1.
func fds(from, to int) []int {
	out := make([]int, 0, to-from)
	for fd := from; fd < to; fd++ {
		out = append(out, fd)
	}
	return out
}

func TestFDHeadroomFrom(t *testing.T) {
	tests := []struct {
		name  string
		fds   []int
		limit uint64
		ok    bool
	}{
		{"plenty free", fds(0, 100), 1024, true},
		{"one short of 64 free", fds(0, 961), 1024, false},
		{"64 free", fds(0, 960), 1024, true},
		{"5% free of a large limit", fds(0, 9500), 10000, true},
		{"under 5% free of a large limit", fds(0, 9600), 10000, false},
		{"the limit is below the headroom", fds(0, 30), 40, false},
		// A limit lowered under a running process leaves its higher
		// descriptors open; they take no number a new open could use.
		{"descriptors above a lowered limit do not count", append(fds(0, 37), fds(150, 250)...), 150, true},
		{"too few numbers free below a lowered limit", append(fds(0, 37), fds(47, 136)...), 47, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := fdHeadroomFrom(tt.fds, tt.limit)
			if (err == nil) != tt.ok {
				t.Errorf("fdHeadroomFrom(%d descriptors, %d) = %v, want ok=%v", len(tt.fds), tt.limit, err, tt.ok)
			}
		})
	}
}

// TestFDHeadroomFrom_NamesTheFreeCount pins the reason an operator reads: the
// descriptors free under the limit, not how many are open.
func TestFDHeadroomFrom_NamesTheFreeCount(t *testing.T) {
	err := fdHeadroomFrom(append(fds(0, 37), fds(47, 136)...), 47)
	const want = "10 of 47 file descriptors are free; a restart needs 64 free"
	if err == nil || err.Error() != want {
		t.Errorf("fdHeadroomFrom = %v, want %q", err, want)
	}
}

// TestProbeRecovered_HealthyHostPasses pins that the probe a restart waits on
// passes on a host with space and descriptors to spare, as a test host has.
func TestProbeRecovered_HealthyHostPasses(t *testing.T) {
	if err := probeRecovered(t.TempDir()); err != nil {
		t.Errorf("probeRecovered = %v, want nil", err)
	}
}
