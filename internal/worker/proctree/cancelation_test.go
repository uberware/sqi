// SPDX-License-Identifier: AGPL-3.0-or-later

package proctree_test

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/proctree"
	"github.com/uberware/sqi/internal/worker/protocol"
)

func TestResolve(t *testing.T) {
	tests := []struct {
		name       string
		in         *protocol.CancelationMethod
		def        time.Duration
		wantNotify bool
		wantPeriod time.Duration
	}{
		{name: "nil is TERMINATE", in: nil, def: 30 * time.Second},
		{name: "TERMINATE", in: &protocol.CancelationMethod{Mode: "TERMINATE"}, def: 30 * time.Second},
		{
			name: "TERMINATE ignores a period",
			in:   &protocol.CancelationMethod{Mode: "TERMINATE", NotifyPeriodSeconds: 60},
			def:  30 * time.Second,
		},
		{name: "unknown mode is TERMINATE", in: &protocol.CancelationMethod{Mode: "BOGUS"}, def: 30 * time.Second},
		{
			name:       "NOTIFY_THEN_TERMINATE takes the 30s default",
			in:         &protocol.CancelationMethod{Mode: "NOTIFY_THEN_TERMINATE"},
			def:        30 * time.Second,
			wantNotify: true,
			wantPeriod: 30 * time.Second,
		},
		{
			name:       "NOTIFY_THEN_TERMINATE takes the 120s default",
			in:         &protocol.CancelationMethod{Mode: "NOTIFY_THEN_TERMINATE"},
			def:        120 * time.Second,
			wantNotify: true,
			wantPeriod: 120 * time.Second,
		},
		{
			name:       "an explicit period wins",
			in:         &protocol.CancelationMethod{Mode: "NOTIFY_THEN_TERMINATE", NotifyPeriodSeconds: 5},
			def:        30 * time.Second,
			wantNotify: true,
			wantPeriod: 5 * time.Second,
		},
		{
			name:       "a period past the ceiling is clamped",
			in:         &protocol.CancelationMethod{Mode: "NOTIFY_THEN_TERMINATE", NotifyPeriodSeconds: 900},
			def:        30 * time.Second,
			wantNotify: true,
			wantPeriod: proctree.MaxNotifyPeriod,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotNotify, gotPeriod := proctree.Resolve(tt.in, tt.def)
			if gotNotify != tt.wantNotify || gotPeriod != tt.wantPeriod {
				t.Errorf("Resolve = (%v, %v); want (%v, %v)", gotNotify, gotPeriod, tt.wantNotify, tt.wantPeriod)
			}
		})
	}
}
