// SPDX-License-Identifier: AGPL-3.0-or-later

package proctree

import (
	"errors"
	"time"
)

// stopPoll is how often Stop checks whether a notified tree has exited.
const stopPoll = 50 * time.Millisecond

// Stop ends every process of the tree by an OpenJD cancelation method, as
// [Resolve] reports it.
//
// Without notify it kills the tree at once (TERMINATE). With notify
// (NOTIFY_THEN_TERMINATE) it notifies the tree and waits for it to exit for
// at most period, then kills whatever is left; if the notify cannot be
// delivered it kills at once rather than wait for an exit nothing asked for.
//
// Kill is always called, so a process that outlived the tree's leader is
// ended too. A kill error is returned only when the tree is still alive
// afterwards: killing a tree that has already emptied fails on POSIX (the
// process group no longer exists), and is not an error.
func (t *Tree) Stop(notify bool, period time.Duration) error {
	var notifyErr error
	if notify {
		notifyErr = t.Notify()
		if notifyErr == nil {
			t.awaitExit(period)
		}
	}
	killErr := t.Kill()
	if killErr != nil && !t.Alive() {
		killErr = nil
	}
	return errors.Join(notifyErr, killErr)
}

// awaitExit returns once no process of the tree is running, or period has
// passed.
func (t *Tree) awaitExit(period time.Duration) {
	deadline := time.Now().Add(period)
	for t.Alive() && time.Now().Before(deadline) {
		time.Sleep(stopPoll)
	}
}
