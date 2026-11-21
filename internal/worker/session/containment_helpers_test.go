// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/procout"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// lowerDrainDelay shortens procout.DrainDelay for the test, so an action whose
// descendant survives it returns quickly enough to be measured.
func lowerDrainDelay(t *testing.T) {
	t.Helper()
	setDrainDelay(t, 200*time.Millisecond)
}

// setDrainDelay sets procout.DrainDelay to d for the test.
func setDrainDelay(t *testing.T, d time.Duration) {
	t.Helper()
	orig := procout.DrainDelay
	procout.DrainDelay = d
	t.Cleanup(func() { procout.DrainDelay = orig })
}

// runActionAsync runs action through runAction in the background and returns
// the channel its error arrives on.
func runActionAsync(ctx context.Context, t *testing.T, action *protocol.Action, handler func(stream, line string) string) <-chan error {
	t.Helper()
	dir := t.TempDir()
	result := make(chan error, 1)
	go func() { result <- runAction(ctx, action, dir, nil, nil, nil, "", nopLogger(), handler) }()
	return result
}

// awaitRunAction waits up to d for the action's result.
func awaitRunAction(t *testing.T, result <-chan error, d time.Duration) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(d):
		t.Fatalf("runAction did not return within %v", d)
		return nil
	}
}

// wantTimedOut fails the test unless err reports the action's 1s timeout, and
// not a cancel.
func wantTimedOut(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "timed out after 1s") {
		t.Errorf("runAction error = %v, want one saying it timed out after 1s", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("runAction error = %v wraps context.Canceled; a timeout is not a cancel", err)
	}
}
