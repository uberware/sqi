// SPDX-License-Identifier: AGPL-3.0-or-later

package ha_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/ha"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

const rosterNow = int64(1_000_000_000_000)

func ago(d time.Duration) int64 { return rosterNow - d.Microseconds() }

func TestBuildRoster(t *testing.T) {
	ttl := 15 * time.Second
	live := store.Lease{Holder: "a", Epoch: 3, ExpiresUS: rosterNow + 10_000_000, NowUS: rosterNow}
	broker := func(d time.Duration) *int64 { v := ago(d); return &v }

	tests := []struct {
		name      string
		lease     store.Lease
		members   []store.ServerMember
		wantEpoch int64
		want      []struct{ id, role string }
	}{
		{
			name:  "leader first, then name, standby before missing by name only",
			lease: live,
			members: []store.ServerMember{
				{InstanceID: "c", Name: "c", LastSeenUS: ago(time.Hour)},
				{InstanceID: "b", Name: "b", LastSeenUS: ago(time.Second)},
				{InstanceID: "a", Name: "z-leader", LastSeenUS: ago(time.Second)},
			},
			wantEpoch: 3,
			want:      []struct{ id, role string }{{"a", "leader"}, {"b", "standby"}, {"c", "missing"}},
		},
		{
			name:  "equal names sort by instance id",
			lease: store.Lease{Epoch: 1, NowUS: rosterNow},
			members: []store.ServerMember{
				{InstanceID: "y", Name: "n", LastSeenUS: ago(time.Second)},
				{InstanceID: "x", Name: "n", LastSeenUS: ago(time.Second)},
			},
			wantEpoch: 1,
			want:      []struct{ id, role string }{{"x", "standby"}, {"y", "standby"}},
		},
		{
			name:      "exactly lease_ttl ago is still standby, one microsecond more is missing",
			lease:     store.Lease{Epoch: 1, NowUS: rosterNow},
			wantEpoch: 1,
			members: []store.ServerMember{
				{InstanceID: "edge", Name: "a", LastSeenUS: ago(ttl)},
				{InstanceID: "past", Name: "b", LastSeenUS: ago(ttl) - 1},
			},
			want: []struct{ id, role string }{{"edge", "standby"}, {"past", "missing"}},
		},
		{
			name:      "a lapsed lease leads nobody",
			lease:     store.Lease{Holder: "a", Epoch: 4, ExpiresUS: rosterNow - 1, NowUS: rosterNow},
			wantEpoch: 4,
			members:   []store.ServerMember{{InstanceID: "a", Name: "a", LastSeenUS: ago(time.Hour)}},
			want:      []struct{ id, role string }{{"a", "missing"}},
		},
		{
			name:      "a released lease leads nobody",
			lease:     store.Lease{Holder: "", Epoch: 4, NowUS: rosterNow},
			wantEpoch: 4,
			members:   []store.ServerMember{{InstanceID: "a", Name: "a", LastSeenUS: ago(time.Second)}},
			want:      []struct{ id, role string }{{"a", "standby"}},
		},
		{
			name:      "a holder that is not a member is ignored",
			lease:     live,
			wantEpoch: 3,
			members:   []store.ServerMember{{InstanceID: "b", Name: "b", LastSeenUS: ago(time.Second)}},
			want:      []struct{ id, role string }{{"b", "standby"}},
		},
		{name: "no members", lease: live, wantEpoch: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := ha.BuildRoster(tc.lease, tc.members, rosterNow, ttl)
			if r.Epoch != tc.wantEpoch {
				t.Errorf("epoch = %d, want %d", r.Epoch, tc.wantEpoch)
			}
			if r.Members == nil {
				t.Error("Members is nil, want a non-nil slice")
			}
			if len(r.Members) != len(tc.want) {
				t.Fatalf("members = %+v, want %d", r.Members, len(tc.want))
			}
			for i, w := range tc.want {
				if r.Members[i].InstanceID != w.id || r.Members[i].Role != w.role {
					t.Errorf("member %d = %s/%s, want %s/%s", i, r.Members[i].InstanceID, r.Members[i].Role, w.id, w.role)
				}
			}
		})
	}

	t.Run("fields", func(t *testing.T) {
		started := time.Date(2026, 11, 3, 1, 2, 3, 0, time.UTC)
		r := ha.BuildRoster(live, []store.ServerMember{{
			InstanceID: "a", Name: "sched-a", Host: "h", Version: "v1", PID: 42, StartedAt: started,
			LastSeenUS: ago(2100 * time.Millisecond), BrokerOKUS: broker(14 * time.Second),
		}, {
			InstanceID: "b", Name: "sched-b", LastSeenUS: ago(time.Second), BrokerOKUS: broker(16 * time.Second),
		}, {
			InstanceID: "c", Name: "sched-c", LastSeenUS: ago(time.Second), // never saw a broker
		}, {
			InstanceID: "d", Name: "sched-d", LastSeenUS: rosterNow + 5_000_000, // ahead of the clock
		}}, rosterNow, ttl)
		a := r.Members[0]
		if a.Name != "sched-a" || a.Host != "h" || a.Version != "v1" || a.PID != 42 || !a.StartedAt.Equal(started) {
			t.Errorf("a = %+v", a)
		}
		if a.LastSeenAgo != 2100*time.Millisecond {
			t.Errorf("LastSeenAgo = %v, want 2.1s", a.LastSeenAgo)
		}
		if !a.BrokerOK {
			t.Error("broker seen 14s ago with a 15s ttl: BrokerOK false, want true")
		}
		if r.Members[1].BrokerOK {
			t.Error("broker seen 16s ago with a 15s ttl: BrokerOK true, want false")
		}
		if r.Members[2].BrokerOK {
			t.Error("a broker never seen: BrokerOK true, want false")
		}
		if r.Members[3].LastSeenAgo != 0 {
			t.Errorf("a stamp ahead of the clock: LastSeenAgo = %v, want 0", r.Members[3].LastSeenAgo)
		}
	})
}

