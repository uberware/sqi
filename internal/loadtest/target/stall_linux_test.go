// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// threadStates returns the state letter of every thread of pid, read from
// /proc/<pid>/task/*/stat.
func threadStates(t *testing.T, pid int) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("/proc", strconv.Itoa(pid), "task", "*", "stat"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("list threads of %d: %v (%d found)", pid, err, len(paths))
	}
	states := make([]string, 0, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		// "tid (comm) S ...": comm may hold spaces and parentheses, so the
		// state is the first field after the last ')'.
		fields := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
		if len(fields) == 0 {
			t.Fatalf("%s: no state in %q", p, b)
		}
		states = append(states, fields[0])
	}
	return states
}

// TestProc_StallReturnsOnceStopped checks stall returns only once every
// thread of the process has stopped. kill(2) only queues SIGSTOP: the threads
// that have not taken it yet keep running, and a stalled sqi-server could
// still answer a request, until each one does.
func TestProc_StallReturnsOnceStopped(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "count")
	p := startHelper(t, "counter", helperCounterEnv+"="+counter)
	assertAdvancing(t, counter)
	for i := range 50 {
		if err := p.stall(); err != nil {
			t.Fatalf("stall %d: %v", i, err)
		}
		states := threadStates(t, p.pid())
		for _, s := range states {
			if s != "T" {
				t.Fatalf("stall %d returned with thread states %v; want every thread stopped (T)", i, states)
			}
		}
		if err := p.resume(); err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
	}
}
