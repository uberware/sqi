// SPDX-License-Identifier: AGPL-3.0-or-later

// Package procout starts a child process with its output read through pipes
// that let the process's exit be seen while a descendant still holds that
// output open.
//
// exec.Cmd.StdoutPipe requires every read to finish before Wait is called, so
// a caller that reads to EOF first never sees the process exit while a
// descendant that inherited stdout or stderr is alive. Unreal Editor's
// detached UnrealTraceServer is one: it leaves the task's process group and
// outlives the editor. A service an OpenJD onEnter action starts in the
// background is another.
//
// It depends only on the standard library, so any sub-package of
// internal/worker can import it without creating a cycle.
package procout

import (
	"errors"
	"io"
	"os/exec"
	"time"
)

// DrainDelay is how long output is still read after the process has exited,
// for a descendant that still holds it open. Output written after that is not
// captured. A variable so tests can shorten it.
var DrainDelay = 5 * time.Second

// Output is a started process's stdout and stderr.
type Output struct {
	Stdout, Stderr   io.Reader
	cmd              *exec.Cmd
	stdoutW, stderrW *io.PipeWriter
}

// Start starts cmd with its stdout and stderr connected to the returned
// Output's readers. The caller reads both concurrently and calls Wait.
func Start(cmd *exec.Cmd) (*Output, error) {
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	cmd.WaitDelay = DrainDelay
	o := &Output{Stdout: stdoutR, Stderr: stderrR, cmd: cmd, stdoutW: stdoutW, stderrW: stderrW}
	if err := cmd.Start(); err != nil {
		o.close()
		return nil, err
	}
	return o, nil
}

// Wait waits for the process to exit and then ends both readers, which reach
// EOF once they have read what the process wrote. It returns once the output
// has closed, or DrainDelay after the exit when a descendant still holds it
// open. stillOpen reports that second case for a process that otherwise
// exited cleanly; err is then nil.
func (o *Output) Wait() (stillOpen bool, err error) {
	err = o.cmd.Wait()
	o.close()
	if errors.Is(err, exec.ErrWaitDelay) {
		return true, nil
	}
	return false, err
}

func (o *Output) close() {
	o.stdoutW.Close()
	o.stderrW.Close()
}
