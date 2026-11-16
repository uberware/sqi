// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// hangingAbortStore blocks AbortRetirement until its context ends, as a write
// on a connection to a database that has gone away does.
type hangingAbortStore struct {
	store.Store

	entered chan struct{}
}

func (s hangingAbortStore) AbortRetirement(ctx context.Context, _ string, _ time.Time, _ string) (bool, error) {
	s.entered <- struct{}{}
	<-ctx.Done()
	return false, ctx.Err()
}

// A provider call stuck in a store write does not hold shutdown: abandoning
// the calls cancels the write, so closing the write gate returns at once
// rather than waiting for a database that may never answer.
func TestActuator_AbandonCutsAHungStoreWrite(t *testing.T) {
	entered := make(chan struct{}, 1)
	f := newActFixture(t, func(st *fake.Store) store.Store { return hangingAbortStore{Store: st, entered: entered} })
	f.bus.setAnswer(func(string, protocol.ControlMsg) ([]byte, error) {
		return controlReply(false, protocol.ControlRefusalNotAllowed), nil
	})
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	t0 := time.Now().UTC()

	f.s.sweepCapacityAt(t.Context(), t0)
	f.s.sweepCapacityAt(t.Context(), t0.Add(2*time.Minute))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the refused retirement never reached AbortRetirement")
	}

	done := make(chan struct{})
	go func() {
		f.s.AbandonCapacityCalls()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("AbandonCapacityCalls blocked behind a store write that cannot be canceled")
	}
	f.s.capState.act.calls.Wait()
}
