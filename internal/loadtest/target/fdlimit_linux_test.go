// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package target

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fdChild is an "fdopen" helper the test drives over a pipe.
type fdChild struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Scanner
}

func startFDChild(t *testing.T) *fdChild {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "held")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), self)
	cmd.Env = append(os.Environ(), helperEnv+"=fdopen", helperFDFileEnv+"="+file)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the fdopen helper: %v", err)
	}
	t.Cleanup(func() {
		in.Close()
		_ = cmd.Process.Kill() //nolint:errcheck // already gone is fine
		_ = cmd.Wait()         //nolint:errcheck // killed on purpose
	})
	return &fdChild{cmd: cmd, in: in, out: bufio.NewScanner(out)}
}

// do sends a command and returns the child's answer.
func (c *fdChild) do(t *testing.T, cmd string) string {
	t.Helper()
	if _, err := io.WriteString(c.in, cmd+"\n"); err != nil {
		t.Fatalf("send %q: %v", cmd, err)
	}
	if !c.out.Scan() {
		t.Fatalf("no answer to %q: %v", cmd, c.out.Err())
	}
	return c.out.Text()
}

func (c *fdChild) pid() int { return c.cmd.Process.Pid }

func nofile(t *testing.T, pid int) unix.Rlimit {
	t.Helper()
	var l unix.Rlimit
	if err := unix.Prlimit(pid, unix.RLIMIT_NOFILE, nil, &l); err != nil {
		t.Fatalf("read RLIMIT_NOFILE of %d: %v", pid, err)
	}
	return l
}

func TestLimitServerFDs_RefusesNewFiles(t *testing.T) {
	c := startFDChild(t)
	if got := c.do(t, "open"); got != "ok" {
		t.Fatalf("open before the limit = %q", got)
	}
	c.do(t, "closeall")
	before := nofile(t, c.pid())

	const headroom = 5
	open, saved, err := limitFDs(c.pid(), headroom)
	if err != nil {
		t.Fatalf("limitFDs: %v", err)
	}
	if open < 3 {
		t.Errorf("open = %d, want at least stdin, stdout and stderr", open)
	}
	after := nofile(t, c.pid())
	if want := uint64(open + headroom); after.Cur != want {
		t.Errorf("soft limit = %d, want %d", after.Cur, want)
	}
	if after.Max != before.Max {
		t.Errorf("hard limit = %d, want it left at %d", after.Max, before.Max)
	}
	for i := range headroom {
		if got := c.do(t, "open"); got != "ok" {
			t.Fatalf("open %d of %d within the headroom = %q", i+1, headroom, got)
		}
	}
	if got := c.do(t, "open"); !strings.Contains(got, "too many open files") {
		t.Errorf("open past the headroom = %q, want too many open files", got)
	}

	if err := restoreFDs(c.pid(), saved); err != nil {
		t.Fatalf("restoreFDs: %v", err)
	}
	if got := nofile(t, c.pid()); got != before {
		t.Errorf("limit after the restore = %+v, want %+v", got, before)
	}
	if got := c.do(t, "open"); got != "ok" {
		t.Errorf("open after the restore = %q", got)
	}
}

func TestLimitFDs_NoHeadroomOverHardLimit(t *testing.T) {
	c := startFDChild(t)
	hard := nofile(t, c.pid()).Max
	if _, _, err := limitFDs(c.pid(), int(hard)); err == nil {
		t.Error("limitFDs with open+headroom over the hard limit succeeded")
	}
	if _, _, err := limitFDs(c.pid(), -1); err == nil {
		t.Error("limitFDs with a negative headroom succeeded")
	}
}

func TestRestoreFDs_DeadProcessIsNothingToRestore(t *testing.T) {
	c := startFDChild(t)
	_, saved, err := limitFDs(c.pid(), 5)
	if err != nil {
		t.Fatal(err)
	}
	pid := c.pid()
	_ = c.cmd.Process.Kill() //nolint:errcheck // the point of the test
	_ = c.cmd.Wait()         //nolint:errcheck // killed on purpose
	if err := restoreFDs(pid, saved); err != nil {
		t.Errorf("restoreFDs of a reaped process: %v", err)
	}
}

// managedWithServer is a target whose server is a helper process.
func managedWithServer(t *testing.T) (*managed, *proc) {
	t.Helper()
	m := newTestManaged(t, laneScenario(false))
	p := startHelper(t, "counter", helperCounterEnv+"="+filepath.Join(t.TempDir(), "n"))
	m.server = p
	return m, p
}