type failingLeadership struct {
	store.LeadershipStore

	leaseErr, listErr error
}

func (f failingLeadership) GetLeadership(ctx context.Context) (store.Lease, error) {
	if f.leaseErr != nil {
		return store.Lease{}, f.leaseErr
	}
	return f.LeadershipStore.GetLeadership(ctx)
}

func (f failingLeadership) ListServerMembers(ctx context.Context) ([]store.ServerMember, int64, error) {
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.LeadershipStore.ListServerMembers(ctx)
}

func TestReadRoster(t *testing.T) {
	ctx := t.Context()
	db := fake.New()
	now := rosterNow
	db.SetClock(func() int64 { return now })
	for _, id := range []string{"a", "b"} {
		if err := db.UpsertServerMember(ctx, store.ServerMember{InstanceID: id, Name: id, StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	epoch, ok, err := db.AcquireLeadership(ctx, "a", 15*time.Second)
	if err != nil || !ok {
		t.Fatalf("acquire = %d, %v, %v", epoch, ok, err)
	}
	now += 2_000_000

	r, err := ha.ReadRoster(ctx, db, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if r.Epoch != epoch || len(r.Members) != 2 || r.Members[0].InstanceID != "a" || r.Members[0].Role != ha.RoleLeader ||
		r.Members[1].Role != ha.RoleStandby || r.Members[1].LastSeenAgo != 2*time.Second {
		t.Fatalf("roster = %+v", r)
	}

	boom := errors.New("boom")
	if _, err := ha.ReadRoster(ctx, failingLeadership{LeadershipStore: db, leaseErr: boom}, time.Second); !errors.Is(err, boom) {
		t.Errorf("lease error = %v, want boom", err)
	}
	if _, err := ha.ReadRoster(ctx, failingLeadership{LeadershipStore: db, listErr: boom}, time.Second); !errors.Is(err, boom) {
		t.Errorf("list error = %v, want boom", err)
	}
}
