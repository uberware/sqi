// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// lapse is longer than the 50 ms leases the tests let expire, and longer than
// the millisecond resolution of the SQLite clock.
const lapse = 100 * time.Millisecond

func TestParity_GetLeadership_Fresh(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			l, err := st.GetLeadership(t.Context())
			if err != nil {
				t.Fatalf("GetLeadership: %v", err)
			}
			if l.Holder != "" || l.Epoch != 0 {
				t.Fatalf("fresh lease = %+v, want no holder at epoch 0", l)
			}
			if l.NowUS <= 0 {
				t.Fatalf("NowUS = %d, want the database clock", l.NowUS)
			}
			if got := time.UnixMicro(l.NowUS); time.Since(got).Abs() > time.Minute {
				t.Fatalf("NowUS = %v, want about now", got)
			}
		})
	}
}

func TestParity_AcquireLeadership_OnEmpty(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			epoch, ok, err := st.AcquireLeadership(ctx, "a", 15*time.Second)
			if err != nil || !ok || epoch != 1 {
				t.Fatalf("AcquireLeadership = (%d, %v, %v), want (1, true, nil)", epoch, ok, err)
			}
			l, err := st.GetLeadership(ctx)
			if err != nil {
				t.Fatalf("GetLeadership: %v", err)
			}
			if l.Holder != "a" || l.Epoch != 1 {
				t.Fatalf("lease = %+v, want holder a at epoch 1", l)
			}
			if l.ExpiresUS <= l.NowUS {
				t.Fatalf("ExpiresUS %d is not after NowUS %d", l.ExpiresUS, l.NowUS)
			}
			// The ttl is honored: about 15 s ahead, not 15 ms or 15 min.
			if ahead := time.Duration(l.ExpiresUS-l.NowUS) * time.Microsecond; ahead < 14*time.Second || ahead > 16*time.Second {
				t.Fatalf("lease expires %v ahead, want about 15s", ahead)
			}
		})
	}
}

func TestParity_AcquireLeadership_RefusedWhileHeld(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			if _, ok, err := st.AcquireLeadership(ctx, "a", 15*time.Second); err != nil || !ok {
				t.Fatalf("a: ok=%v err=%v", ok, err)
			}
			epoch, ok, err := st.AcquireLeadership(ctx, "b", 15*time.Second)
			if err != nil || ok {
				t.Fatalf("b = (%d, %v, %v), want refused with no error", epoch, ok, err)
			}
			l, err := st.GetLeadership(ctx)
			if err != nil || l.Holder != "a" || l.Epoch != 1 {
				t.Fatalf("lease = %+v, err=%v, want holder a at epoch 1", l, err)
			}
			// The holder cannot acquire over its own live lease either; it renews.
			if _, ok, err := st.AcquireLeadership(ctx, "a", 15*time.Second); err != nil || ok {
				t.Fatalf("a again: ok=%v err=%v, want refused", ok, err)
			}
		})
	}
}

func TestParity_AcquireLeadership_AfterExpiryBumpsEpoch(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			if _, ok, err := st.AcquireLeadership(ctx, "a", 50*time.Millisecond); err != nil || !ok {
				t.Fatalf("a: ok=%v err=%v", ok, err)
			}
			time.Sleep(lapse)
			epoch, ok, err := st.AcquireLeadership(ctx, "b", 15*time.Second)
			if err != nil || !ok || epoch != 2 {
				t.Fatalf("b = (%d, %v, %v), want (2, true, nil)", epoch, ok, err)
			}
			l, err := st.GetLeadership(ctx)
			if err != nil || l.Holder != "b" || l.Epoch != 2 {
				t.Fatalf("lease = %+v, err=%v, want holder b at epoch 2", l, err)
			}
		})
	}
}

func TestParity_AcquireLeadership_NonPositiveTTLIsRefused(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			for _, ttl := range []time.Duration{0, -time.Second, 100 * time.Nanosecond} {
				if _, ok, err := st.AcquireLeadership(ctx, "a", ttl); err == nil || ok {
					t.Fatalf("ttl %v: ok=%v err=%v, want an error", ttl, ok, err)
				}
				if _, err := st.RenewLeadership(ctx, "a", 1, ttl); err == nil {
					t.Fatalf("renew ttl %v: want an error", ttl)
				}
			}
			l, err := st.GetLeadership(ctx)
			if err != nil || l.Holder != "" || l.Epoch != 0 {
				t.Fatalf("lease = %+v, err=%v, want untouched", l, err)
			}
		})
	}
}

