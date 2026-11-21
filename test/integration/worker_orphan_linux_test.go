// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration && linux

package integration

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// orphanJobYAML runs a task whose shell starts a silent child (no stdout, so a
// dead worker's closed pipe never kills it with SIGPIPE) and then sleeps. Each
// writes its pid into %s.
const orphanJobYAML = `specificationVersion: "jobtemplate-2023-09"
name: Orphaned Tree
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: sh
          args:
            - "-c"
            - "sh -c 'echo $$ > %[1]s/child.pid; exec sleep 300' </dev/null >/dev/null 2>&1 & echo $$ > %[1]s/main.pid; sleep 300"
`

// processGone reports whether pid has exited (or exited and awaits reaping).
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && strings.HasPrefix(strings.TrimSpace(s[i+1:]), "Z")
}

// TestWorkerRestart_KillsTheKilledWorkersTree kills a real worker outright
// while its task's tree runs, then starts a new worker on the same data
// directory: the new worker must kill the old tree (H13f4) rather than leave
// it running beside the re-run its registration triggers.
func TestWorkerRestart_KillsTheKilledWorkersTree(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	dataDir := t.TempDir()
	pidDir := t.TempDir()
	env := []string{"SQI_WORKER_DATA_DIR=" + dataDir}

	first := startRealWorkerNoWait(t, ts, farmID, queueID, nil, env, false)
	pollForOnlineWorker(t, ts, farmID, 30*time.Second)

	submitJobCustomYAML(t, ts, farmID, queueID, fmt.Sprintf(orphanJobYAML, pidDir))

	mainPID := waitForPID(t, filepath.Join(pidDir, "main.pid"), 30*time.Second)
	childPID := waitForPID(t, filepath.Join(pidDir, "child.pid"), 10*time.Second)
	t.Cleanup(func() {
		for _, pid := range []int{mainPID, childPID} {
			syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck // only after a failure; ESRCH otherwise
		}
	})
	// The re-run writes the same files; only these first pids are judged.
	for _, f := range []string{"main.pid", "child.pid"} {
		os.Remove(filepath.Join(pidDir, f)) // already read; a failure only leaves the file for the re-run to overwrite
	}

	// SIGKILL the worker alone, as a crash or the OOM killer would.
	if err := first.Process.Kill(); err != nil {
		t.Fatalf("kill worker: %v", err)
	}
	time.Sleep(time.Second)
	for _, pid := range []int{mainPID, childPID} {
		if processGone(pid) {
			t.Fatalf("pid %d ended with the worker; the test needs it to outlive the kill", pid)
		}
	}

	startRealWorkerNoWait(t, ts, farmID, queueID, nil, env, false)
	deadline := time.Now().Add(10 * time.Second)
	for !processGone(mainPID) || !processGone(childPID) {
		if time.Now().After(deadline) {
			t.Fatalf("the killed worker's tree survived the new worker's start: main %d gone=%v, child %d gone=%v",
				mainPID, processGone(mainPID), childPID, processGone(childPID))
		}
		time.Sleep(100 * time.Millisecond)
	}
}
