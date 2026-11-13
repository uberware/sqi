// SPDX-License-Identifier: AGPL-3.0-or-later

package procout

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for the child processes, so these
// tests need no shell and run on Windows as well as POSIX.
//
//   - "write" prints a line and exits SQI_PROCOUT_EXIT.
//   - "hold" starts a "sleep" grandchild that inherits its stdout and stderr,
//     prints the grandchild's PID and a line, and exits 0.
//   - "sleep" sleeps for a minute.
func TestMain(m *testing.M) {
	switch os.Getenv("SQI_PROCOUT_MODE") {
	case "write":
		fmt.Println("before exit")
		code, _ := strconv.Atoi(os.Getenv("SQI_PROCOUT_EXIT")) //nolint:errcheck // unset means 0
		os.Exit(code)
	case "hold":
		c := exec.Command(os.Args[0]) //nolint:noctx // the grandchild must outlive this process
		c.Env = append(os.Environ(), "SQI_PROCOUT_MODE=sleep")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Printf("child=%d\n", c.Process.Pid)
		fmt.Println("before exit")
		os.Exit(0)
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// helper returns a command running the test binary in mode.
func helper(t *testing.T, mode string, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0])
	cmd.Env = append(append(os.Environ(), "SQI_PROCOUT_MODE="+mode), env...)
	return cmd
}

// TestWait_OnEveryPlatform is TestWait_WithoutAGroup without a shell: the
// output written before the exit is read, the exit status is kept, and a
// grandchild holding the output delays Wait by DrainDelay, not by its life.
func TestWait_OnEveryPlatform(t *testing.T) {
	shortDrain(t, 300*time.Millisecond)

	for _, tc := range []struct {
		name          string
		cmd           func(t *testing.T) *exec.Cmd
		wantExit      int
		wantStillOpen bool
	}{
		{name: "clean exit", cmd: func(t *testing.T) *exec.Cmd { return helper(t, "write") }},
		{name: "failed exit", cmd: func(t *testing.T) *exec.Cmd { return helper(t, "write", "SQI_PROCOUT_EXIT=3") }, wantExit: 3},
		{name: "grandchild holds the output", cmd: func(t *testing.T) *exec.Cmd { return helper(t, "hold") }, wantStillOpen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Start(tc.cmd(t))
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			lines := readAll(out)

			start := time.Now()
			stillOpen, err := out.Wait(nil)
			elapsed := time.Since(start)
			got := <-lines
			killPID(got)

			if elapsed > 10*time.Second {
				t.Fatalf("Wait took %v: it waited for the grandchild, not for the process", elapsed)
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

// TestWait_UnreadOutputDoesNotHangOnEveryPlatform: Wait returns for a caller
// that never reads the output, after Discard at once and without it once
// DrainDelay has passed.
func TestWait_UnreadOutputDoesNotHangOnEveryPlatform(t *testing.T) {
	shortDrain(t, 300*time.Millisecond)

	for _, discard := range []bool{true, false} {
		t.Run(fmt.Sprintf("discard=%v", discard), func(t *testing.T) {
			out, err := Start(helper(t, "write"))
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if discard {
				out.Discard()
			}
			done := make(chan struct{})
			go func() {
				out.Wait(nil) //nolint:errcheck // only the return matters
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Wait hung on output nothing read")
			}
		})
	}
}

// shortDrain shortens DrainDelay for one test.
func shortDrain(t *testing.T, d time.Duration) {
	t.Helper()
	orig := DrainDelay
	DrainDelay = d
	t.Cleanup(func() { DrainDelay = orig })
}

// readAll reads both streams to EOF and returns stdout's lines once both end.
func readAll(out *Output) <-chan []string {
	lines := make(chan []string, 1)
	errDone := make(chan struct{})
	go func() {
		io.Copy(io.Discard, out.Stderr) //nolint:errcheck // the tests read only stdout
		close(errDone)
	}()
	go func() {
		var got []string
		sc := bufio.NewScanner(out.Stdout)
		for sc.Scan() {
			got = append(got, sc.Text())
		}
		<-errDone
		lines <- got
	}()
	return lines
}

// killPID kills the process a helper reported as "child=<pid>".
func killPID(lines []string) {
	for _, l := range lines {
		if pid, ok := strings.CutPrefix(l, "child="); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(pid)); err == nil {
				if p, err := os.FindProcess(n); err == nil {
					p.Kill() //nolint:errcheck // best-effort cleanup of the test's own grandchild
				}
			}
		}
	}
}