func TestParity_RenewLeadership_ExtendsTheLease(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			epoch, ok, err := st.AcquireLeadership(ctx, "a", 2*time.Second)
			if err != nil || !ok {
				t.Fatalf("a: ok=%v err=%v", ok, err)
			}
			before, err := st.GetLeadership(ctx)
			if err != nil {
				t.Fatalf("GetLeadership: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
			ok, err = st.RenewLeadership(ctx, "a", epoch, 30*time.Second)
			if err != nil || !ok {
				t.Fatalf("RenewLeadership = (%v, %v), want (true, nil)", ok, err)
			}
			after, err := st.GetLeadership(ctx)
			if err != nil {
				t.Fatalf("GetLeadership: %v", err)
			}
			if after.Holder != "a" || after.Epoch != epoch {
				t.Fatalf("lease = %+v, want holder a at epoch %d", after, epoch)
			}
			if after.ExpiresUS <= before.ExpiresUS+int64(20*time.Second/time.Microsecond) {
				t.Fatalf("ExpiresUS %d did not move out from %d", after.ExpiresUS, before.ExpiresUS)
			}
		})
	}
}

func TestParity_RenewLeadership_RefusedForAnyoneElse(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			epoch, _, err := st.AcquireLeadership(ctx, "a", 15*time.Second)
			if err != nil {
				t.Fatalf("a: %v", err)
			}
			for _, tc := range []struct {
				name   string
				holder string
				epoch  int64
			}{
				{"wrong holder", "b", epoch},
				{"wrong epoch", "a", epoch + 1},
				{"zero epoch", "a", 0},
			} {
				ok, err := st.RenewLeadership(ctx, tc.holder, tc.epoch, time.Hour)
				if err != nil || ok {
					t.Fatalf("%s: RenewLeadership = (%v, %v), want (false, nil)", tc.name, ok, err)
				}
			}
			l, err := st.GetLeadership(ctx)
			if err != nil {
				t.Fatalf("GetLeadership: %v", err)
			}
			if ahead := time.Duration(l.ExpiresUS-l.NowUS) * time.Microsecond; ahead > 16*time.Second {
				t.Fatalf("a refused renewal moved the expiry %v ahead", ahead)
			}
		})
	}
}

func TestParity_RenewLeadership_StaleEpochFails(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			if _, ok, err := st.AcquireLeadership(ctx, "a", 50*time.Millisecond); err != nil || !ok {
				t.Fatalf("a: ok=%v err=%v", ok, err)
			}
			time.Sleep(lapse)
			if _, ok, err := st.AcquireLeadership(ctx, "b", 15*time.Second); err != nil || !ok {
				t.Fatalf("b: ok=%v err=%v", ok, err)
			}
			ok, err := st.RenewLeadership(ctx, "a", 1, 15*time.Second)
			if err != nil || ok {
				t.Fatalf("RenewLeadership(a, 1) = (%v, %v), want (false, nil)", ok, err)
			}
			l, err := st.GetLeadership(ctx)
			if err != nil || l.Holder != "b" || l.Epoch != 2 {
				t.Fatalf("lease = %+v, err=%v, want holder b at epoch 2", l, err)
			}
		})
	}
}

func TestParity_RenewLeadership_LapsedButUntakenCanBeRenewed(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			epoch, _, err := st.AcquireLeadership(ctx, "a", 50*time.Millisecond)
			if err != nil {
				t.Fatalf("a: %v", err)
			}
			time.Sleep(lapse)
			ok, err := st.RenewLeadership(ctx, "a", epoch, 15*time.Second)
			if err != nil || !ok {
				t.Fatalf("RenewLeadership = (%v, %v), want (true, nil): nobody took over", ok, err)
			}
			if _, ok, err := st.AcquireLeadership(ctx, "b", 15*time.Second); err != nil || ok {
				t.Fatalf("b: ok=%v err=%v, want refused after the renewal", ok, err)
			}
		})
	}
}