func TestManaged_LimitAndRestoreServerFDs(t *testing.T) {
	m, p := managedWithServer(t)
	before := nofile(t, p.pid())

	open, err := m.LimitServerFDs(10)
	if err != nil {
		t.Fatalf("LimitServerFDs: %v", err)
	}
	if open < 3 {
		t.Errorf("open = %d", open)
	}
	limited := nofile(t, p.pid())
	if limited.Cur != uint64(open+10) {
		t.Errorf("soft limit = %d, want %d", limited.Cur, open+10)
	}

	// Limiting a limited server keeps the first saved limit.
	if _, err := m.LimitServerFDs(20); err != nil {
		t.Fatalf("second LimitServerFDs: %v", err)
	}
	if got := nofile(t, p.pid()); got != limited {
		t.Errorf("a second LimitServerFDs changed the limit to %+v, want %+v", got, limited)
	}

	if err := m.RestoreServerFDs(); err != nil {
		t.Fatalf("RestoreServerFDs: %v", err)
	}
	if got := nofile(t, p.pid()); got != before {
		t.Errorf("limit after the restore = %+v, want %+v", got, before)
	}
	if err := m.RestoreServerFDs(); err != nil {
		t.Errorf("a second RestoreServerFDs: %v", err)
	}
}

func TestManaged_RestoreServerFDsAfterTheServerDied(t *testing.T) {
	m, p := managedWithServer(t)
	if _, err := m.LimitServerFDs(10); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.killOne(ctx); err != nil {
		t.Fatalf("kill the server: %v", err)
	}
	if err := m.RestoreServerFDs(); err != nil {
		t.Errorf("RestoreServerFDs after the server died: %v", err)
	}
}

func TestManaged_LimitServerFDsErrors(t *testing.T) {
	m := newTestManaged(t, laneScenario(false))
	if _, err := m.LimitServerFDs(5); err == nil {
		t.Error("LimitServerFDs without a server succeeded")
	}
	if err := m.RestoreServerFDs(); err != nil {
		t.Errorf("RestoreServerFDs with nothing limited: %v", err)
	}
}

// limitRecServer starts a fake server that records its open-file limit when it
// is stopped, and waits until it handles signals. recorded reads what it wrote.
func limitRecServer(t *testing.T) (p *proc, recorded func() string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "limit")
	p = startHelper(t, "limitrec", helperLimitFileEnv+"="+file)
	readPID(t, file+".ready")
	return p, func() string {
		t.Helper()
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("the server recorded no limit: %v", err)
		}
		return string(raw)
	}
}

func limitString(l unix.Rlimit) string {
	return strconv.FormatUint(l.Cur, 10) + " " + strconv.FormatUint(l.Max, 10)
}

// The server records the limit it has when the harness stops it, so these show
// the limit was back to the original before the stop, not just that the target
// forgot it.
func TestManaged_CloseRestoresServerFDs(t *testing.T) {
	m := newTestManaged(t, laneScenario(false))
	p, recorded := limitRecServer(t)
	m.server = p
	original := limitString(nofile(t, p.pid()))
	if _, err := m.LimitServerFDs(10); err != nil {
		t.Fatal(err)
	}
	if limitString(nofile(t, p.pid())) == original {
		t.Fatal("LimitServerFDs did not lower the limit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := recorded(); got != original {
		t.Errorf("the server's limit when Close stopped it = %q, want the original %q", got, original)
	}
}

func TestManaged_StopForChecksRestoresServerFDs(t *testing.T) {
	m := newTestManaged(t, laneScenario(false))
	p, recorded := limitRecServer(t)
	m.server = p
	original := limitString(nofile(t, p.pid()))
	if _, err := m.LimitServerFDs(10); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := m.StopForChecks(ctx); err != nil {
		t.Fatalf("StopForChecks: %v", err)
	}
	if got := recorded(); got != original {
		t.Errorf("the server's limit when StopForChecks stopped it = %q, want the original %q", got, original)
	}
	if m.fdSaved != nil {
		t.Error("StopForChecks left the server's open-file limit saved")
	}
}

// A restarted server is a new process that starts with the harness's limit:
// the limit saved for the old one is neither restored onto it nor mistaken for
// its own, and limiting it saves its limit afresh.
func TestManaged_ServerRestartForgetsSavedFDLimit(t *testing.T) {
	m, _ := managedWithServer(t)
	if _, err := m.LimitServerFDs(10); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := m.stopServer(ctx); err != nil {
		t.Fatalf("stopServer: %v", err)
	}
	if m.fdSaved != nil {
		t.Error("the saved limit survived stopping the server")
	}

	p2, recorded := limitRecServer(t)
	m.server = p2
	original := limitString(nofile(t, p2.pid()))
	if err := m.RestoreServerFDs(); err != nil {
		t.Fatalf("RestoreServerFDs on the restarted server: %v", err)
	}
	if got := limitString(nofile(t, p2.pid())); got != original {
		t.Errorf("the restarted server's limit = %q, want it untouched at %q", got, original)
	}
	open, err := m.LimitServerFDs(7)
	if err != nil {
		t.Fatalf("LimitServerFDs on the restarted server: %v", err)
	}
	if got := nofile(t, p2.pid()).Cur; got != uint64(open+7) {
		t.Errorf("the restarted server's soft limit = %d, want %d", got, open+7)
	}
	if err := m.RestoreServerFDs(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := recorded(); got != original {
		t.Errorf("the restarted server's limit at its stop = %q, want %q", got, original)
	}
}
