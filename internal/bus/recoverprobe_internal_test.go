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

func TestFDHeadroomFrom(t *testing.T) {
	tests := []struct {
		open, limit uint64
		ok          bool
	}{
		{100, 1024, true},
		{961, 1024, false}, // needs 64
		{950, 1024, true},
		{9000, 10000, true},
		{9600, 10000, false}, // needs 500 (5%)
		{30, 40, false},      // the limit is below the headroom
	}
	for _, tt := range tests {
		err := fdHeadroomFrom(tt.open, tt.limit)
		if (err == nil) != tt.ok {
			t.Errorf("fdHeadroomFrom(%d, %d) = %v, want ok=%v", tt.open, tt.limit, err, tt.ok)
		}
	}
}

// TestProbeRecovered_HealthyHostPasses pins that the probe a restart waits on
// passes on a host with space and descriptors to spare, as a test host has.
func TestProbeRecovered_HealthyHostPasses(t *testing.T) {
	if err := probeRecovered(t.TempDir()); err != nil {
		t.Errorf("probeRecovered = %v, want nil", err)
	}
}
