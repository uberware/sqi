// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/uberware/sqi/internal/capacity/script"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// closedAfterStore fails every member read once closed is set, as a store
// shutdown has closed does.
type closedAfterStore struct {
	store.Store

	closed *atomic.Bool
}

func (s closedAfterStore) ListCapacityGroupMembers(ctx context.Context, groupID string) ([]store.Worker, error) {
	if s.closed.Load() {
		return nil, errors.New("sql: database is closed")
	}
	return s.Store.ListCapacityGroupMembers(ctx, groupID)
}

// A provider list that returns after shutdown abandoned the calls reads
// nothing from the store: the orphan cleanup's member read goes through the
// write gate like its writes.
func TestScriptActuator_OrphanCleanupAfterAbandonReadsNothing(t *testing.T) {
	var closed atomic.Bool
	af := newActFixture(t, func(st *fake.Store) store.Store { return closedAfterStore{Store: st, closed: &closed} })
	prov := newFakeProvider()
	af.s.cfg.ScriptProviders = map[string]script.Provider{"cloud": {Name: "cloud", Command: []string{"unused"}}}
	af.s.capState.act.newScriptProvider = func(script.Provider) capacityProvider { return prov }
	g := createGroup(t, af.st, sizedGroup("cloud", func(g *store.CapacityGroup) { g.Provider = "script:cloud" }))
	f := &scriptFixture{actFixture: af, prov: prov, g: g}
	prov.setList("i-1", "i-2")
	prov.onCall = func(c providerCall) {
		if c.action == "list" {
			af.s.AbandonCapacityCalls()
			closed.Store(true)
		}
	}

	degradeWith(t, f, "i-1")

	if n := len(prov.callsOf("list")); n != 1 {
		t.Fatalf("list calls = %d, want 1", n)
	}
	if out := af.logs.String(); strings.Contains(out, "read members for orphan cleanup failed") {
		t.Errorf("the orphan cleanup read the store after the abandon:\n%s", out)
	}
	if n := len(prov.callsOf("retire_instances")); n != 0 {
		t.Errorf("instance retirements = %d, want 0", n)
	}
}
