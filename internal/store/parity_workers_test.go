// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

func TestParity_UpdateWorkerHeartbeat(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			t0, t1 := subSecond()
			farm := seedGraph(t, st, graphOpts{}).Farm
			w := seedWorker(t, st, farm.ID, store.WorkerStatusOnline, t0)

			if err := st.UpdateWorkerHeartbeat(ctx, w.ID, t1); err != nil {
				t.Fatalf("UpdateWorkerHeartbeat: %v", err)
			}
			got, err := st.GetWorker(ctx, w.ID)
			if err != nil || got.LastHeartbeatAt == nil || !got.LastHeartbeatAt.Equal(t1) {
				t.Fatalf("GetWorker = (%+v, %v), want last heartbeat %v", got, err, t1)
			}
			if err := st.UpdateWorkerHeartbeat(ctx, "ghost", t1); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown worker: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func mkCredential(workerID string) store.WorkerCredential {
	return store.WorkerCredential{
		ID: uuid.NewString(), WorkerID: workerID, PublicKey: "U" + uuid.NewString(), Name: workerID,
		EnrolledAt: time.Now().UTC(),
	}
}

func mustCreateCredential(t *testing.T, st store.Store, c store.WorkerCredential) store.WorkerCredential {
	t.Helper()
	out, err := st.CreateWorkerCredential(t.Context(), c)
	if err != nil {
		t.Fatalf("CreateWorkerCredential %s: %v", c.WorkerID, err)
	}
	return out
}

func activeCredentialIDs(t *testing.T, st store.Store) []string {
	t.Helper()
	creds, err := st.ListActiveWorkerCredentials(t.Context())
	if err != nil {
		t.Fatalf("ListActiveWorkerCredentials: %v", err)
	}
	ids := make([]string, 0, len(creds))
	for _, c := range creds {
		if c.RevokedAt != nil {
			t.Fatalf("listed credential %+v is revoked", c)
		}
		ids = append(ids, c.ID)
	}
	slices.Sort(ids)
	return ids
}

func TestParity_CreateWorkerCredential(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			want := mkCredential("w1")
			got, err := st.CreateWorkerCredential(ctx, want)
			if err != nil || got.ID != want.ID || got.WorkerID != "w1" || got.PublicKey != want.PublicKey ||
				got.Name != want.Name || !got.EnrolledAt.Equal(want.EnrolledAt) || got.LastSeenAt != nil || got.RevokedAt != nil {
				t.Fatalf("CreateWorkerCredential = (%+v, %v), want %+v", got, err, want)
			}

			sameID := mkCredential("w9")
			sameID.ID = want.ID
			sameKey := mkCredential("w8")
			sameKey.PublicKey = want.PublicKey
			for label, c := range map[string]store.WorkerCredential{
				"same worker": mkCredential("w1"), "same id": sameID, "same public key": sameKey,
			} {
				if _, err := st.CreateWorkerCredential(ctx, c); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("%s: err = %v, want ErrConflict", label, err)
				}
			}

			// A revoked credential frees its worker ID (key rotation) but never its key.
			if err := st.RevokeWorkerCredential(ctx, "w1", time.Now().UTC()); err != nil {
				t.Fatalf("RevokeWorkerCredential: %v", err)
			}
			rotated := mustCreateCredential(t, st, mkCredential("w1"))
			reused := mkCredential("w7")
			reused.PublicKey = want.PublicKey
			if _, err := st.CreateWorkerCredential(ctx, reused); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("revoked key reused: err = %v, want ErrConflict", err)
			}
			if got := activeCredentialIDs(t, st); !slices.Equal(got, []string{rotated.ID}) {
				t.Fatalf("active = %v, want only the rotated credential %s", got, rotated.ID)
			}
		})
	}
}

func TestParity_ListActiveWorkerCredentials(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			if got := activeCredentialIDs(t, st); len(got) != 0 {
				t.Fatalf("empty store: active = %v, want none", got)
			}
			a := mustCreateCredential(t, st, mkCredential("w1"))
			mustCreateCredential(t, st, mkCredential("w2"))
			c := mustCreateCredential(t, st, mkCredential("w3"))
			if err := st.RevokeWorkerCredential(t.Context(), "w2", time.Now().UTC()); err != nil {
				t.Fatalf("RevokeWorkerCredential: %v", err)
			}
			if got, want := activeCredentialIDs(t, st), sorted(a.ID, c.ID); !slices.Equal(got, want) {
				t.Fatalf("active = %v, want %v (the revoked one never listed)", got, want)
			}
		})
	}
}

