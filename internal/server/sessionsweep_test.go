// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// seedSession inserts a session expiring at the given time.
func seedSession(t *testing.T, st store.Store, userID, tokenHash string, expiresAt time.Time) {
	t.Helper()
	if _, err := st.CreateSession(context.Background(), store.Session{
		ID:        uuid.NewString(),
		TokenHash: tokenHash,
		UserID:    userID,
		ExpiresAt: expiresAt,
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

func seedSweepUser(t *testing.T, st store.Store) store.User {
	t.Helper()
	u, err := st.CreateUser(context.Background(), store.User{
		ID: uuid.NewString(), Username: "sweeper-" + uuid.NewString()[:8],
		PasswordHash: "$argon2id$stub", Role: "user",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

// TestStartSessionSweeper_ReapsExpiredOnStart pins the reason this exists:
// nothing else deletes a session on expiry, so without the sweep the table
// grows by one row per login forever.
func TestStartSessionSweeper_ReapsExpiredOnStart(t *testing.T) {
	st := fake.New()
	u := seedSweepUser(t, st)
	now := time.Now().UTC()

	seedSession(t, st, u.ID, "expired-1", now.Add(-time.Hour))
	seedSession(t, st, u.ID, "expired-2", now.Add(-time.Minute))
	seedSession(t, st, u.ID, "live", now.Add(time.Hour))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := &Server{store: st, logger: testLogger(), cfg: Config{AuthEnabled: true}}

	// The sweeper runs one pass immediately rather than waiting a full
	// interval, so a server restarted more often than the interval still
	// makes progress.
	s.startSessionSweeper(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := st.GetSessionByTokenHash(ctx, "expired-1", now); err != nil {
			break // reaped
		}
		if time.Now().After(deadline) {
			t.Fatal("expired session was not reaped within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := st.GetSessionByTokenHash(ctx, "expired-2", now); err == nil {
		t.Error("second expired session survived the sweep")
	}
	// A live session must not be touched — the sweep keys on expiry only.
	if _, err := st.GetSessionByTokenHash(ctx, "live", now); err != nil {
		t.Errorf("live session was reaped: %v", err)
	}
}

// TestStartSessionSweeper_StopsOnContextCancel pins that the goroutine is
// tied to the server lifecycle rather than leaking past shutdown.
func TestStartSessionSweeper_StopsOnContextCancel(t *testing.T) {
	st := &countingStore{Store: fake.New()}
	ctx, cancel := context.WithCancel(t.Context())
	s := &Server{store: st, logger: testLogger(), cfg: Config{AuthEnabled: true}}

	s.startSessionSweeper(ctx)
	cancel()

	// The initial sweep may or may not have landed before cancel; what matters
	// is that no further sweeps happen afterwards.
	time.Sleep(50 * time.Millisecond)
	before := st.deleteExpiredCalls.Load()
	time.Sleep(100 * time.Millisecond)
	if after := st.deleteExpiredCalls.Load(); after != before {
		t.Errorf("sweeper kept running after cancel: %d -> %d calls", before, after)
	}
}

// TestStartSessionSweeper_NoOpWhenAuthDisabled pins the auth-off invariant:
// no sessions are minted, so nothing sweeps and no goroutine is started.
func TestStartSessionSweeper_NoOpWhenAuthDisabled(t *testing.T) {
	st := &countingStore{Store: fake.New()}
	s := &Server{store: st, logger: testLogger(), cfg: Config{AuthEnabled: false}}

	s.startSessionSweeper(t.Context())
	time.Sleep(50 * time.Millisecond)

	if got := st.deleteExpiredCalls.Load(); got != 0 {
		t.Errorf("DeleteExpiredSessions calls = %d, want 0 with auth disabled", got)
	}
}