func TestParity_ReleaseThenAcquireBumpsEpoch(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			if _, ok, err := st.AcquireLeadership(ctx, "a", 15*time.Second); err != nil || !ok {
				t.Fatalf("a: ok=%v err=%v", ok, err)
			}
			if err := st.ReleaseLeadership(ctx, "a", 1); err != nil {
				t.Fatalf("ReleaseLeadership: %v", err)
			}
			l, err := st.GetLeadership(ctx)
			if err != nil || l.Holder != "" || l.Epoch != 1 {
				t.Fatalf("after release lease = %+v, err=%v, want no holder, epoch still 1", l, err)
			}
			if ok, err := st.RenewLeadership(ctx, "a", 1, 15*time.Second); err != nil || ok {
				t.Fatalf("renew after release = (%v, %v), want (false, nil)", ok, err)
			}
			epoch, ok, err := st.AcquireLeadership(ctx, "b", 15*time.Second)
			if err != nil || !ok || epoch != 2 {
				t.Fatalf("b = (%d, %v, %v), want (2, true, nil)", epoch, ok, err)
			}
		})
	}
}

func TestParity_ReleaseByNonHolderIsNoop(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			if _, ok, err := st.AcquireLeadership(ctx, "a", 15*time.Second); err != nil || !ok {
				t.Fatalf("a: ok=%v err=%v", ok, err)
			}
			for _, tc := range []struct {
				holder string
				epoch  int64
			}{{"b", 1}, {"a", 2}, {"a", 0}} {
				if err := st.ReleaseLeadership(ctx, tc.holder, tc.epoch); err != nil {
					t.Fatalf("ReleaseLeadership(%s, %d): %v", tc.holder, tc.epoch, err)
				}
			}
			l, err := st.GetLeadership(ctx)
			if err != nil || l.Holder != "a" || l.Epoch != 1 {
				t.Fatalf("lease = %+v, err=%v, want holder a at epoch 1", l, err)
			}
		})
	}
}

func TestParity_ServerMembers_UpsertListDelete(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			started := time.Date(2026, 11, 3, 9, 30, 0, 123456789, time.UTC)
			m := store.ServerMember{
				InstanceID: "i-1", Name: "alpha", Host: "render-01", PID: 4242, Version: "0.9.0",
				GrantsFingerprint: "fp1", StartedAt: started,
			}
			if err := st.UpsertServerMember(ctx, m); err != nil {
				t.Fatalf("UpsertServerMember: %v", err)
			}
			got, now1, err := st.ListServerMembers(ctx)
			if err != nil || len(got) != 1 {
				t.Fatalf("ListServerMembers = %+v, err=%v, want one", got, err)
			}
			first := got[0]
			if first.InstanceID != "i-1" || first.Name != "alpha" || first.Host != "render-01" ||
				first.PID != 4242 || first.Version != "0.9.0" || first.GrantsFingerprint != "fp1" ||
				!first.StartedAt.Equal(started) {
				t.Fatalf("member = %+v, want the values upserted", first)
			}
			if first.BrokerOKUS != nil {
				t.Fatalf("BrokerOKUS = %d, want nil when BrokerOK was false", *first.BrokerOKUS)
			}
			if first.LastSeenUS <= 0 || first.LastSeenUS > now1 {
				t.Fatalf("LastSeenUS = %d, now = %d, want a stamp at or before now", first.LastSeenUS, now1)
			}

			time.Sleep(10 * time.Millisecond)
			m.Version, m.GrantsFingerprint = "0.9.1", "fp2"
			if err := st.UpsertServerMember(ctx, m); err != nil {
				t.Fatalf("UpsertServerMember again: %v", err)
			}
			got, _, err = st.ListServerMembers(ctx)
			if err != nil || len(got) != 1 {
				t.Fatalf("after second upsert: %+v, err=%v, want still one row", got, err)
			}
			if got[0].LastSeenUS <= first.LastSeenUS {
				t.Fatalf("LastSeenUS %d did not advance from %d", got[0].LastSeenUS, first.LastSeenUS)
			}
			if got[0].Version != "0.9.1" || got[0].GrantsFingerprint != "fp2" {
				t.Fatalf("member = %+v, want the second upsert's values", got[0])
			}

			time.Sleep(10 * time.Millisecond)
			n, err := st.DeleteServerMembersUnseenFor(ctx, time.Hour)
			if err != nil || n != 0 {
				t.Fatalf("Delete(1h) = (%d, %v), want (0, nil) for a member just seen", n, err)
			}
			n, err = st.DeleteServerMembersUnseenFor(ctx, 0)
			if err != nil || n != 1 {
				t.Fatalf("Delete(0) = (%d, %v), want (1, nil)", n, err)
			}
			got, _, err = st.ListServerMembers(ctx)
			if err != nil || len(got) != 0 {
				t.Fatalf("after delete: %+v, err=%v, want none", got, err)
			}
		})
	}
}

