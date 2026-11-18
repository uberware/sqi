// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/proctree"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// TestOnRunDefaultNotifyPeriod pins the notify period a task's onRun gets
// under NOTIFY_THEN_TERMINATE with no notifyPeriodInSeconds: OpenJD's 120 s
// for onRun, not the 30 s other actions get.
func TestOnRunDefaultNotifyPeriod(t *testing.T) {
	notify, period := proctree.Resolve(&protocol.CancelationMethod{Mode: "NOTIFY_THEN_TERMINATE"}, defaultNotifyPeriod)
	if !notify || period != 120*time.Second {
		t.Errorf("Resolve(NOTIFY_THEN_TERMINATE, defaultNotifyPeriod) = (%v, %v), want (true, 2m0s)", notify, period)
	}
}
