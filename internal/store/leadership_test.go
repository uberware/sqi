// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

func TestLease_Held(t *testing.T) {
	t.Parallel()
	const now = int64(1_000_000)
	tests := []struct {
		name  string
		lease store.Lease
		want  bool
	}{
		{"unexpired", store.Lease{Holder: "a", ExpiresUS: now + 1, NowUS: now}, true},
		{"expires at the clock read", store.Lease{Holder: "a", ExpiresUS: now, NowUS: now}, true},
		{"lapsed by a microsecond", store.Lease{Holder: "a", ExpiresUS: now - 1, NowUS: now}, false},
		{"released, unexpired", store.Lease{ExpiresUS: now + 1, NowUS: now}, false},
		{"never held", store.Lease{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.lease.Held(); got != tc.want {
				t.Errorf("Held() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestServerMember_BrokerSeenWithin(t *testing.T) {
	t.Parallel()
	const now = int64(10_000_000)
	const ttl = 3 * time.Second
	stamp := func(us int64) *int64 { return &us }
	tests := []struct {
		name  string
		stamp *int64
		want  bool
	}{
		{"never seen", nil, false},
		{"seen now", stamp(now), true},
		{"seen exactly ttl ago", stamp(now - ttl.Microseconds()), true},
		{"seen a microsecond past ttl", stamp(now - ttl.Microseconds() - 1), false},
		{"stamp ahead of the clock", stamp(now + 5), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := store.ServerMember{BrokerOKUS: tc.stamp}
			if got := m.BrokerSeenWithin(now, ttl); got != tc.want {
				t.Errorf("BrokerSeenWithin = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLeaseTTLMicros(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		ttl     time.Duration
		want    int64
		wantErr bool
	}{
		{"seconds", 15 * time.Second, 15_000_000, false},
		{"one microsecond", time.Microsecond, 1, false},
		{"rounds to nothing", 999 * time.Nanosecond, 0, true},
		{"zero", 0, 0, true},
		{"negative", -time.Second, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := store.LeaseTTLMicros(tc.ttl)
			if (err != nil) != tc.wantErr {
				t.Fatalf("LeaseTTLMicros(%v) err = %v, want error %v", tc.ttl, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("LeaseTTLMicros(%v) = %d, want %d", tc.ttl, got, tc.want)
			}
		})
	}
}
