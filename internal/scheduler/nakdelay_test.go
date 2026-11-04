// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store/fake"
)

func TestNakDelay_DoublesUpToACap(t *testing.T) {
	tests := []struct {
		delivered uint64
		want      time.Duration
	}{
		{0, time.Second}, // no metadata
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{5, 16 * time.Second},
		{6, 30 * time.Second},
		{9, 30 * time.Second},
		{1000, 30 * time.Second},
	}
	for _, tt := range tests {
		if got := nakDelay(tt.delivered); got != tt.want {
			t.Errorf("nakDelay(%d) = %v, want %v", tt.delivered, got, tt.want)
		}
	}
}

// TestNakDelay_OutlastsAFailover checks the property the backoff is for: a
// message whose processing keeps failing while the database is unreachable
// (a PostgreSQL restart, or a leader cut off from it until a standby takes
// over) is still redelivered after the outage. The task-status and task-log
// consumers allow 10 deliveries, so the 9 delays between them must outlast a
// failover's worst case at the default lease (about 25 s) many times over.
// Without a delay all 10 deliveries would be spent in milliseconds and the
// terminal status of a task that finished during the outage would be lost, so
// the task would run again.
func TestNakDelay_OutlastsAFailover(t *testing.T) {
	var total time.Duration
	for n := uint64(1); n < 10; n++ {
		total += nakDelay(n)
	}
	if total < 2*time.Minute {
		t.Fatalf("9 redeliveries span %v, want at least 2m", total)
	}
}

func TestNakMsg_DelaysByDeliveryCount(t *testing.T) {
	s := newLogTestScheduler(fake.New())
	tests := []struct {
		name string
		msg  *fakeJSMsg
		want time.Duration
	}{
		{"first delivery", &fakeJSMsg{delivered: 1}, time.Second},
		{"fourth delivery", &fakeJSMsg{delivered: 4}, 8 * time.Second},
		{"unreadable metadata", &fakeJSMsg{delivered: 4, metaErr: errors.New("no metadata")}, time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s.nakMsg(t.Context(), tt.msg)
			if !tt.msg.nacked {
				t.Fatal("not nacked")
			}
			if tt.msg.nakDelay != tt.want {
				t.Fatalf("redelivery delay %v, want %v", tt.msg.nakDelay, tt.want)
			}
		})
	}
}