func TestParity_RevokeWorkerCredential(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			now := time.Now().UTC()
			keep := mustCreateCredential(t, st, mkCredential("w2"))
			mustCreateCredential(t, st, mkCredential("w1"))
			if err := st.RevokeWorkerCredential(ctx, "w1", now); err != nil {
				t.Fatalf("RevokeWorkerCredential: %v", err)
			}
			if _, err := storetest.ActiveWorkerCredential(ctx, st, "w1"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("w1 after revoke: err = %v, want no active credential", err)
			}
			// After a rotation the worker has a revoked row and an active one:
			// revoking reaches the active one.
			mustCreateCredential(t, st, mkCredential("w1"))
			if err := st.RevokeWorkerCredential(ctx, "w1", now); err != nil {
				t.Fatalf("revoke after rotation: %v", err)
			}
			if _, err := storetest.ActiveWorkerCredential(ctx, st, "w1"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("w1 after second revoke: err = %v, want no active credential", err)
			}
			if got := activeCredentialIDs(t, st); !slices.Equal(got, []string{keep.ID}) {
				t.Fatalf("active = %v, want only %s", got, keep.ID)
			}
			if err := st.RevokeWorkerCredential(ctx, "ghost", now); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown worker: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParity_TouchWorkerCredential(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			_, seen := subSecond()
			mustCreateCredential(t, st, mkCredential("w1"))
			if err := st.RevokeWorkerCredential(ctx, "w1", time.Now().UTC()); err != nil {
				t.Fatalf("RevokeWorkerCredential: %v", err)
			}
			if err := st.TouchWorkerCredential(ctx, "w1", seen); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("only a revoked credential: err = %v, want ErrNotFound", err)
			}
			active := mustCreateCredential(t, st, mkCredential("w1"))
			if err := st.TouchWorkerCredential(ctx, "w1", seen); err != nil {
				t.Fatalf("TouchWorkerCredential: %v", err)
			}
			got, err := storetest.ActiveWorkerCredential(ctx, st, "w1")
			if err != nil || got.ID != active.ID || got.LastSeenAt == nil || !got.LastSeenAt.Equal(seen) {
				t.Fatalf("active credential = (%+v, %v), want %s last seen %v", got, err, active.ID, seen)
			}
			if err := st.TouchWorkerCredential(ctx, "ghost", seen); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown worker: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func mkJoinToken() store.WorkerJoinToken {
	now := time.Now().UTC()
	return store.WorkerJoinToken{
		ID: uuid.NewString(), TokenHash: "sha256-" + uuid.NewString(), Prefix: "sqi_jt_ab", Name: "rack 4",
		ExpiresAt: now.Add(time.Hour), CreatedBy: "admin", CreatedAt: now,
	}
}

func mustCreateJoinToken(t *testing.T, st store.Store) store.WorkerJoinToken {
	t.Helper()
	tok, err := st.CreateWorkerJoinToken(t.Context(), mkJoinToken())
	if err != nil {
		t.Fatalf("CreateWorkerJoinToken: %v", err)
	}
	return tok
}

func TestParity_GetWorkerJoinTokenByHash(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			tok := mustCreateJoinToken(t, st)
			mustCreateJoinToken(t, st)
			got, err := st.GetWorkerJoinTokenByHash(t.Context(), tok.TokenHash)
			if err != nil || got.ID != tok.ID || got.TokenHash != tok.TokenHash || got.Prefix != tok.Prefix ||
				got.Name != tok.Name || got.CreatedBy != tok.CreatedBy || !got.ExpiresAt.Equal(tok.ExpiresAt) ||
				!got.CreatedAt.Equal(tok.CreatedAt) || got.UsedAt != nil {
				t.Fatalf("GetWorkerJoinTokenByHash = (%+v, %v), want %+v", got, err, tok)
			}
			if _, err := st.GetWorkerJoinTokenByHash(t.Context(), "sha256-unknown"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown hash: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParity_MarkWorkerJoinTokenUsed(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			_, used := subSecond()
			tok := mustCreateJoinToken(t, st)
			if err := st.MarkWorkerJoinTokenUsed(ctx, tok.ID, used); err != nil {
				t.Fatalf("MarkWorkerJoinTokenUsed: %v", err)
			}
			got, err := st.GetWorkerJoinTokenByHash(ctx, tok.TokenHash)
			if err != nil || got.UsedAt == nil || !got.UsedAt.Equal(used) {
				t.Fatalf("token after mark = (%+v, %v), want used at %v", got, err, used)
			}
			// A used token can no longer be redeemed.
			if _, err := st.RedeemWorkerJoinToken(ctx, tok.TokenHash, time.Now().UTC(), mkCredential("w1")); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("redeem a used token: err = %v, want ErrNotFound", err)
			}
			if err := st.MarkWorkerJoinTokenUsed(ctx, uuid.NewString(), used); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
			}
		})
	}
}