func TestParity_ServerMembers_BrokerStampIsKept(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			m := store.ServerMember{
				InstanceID: "i-1", Name: "alpha", Host: "h", PID: 1, Version: "v",
				GrantsFingerprint: "fp", StartedAt: time.Now(), BrokerOK: true,
			}
			if err := st.UpsertServerMember(ctx, m); err != nil {
				t.Fatalf("UpsertServerMember: %v", err)
			}
			got, _, err := st.ListServerMembers(ctx)
			if err != nil || len(got) != 1 || got[0].BrokerOKUS == nil {
				t.Fatalf("members = %+v, err=%v, want a broker stamp", got, err)
			}
			stamp := *got[0].BrokerOKUS
			if stamp <= 0 || stamp > got[0].LastSeenUS+int64(time.Second/time.Microsecond) {
				t.Fatalf("broker stamp %d, last seen %d: not a database clock reading", stamp, got[0].LastSeenUS)
			}

			// A refresh that did not see the broker keeps the earlier stamp.
			time.Sleep(10 * time.Millisecond)
			m.BrokerOK = false
			if err := st.UpsertServerMember(ctx, m); err != nil {
				t.Fatalf("UpsertServerMember: %v", err)
			}
			got, _, err = st.ListServerMembers(ctx)
			if err != nil || len(got) != 1 || got[0].BrokerOKUS == nil || *got[0].BrokerOKUS != stamp {
				t.Fatalf("members = %+v, err=%v, want the stamp %d kept", got, err, stamp)
			}

			// One that did see it moves the stamp forward.
			time.Sleep(10 * time.Millisecond)
			m.BrokerOK = true
			if err := st.UpsertServerMember(ctx, m); err != nil {
				t.Fatalf("UpsertServerMember: %v", err)
			}
			got, _, err = st.ListServerMembers(ctx)
			if err != nil || len(got) != 1 || got[0].BrokerOKUS == nil || *got[0].BrokerOKUS <= stamp {
				t.Fatalf("members = %+v, err=%v, want a stamp after %d", got, err, stamp)
			}
		})
	}
}

func TestParity_ServerMembers_ListIsOrderedByNameThenID(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			for _, m := range []store.ServerMember{
				{InstanceID: "i-3", Name: "beta"},
				{InstanceID: "i-2", Name: "alpha"},
				{InstanceID: "i-1", Name: "beta"},
			} {
				m.Host, m.Version, m.GrantsFingerprint, m.StartedAt = "h", "v", "fp", time.Now()
				if err := st.UpsertServerMember(ctx, m); err != nil {
					t.Fatalf("UpsertServerMember(%s): %v", m.InstanceID, err)
				}
			}
			got, _, err := st.ListServerMembers(ctx)
			if err != nil {
				t.Fatalf("ListServerMembers: %v", err)
			}
			var ids []string
			for _, m := range got {
				ids = append(ids, m.InstanceID)
			}
			want := []string{"i-2", "i-1", "i-3"}
			if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
				t.Fatalf("order = %v, want %v", ids, want)
			}
		})
	}
}

func TestParity_ListServerMembers_EmptyStillReportsTheClock(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			got, now, err := st.ListServerMembers(t.Context())
			if err != nil || len(got) != 0 {
				t.Fatalf("ListServerMembers = %+v, err=%v, want none", got, err)
			}
			if time.Since(time.UnixMicro(now)).Abs() > time.Minute {
				t.Fatalf("nowUS = %d, want the database clock", now)
			}
		})
	}
}
