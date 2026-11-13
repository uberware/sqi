// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package session

import (
	"context"
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
// result and the directives it printed, not when the service exits.
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
			script := "sleep 60 & echo child=$!; echo openjd_env: SERVICE=up; exit " + strconv.Itoa(tc.exit)
			start := time.Now()
			err := runAction(context.Background(), &protocol.Action{Command: "sh", Args: []string{"-c", script}},
				t.TempDir(), nil, nil, nopLogger(), handler)
			elapsed := time.Since(start)

			mu.Lock()
			got := append([]string(nil), lines...)
			mu.Unlock()
			for _, l := range got {
				if pid, ok := strings.CutPrefix(l, "child="); ok {
					if n, convErr := strconv.Atoi(pid); convErr == nil {
						syscall.Kill(n, syscall.SIGKILL) //nolint:errcheck // best-effort cleanup of the test's own sleep
					}
				}
			}
			if elapsed > 10*time.Second {
				t.Fatalf("runAction took %v: it waited for the descendant holding the output, not for the action", elapsed)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("runAction error = %v, want error %v", err, tc.wantErr)
			}
			if !strings.Contains(strings.Join(got, "\n"), "openjd_env: SERVICE=up") {
				t.Errorf("output %q lost the directive printed before the action exited", got)
			}
		})
	}
}
