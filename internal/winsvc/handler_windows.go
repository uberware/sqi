// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows/svc"
)

const (
	acceptedCmds = svc.AcceptStop | svc.AcceptPreShutdown
	// stopWaitHint is how long the SCM should wait for the next checkpoint.
	// Checkpoints advance every checkpointEvery, well inside this.
	stopWaitHint = 30 * time.Second
)

// handler implements svc.Handler. Shutdown is deliberately not accepted:
// PreShutdown supersedes it and gets the timeout `service install` configures,
// where Shutdown gets only a few seconds.
type handler struct {
	name            string // default; the SCM's args[0] wins
	configPath      string // absolute, or "" (refused)
	workDir         string
	fn              func(context.Context) error
	checkpointEvery time.Duration
	tracePath       func(serviceName string) string
	chdir           func(dir string) error
	inWorkDir       bool  // chdir succeeded: workDir exists and can take a fallback trace
	err             error // fn's result, returned by Run
}

// Execute runs fn and translates SCM requests into context cancellation.
// svc.Run reports Stopped after Execute returns, using its exit codes.
func (h *handler) Execute(args []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (specific bool, code uint32) {
	name := h.name
	if len(args) > 0 && args[0] != "" {
		name = args[0]
	}
	s <- svc.Status{State: svc.StartPending}

	if err := h.chdir(h.workDir); err != nil {
		return h.finish(name, fmt.Errorf("winsvc: change directory to %s: %w", h.workDir, err))
	}
	h.inWorkDir = true
	if err := requireConfigFile(h.name, h.configPath); err != nil {
		return h.finish(name, err)
	}

	ctx, cancel := context.WithCancelCause(withService(context.Background(), name))
	defer cancel(nil)
	done := make(chan error, 1)
	go func() { done <- h.fn(ctx) }()

	running := svc.Status{State: svc.Running, Accepts: acceptedCmds}
	s <- running
	for {
		select {
		case err := <-done:
			return h.finish(name, err)
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- running
			case svc.Stop, svc.PreShutdown:
				cancel(stopCause(c.Cmd))
				return h.finish(name, cleanStop(h.drain(done, r, s)))
			}
		}
	}
}

// cleanStop maps fn's result once the SCM has asked the service to stop. A Stop
// or PreShutdown that lands while fn is still booting (blocked in discovery, a
// dial or registration) makes fn return an error wrapping context.Canceled:
// that is the operator's stop taking effect, not a failure, and reporting it as
// exit code 1 would let the SCM's recovery actions restart a service that was
// just stopped. So a canceled error after a stop is nil. Everything else is
// left alone: any other error after a stop still fails, such as a shutdown
// timeout or context.DeadlineExceeded (and a context.Canceled with no stop
// requested — fn canceled a ctx it derived — never reaches here). Note errors.Is also
// matches any member of an errors.Join, so a joined error holding a Canceled
// leg plus a genuine failure would be forgiven after a stop; none of today's
// post-stop paths builds such a join.
func cleanStop(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// stopCause is the context.Cause fn sees for an SCM stop request.
func stopCause(c svc.Cmd) error {
	if c == svc.PreShutdown {
		return errors.New("service preshutdown")
	}
	return errors.New("service stop")
}

// drain reports StopPending with an advancing checkpoint until fn returns, and
// keeps answering the SCM so its control thread is never blocked on us.
func (h *handler) drain(done <-chan error, r <-chan svc.ChangeRequest, s chan<- svc.Status) error {
	status := svc.Status{State: svc.StopPending, CheckPoint: 1, WaitHint: uint32(stopWaitHint / time.Millisecond)}
	s <- status
	t := time.NewTicker(h.checkpointEvery)
	defer t.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-t.C:
			status.CheckPoint++
			s <- status
		case c := <-r:
			if c.Cmd == svc.Interrogate {
				s <- status
			}
		}
	}
}

// finish records fn's result and maps it to Execute's exit codes: nil exits 0;
// an error is traced (see trace) and exits with a service-specific code of 1,
// so the SCM (and its recovery actions) see a failure.
func (h *handler) finish(name string, err error) (specific bool, code uint32) {
	h.err = err
	if err == nil {
		return false, 0
	}
	h.trace(name, err)
	return true, 1
}

// trace appends err to the default log path. When that fails — its directory
// refused or impossible to create — the line goes to fallbackTracePath in the
// working directory instead, which exists once the service has changed into
// it, noting why the default log could not take it. Both are best effort: the
// exit code reports the failure regardless.
func (h *handler) trace(name string, err error) {
	logErr := appendTrace(h.tracePath(name), err)
	if logErr == nil || !h.inWorkDir {
		return
	}
	writeTrace(fallbackTracePath(h.workDir, name), err, logErr) //nolint:errcheck // best effort: the exit code still reports the failure
}
