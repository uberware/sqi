// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

func TestSetClock_DrivesLeaseExpiryWithoutSleeping(t *testing.T) {
	s := New()
	ctx := t.Context()
	now := int64(1_000_000)
	s.SetClock(func() int64 { return now })

	if _, ok, err := s.AcquireLeadership(ctx, "a", 10*time.Second); err != nil || !ok {
		t.Fatalf("a: ok=%v err=%v", ok, err)
	}
	// The instant the lease lapses is still the holder's; one microsecond
	// later it is gone. This is the boundary the SQL `expires_us < now` draws.
	now += 10_000_000
	if _, ok, err := s.AcquireLeadership(ctx, "b", time.Hour); err != nil || ok {
		t.Fatalf("b at the expiry instant: ok=%v err=%v, want refused", ok, err)
	}
	now++
	epoch, ok, err := s.AcquireLeadership(ctx, "b", time.Hour)
	if err != nil || !ok || epoch != 2 {
		t.Fatalf("b = (%d, %v, %v), want (2, true, nil)", epoch, ok, err)
	}
	l, err := s.GetLeadership(ctx)
	if err != nil || l.NowUS != now || l.ExpiresUS != now+3_600_000_000 {
		t.Fatalf("lease = %+v, err=%v, want NowUS %d and a one-hour expiry", l, err, now)
	}
}

func TestSetClock_NilRestoresTheRealClock(t *testing.T) {
	s := New()
	s.SetClock(func() int64 { return 5 })
	s.SetClock(nil)
	l, err := s.GetLeadership(t.Context())
	if err != nil || time.Since(time.UnixMicro(l.NowUS)).Abs() > time.Minute {
		t.Fatalf("NowUS = %d, err=%v, want about now", l.NowUS, err)
	}
}

func TestDeleteServerMembersUnseenFor_UsesTheInjectedClock(t *testing.T) {
	s := New()
	ctx := t.Context()
	now := int64(1_000_000_000)
	s.SetClock(func() int64 { return now })
	for _, id := range []string{"old", "new"} {
		if id == "new" {
			now += 3_600_000_000 // an hour on
		}
		if err := s.UpsertServerMember(ctx, store.ServerMember{InstanceID: id, Name: id, StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	now += 1_000_000
	n, err := s.DeleteServerMembersUnseenFor(ctx, 30*time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("Delete = (%d, %v), want (1, nil)", n, err)
	}
	got, _, err := s.ListServerMembers(ctx)
	if err != nil || len(got) != 1 || got[0].InstanceID != "new" {
		t.Fatalf("members = %+v, err=%v, want only new", got, err)
	}
}
