// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package session

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/procout"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// TestRunAction_FinishesWhenADescendantKeepsTheOutputOpen: an environment's
// onEnter that starts a background service, which the OpenJD environment
// model invites, leaves that service holding the action's stdout and stderr.
// The action must end soon after its own process exits, with that process's
// result and the directives it printed, not when the service exits, and the
// service must keep running when it writes again.
func TestRunAction_FinishesWhenADescendantKeepsTheOutputOpen(t *testing.T) {
	orig := procout.DrainDelay
	procout.DrainDelay = 200 * time.Millisecond
	defer func() { procout.DrainDelay = orig }()

	for _, tc := range []struct {
		name    string
		exit    int
		wantErr bool
	}{
		{name: "clean exit", exit: 0},
		{name: "failed exit", exit: 3, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var lines []string
			handler := func(_, line string) string {
				mu.Lock()
				defer mu.Unlock()
				lines = append(lines, line)
				return line
			}
			marker := filepath.Join(t.TempDir(), "marker")
			// The service writes once more after the drain, then records that
			// it got past that write.
			script := "(sleep 1; echo service still logging; touch '" + marker + "'; exec sleep 60) & echo child=$!; " +
				"echo openjd_env: SERVICE=up; exit " + strconv.Itoa(tc.exit)
			start := time.Now()
			err := runAction(context.Background(), &protocol.Action{Command: "sh", Args: []string{"-c", script}},
				t.TempDir(), nil, nil, nopLogger(), handler)
			elapsed := time.Since(start)

			mu.Lock()
			got := append([]string(nil), lines...)
			mu.Unlock()
			// Killed only once the test is done with it: the service must
			// outlive the action.
			t.Cleanup(func() {
				for _, l := range got {
					if pid, ok := strings.CutPrefix(l, "child="); ok {
						if n, convErr := strconv.Atoi(pid); convErr == nil {
							syscall.Kill(n, syscall.SIGKILL) //nolint:errcheck // best-effort cleanup of the test's own service
						}
					}
				}
			})
			if elapsed > 10*time.Second {
				t.Fatalf("runAction took %v: it waited for the descendant holding the output, not for the action", elapsed)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("runAction error = %v, want error %v", err, tc.wantErr)
			}
			if !strings.Contains(strings.Join(got, "\n"), "openjd_env: SERVICE=up") {
				t.Errorf("output %q lost the directive printed before the action exited", got)
			}
			if !waitForFile(marker, 5*time.Second) {
				t.Error("the service never got past its write after the drain: it was killed")
			}
		})
	}
}

// waitForFile reports whether path appears within d.
func waitForFile(path string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
