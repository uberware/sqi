// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/protocol"
)

// TestTerminationMode verifies the (mode, effectiveNotifyPeriod) computation
// from an OpenJD Action.cancelation method, including the spec defaults and the
// 600 s clamp.
func TestTerminationMode(t *testing.T) {
	tests := []struct {
		name       string
		in         *protocol.CancelationMethod
		wantMode   string
		wantPeriod time.Duration
	}{
		{
			name:       "nil defaults to TERMINATE",
			in:         nil,
			wantMode:   modeTerminate,
			wantPeriod: 0,
		},
		{
			name:       "explicit TERMINATE",
			in:         &protocol.CancelationMethod{Mode: modeTerminate},
			wantMode:   modeTerminate,
			wantPeriod: 0,
		},
		{
			// TERMINATE with a non-zero period: the period is meaningless for
			// TERMINATE (no SIGTERM phase) and must be ignored.
			name:       "TERMINATE with period: period is ignored",
			in:         &protocol.CancelationMethod{Mode: modeTerminate, NotifyPeriodSeconds: 60},
			wantMode:   modeTerminate,
			wantPeriod: 0,
		},
		{
			name:       "unknown mode treated as TERMINATE",
			in:         &protocol.CancelationMethod{Mode: "BOGUS"},
			wantMode:   modeTerminate,
			wantPeriod: 0,
		},
		{
			name:       "NOTIFY_THEN_TERMINATE with zero uses 120s default",
			in:         &protocol.CancelationMethod{Mode: modeNotifyThenTerminate},
			wantMode:   modeNotifyThenTerminate,
			wantPeriod: 120 * time.Second,
		},
		{
			name:       "NOTIFY_THEN_TERMINATE with explicit value",
			in:         &protocol.CancelationMethod{Mode: modeNotifyThenTerminate, NotifyPeriodSeconds: 45},
			wantMode:   modeNotifyThenTerminate,
			wantPeriod: 45 * time.Second,
		},
		{
			name:       "NOTIFY_THEN_TERMINATE clamped to 600s max",
			in:         &protocol.CancelationMethod{Mode: modeNotifyThenTerminate, NotifyPeriodSeconds: 5000},
			wantMode:   modeNotifyThenTerminate,
			wantPeriod: 600 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMode, gotPeriod := terminationMode(tt.in)
			if gotMode != tt.wantMode {
				t.Errorf("mode = %q; want %q", gotMode, tt.wantMode)
			}
			if gotPeriod != tt.wantPeriod {
				t.Errorf("period = %v; want %v", gotPeriod, tt.wantPeriod)
			}
		})
	}
}
