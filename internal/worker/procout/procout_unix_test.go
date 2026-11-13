// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package procout

import (
	"bufio"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestWait covers a process whose output closes when it exits and one that
// leaves a descendant holding it open. Either way Wait returns soon after the
// process exits, with its exit status, and the output written before the exit
// is read.
func TestWait(t *testing.T) {
	orig := DrainDelay
	DrainDelay = 200 * time.Millisecond
	t.Cleanup(func() { DrainDelay = orig })

	for _, tc := range []struct {
		name          string
		script        string
		wantExit      int
		wantStillOpen bool
	}{
		{name: "output closes on exit", script: "echo before exit; exit 0"},
		{name: "failed exit", script: "echo before exit; exit 3", wantExit: 3},
		{name: "descendant holds the output", script: "sleep 60 & echo child=$!; echo before exit; exit 0", wantStillOpen: true},
		{name: "descendant holds the output after a failed exit", script: "sleep 60 & echo child=$!; echo before exit; exit 3", wantExit: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Start(exec.CommandContext(t.Context(), "sh", "-c", tc.script))
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			lines := make(chan []string, 1)
			go func() { lines <- readLines(out.Stdout) }()
			go io.Copy(io.Discard, out.Stderr) //nolint:errcheck // the test reads only stdout

			start := time.Now()
			stillOpen, err := out.Wait()
			elapsed := time.Since(start)
			got := <-lines
			killChild(got)

			if elapsed > 10*time.Second {
				t.Fatalf("Wait took %v: it waited for the descendant, not for the process", elapsed)
			}
			if stillOpen != tc.wantStillOpen {
				t.Errorf("stillOpen = %v, want %v", stillOpen, tc.wantStillOpen)
			}
			var exitErr *exec.ExitError
			switch {
			case tc.wantExit == 0 && err != nil:
				t.Errorf("Wait error = %v, want nil", err)
			case tc.wantExit != 0 && (!errors.As(err, &exitErr) || exitErr.ExitCode() != tc.wantExit):
				t.Errorf("Wait error = %v, want exit status %d", err, tc.wantExit)
			}
			if !strings.Contains(strings.Join(got, "\n"), "before exit") {
				t.Errorf("output %q lost the line written before the exit", got)
			}
		})
	}
}

func TestStart_ReturnsTheStartError(t *testing.T) {
	if _, err := Start(exec.CommandContext(t.Context(), "/nonexistent/sqi-procout-test")); err == nil {
		t.Fatal("Start of a missing program succeeded")
	}
}

func readLines(r io.Reader) []string {
	var lines []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

// killChild kills the sleep a script reported as "child=<pid>".
func killChild(lines []string) {
	for _, l := range lines {
		if pid, ok := strings.CutPrefix(l, "child="); ok {
			if n, err := strconv.Atoi(pid); err == nil {
				syscall.Kill(n, syscall.SIGKILL) //nolint:errcheck // best-effort cleanup of the test's own sleep
			}
		}
	}
}
