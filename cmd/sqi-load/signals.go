// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
)

// interruptible returns a context that the first stop signal (Ctrl-C,
// SIGTERM, and SIGHUP on Unix) cancels, so the run stops, tears down and
// writes its record, and an abort channel that a second signal closes, so
// the teardown kills instead of waiting. After the second signal the
// default handling is restored: a third ends sqi-load at once. stop releases
// the signals.
func (a *app) interruptible(parent context.Context) (ctx context.Context, abort <-chan struct{}, stop func()) {
	ctx, cancel := context.WithCancel(parent)
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, stopSignals...)
	aborted := make(chan struct{})
	done := make(chan struct{})
	go func() {
		n := 0
		for {
			select {
			case <-done:
				return
			case s := <-sigs:
				n++
				if n == 1 {
					fmt.Fprintf(a.stderr, "sqi-load: %s: stopping the run and writing its record; interrupt again to kill its processes\n", s)
					cancel()
					continue
				}
				fmt.Fprintf(a.stderr, "sqi-load: %s: killing the run's processes\n", s)
				signal.Stop(sigs)
				close(aborted)
				return
			}
		}
	}()
	var once sync.Once
	return ctx, aborted, func() {
		once.Do(func() {
			signal.Stop(sigs)
			close(done)
			cancel()
		})
	}
}
