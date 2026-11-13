// SPDX-License-Identifier: AGPL-3.0-or-later

// Package procout starts a child process with its output read in a way that
// lets the process's exit be seen while a descendant still holds that output
// open.
//
// exec.Cmd.StdoutPipe requires every read to finish before Wait is called, so
// a caller that reads to EOF first never sees the process exit while a
// descendant that inherited stdout or stderr is alive. Unreal Editor's
// detached UnrealTraceServer is one: it leaves the task's process group and
// outlives the editor. A service an OpenJD onEnter action starts in the
// background is another.
//
// The child writes into OS pipes that procout itself reads, so exec's Wait
// returns at the exit. procout then waits for the output to close, or, once
// no process of the child's own group is left, for at most DrainDelay. After
// that the caller's readers end, but the pipes are still read and the output
// thrown away, so a process that holds them is never killed by SIGPIPE for
// writing.
//
// It depends only on the standard library, so any sub-package of
// internal/worker can import it without creating a cycle.
package procout

import (
	"io"
	"os"
	"os/exec"
	"time"
)

// DrainDelay is how long output is still delivered after the process has
// exited and no process of its group is left, for a descendant that still
// holds it open. Output written after that is not delivered. A variable so
// tests can shorten it.
var DrainDelay = 5 * time.Second

// groupPoll is how often Wait checks whether the process group has emptied.
const groupPoll = 100 * time.Millisecond

// Output is a started process's stdout and stderr.
type Output struct {
	Stdout, Stderr io.Reader
	cmd            *exec.Cmd
	streams        [2]*stream
	done           chan struct{} // closed once both streams reach EOF
}

// stream copies one OS pipe into the reader the caller is given.
type stream struct {
	src  *os.File
	dst  *io.PipeWriter
	done chan struct{}
}

// Start starts cmd with its stdout and stderr connected to the returned
// Output's readers. The caller reads both, or calls Discard, and then calls
// Wait.
func Start(cmd *exec.Cmd) (*Output, error) {
	o := &Output{cmd: cmd, done: make(chan struct{})}
	var childEnds []*os.File
	closeAll := func() {
		for _, f := range childEnds {
			f.Close()
		}
		for _, s := range o.streams {
			if s != nil {
				s.src.Close()
				s.dst.Close()
			}
		}
	}
	for i, target := range []*io.Writer{&cmd.Stdout, &cmd.Stderr} {
		r, w, err := os.Pipe()
		if err != nil {
			closeAll()
			return nil, err
		}
		pr, pw := io.Pipe()
		o.streams[i] = &stream{src: r, dst: pw, done: make(chan struct{})}
		childEnds = append(childEnds, w)
		*target = w
		if i == 0 {
			o.Stdout = pr
		} else {
			o.Stderr = pr
		}
	}
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, err
	}
	// The child has its own copies; with the parent's closed, the pipes reach
	// EOF once every process holding them has closed them or exited.
	for _, f := range childEnds {
		f.Close()
	}
	for _, s := range o.streams {
		go s.copy()
	}
	go func() {
		for _, s := range o.streams {
			<-s.done
		}
		close(o.done)
	}()
	return o, nil
}

// copy delivers the pipe's bytes to the caller's reader until the pipe
// reaches EOF. Once the reader is closed, by Wait after the drain delay or by
// the caller, the bytes are read and dropped, so the writer is never blocked
// or killed for writing.
func (s *stream) copy() {
	defer close(s.done)
	defer s.src.Close()
	defer s.dst.Close()
	buf := make([]byte, 32<<10)
	deliver := true
	for {
		n, err := s.src.Read(buf)
		if n > 0 && deliver {
			if _, werr := s.dst.Write(buf[:n]); werr != nil {
				deliver = false
			}
		}
		if err != nil {
			return
		}
	}
}

// Discard reads and drops both streams, for a caller that will not read them.
// It returns at once.
func (o *Output) Discard() {
	for _, r := range []io.Reader{o.Stdout, o.Stderr} {
		go io.Copy(io.Discard, r) //nolint:errcheck // nothing to report about dropped output
	}
}

// Wait waits for the process to exit, then for its output to close, and
// returns the process's exit error. groupAlive reports whether any process of
// the child's own group (its process group, or its job object on Windows) is
// still running; while one is, Wait keeps waiting, as it would for a child it
// had not detached from. Once none is, or when groupAlive is nil, the output
// is delivered for at most DrainDelay more. stillOpen reports that it had not
// closed by then, which means a process outside the group holds it; the
// caller's readers then reach EOF after what was delivered.
func (o *Output) Wait(groupAlive func() bool) (stillOpen bool, err error) {
	err = o.cmd.Wait()
	if groupAlive != nil {
		tick := time.NewTicker(groupPoll)
		defer tick.Stop()
		for groupAlive() {
			select {
			case <-o.done:
				return false, err
			case <-tick.C:
			}
		}
	}
	timer := time.NewTimer(DrainDelay)
	defer timer.Stop()
	select {
	case <-o.done:
		return false, err
	case <-timer.C:
	}
	for _, s := range o.streams {
		s.dst.Close()
	}
	return true, err
}
