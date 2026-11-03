// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// errStore fails every active-credential lookup, as an unreachable database
// would.
type errStore struct {
	store.Store
}

func (errStore) GetActiveWorkerCredentialByPublicKey(context.Context, string) (store.WorkerCredential, error) {
	return store.WorkerCredential{}, errors.New("database is down")
}

func TestCredentialLookup_MapsNotFound(t *testing.T) {
	st := fake.New()
	ctx := context.Background()
	_, pub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID: "cred-a", WorkerID: "worker-a", PublicKey: pub, EnrolledAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	lookup := credentialLookup(st)

	t.Run("active key resolves to its worker", func(t *testing.T) {
		id, err := lookup(ctx, pub)
		if err != nil || id != "worker-a" {
			t.Fatalf("lookup = %q, %v; want worker-a", id, err)
		}
	})

	t.Run("unknown key is ErrUnknownCredential", func(t *testing.T) {
		_, other, err := brokerauth.GenerateSeed()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lookup(ctx, other); !errors.Is(err, bus.ErrUnknownCredential) {
			t.Fatalf("lookup of an unknown key = %v, want bus.ErrUnknownCredential", err)
		}
	})

	t.Run("revoked key is ErrUnknownCredential", func(t *testing.T) {
		if err := st.RevokeWorkerCredential(ctx, "worker-a", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if _, err := lookup(ctx, pub); !errors.Is(err, bus.ErrUnknownCredential) {
			t.Fatalf("lookup of a revoked key = %v, want bus.ErrUnknownCredential", err)
		}
	})

	t.Run("a store failure is not reported as an unknown key", func(t *testing.T) {
		_, err := credentialLookup(errStore{Store: st})(ctx, pub)
		if err == nil || errors.Is(err, bus.ErrUnknownCredential) {
			t.Fatalf("lookup with a failing store = %v; want an error other than bus.ErrUnknownCredential", err)
		}
	})
}
