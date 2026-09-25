// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"context"
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
	workDir         string
	fn              func(context.Context) error
	checkpointEvery time.Duration
	tracePath       func(serviceName string) string
	chdir           func(dir string) error
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

	reason := &stopReason{}
	ctx, cancel := context.WithCancel(withService(context.Background(), name, reason))
	defer cancel()
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
				reason.set(stopReasonFor(c.Cmd))
				cancel()
				return h.finish(name, h.drain(done, r, s))
			}
		}
	}
}

func stopReasonFor(c svc.Cmd) string {
	if c == svc.PreShutdown {
		return "service preshutdown"
	}
	return "service stop"
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
// an error is appended to the default log path and exits with a
// service-specific code of 1, so the SCM (and its recovery actions) see a
// failure.
func (h *handler) finish(name string, err error) (specific bool, code uint32) {
	h.err = err
	if err == nil {
		return false, 0
	}
	appendTrace(h.tracePath(name), err) //nolint:errcheck // best effort: the exit code still reports the failure
	return true, 1
}
