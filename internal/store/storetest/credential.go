// SPDX-License-Identifier: AGPL-3.0-or-later

package storetest

import (
	"context"
	"fmt"

	"github.com/uberware/sqi/internal/store"
)

// ActiveWorkerCredential returns workerID's active credential — the one with a
// nil RevokedAt — or [store.ErrNotFound] if it has none. After a key rotation a
// worker has a revoked row and an active one; only the active one is returned.
//
// It reads through ListActiveWorkerCredentials, the set the broker's
// authorized keys are rebuilt from, so it asserts what authentication sees. More
// than one active credential for a worker is an error: the schema's partial
// unique index forbids it.
func ActiveWorkerCredential(ctx context.Context, st store.WorkerCredentialStore, workerID string) (store.WorkerCredential, error) {
	creds, err := st.ListActiveWorkerCredentials(ctx)
	if err != nil {
		return store.WorkerCredential{}, err
	}
	var found []store.WorkerCredential
	for _, c := range creds {
		if c.WorkerID == workerID {
			found = append(found, c)
		}
	}
	switch len(found) {
	case 0:
		return store.WorkerCredential{}, store.ErrNotFound
	case 1:
		return found[0], nil
	default:
		return store.WorkerCredential{}, fmt.Errorf("worker %s has %d active credentials, want at most 1", workerID, len(found))
	}
}
