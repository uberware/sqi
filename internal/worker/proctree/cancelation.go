// SPDX-License-Identifier: AGPL-3.0-or-later

package proctree

import (
	"time"

	"github.com/uberware/sqi/internal/worker/protocol"
)

// MaxNotifyPeriod is the OpenJD maximum notify period; a larger
// notifyPeriodInSeconds is clamped down to this ceiling.
const MaxNotifyPeriod = 600 * time.Second

// modeNotifyThenTerminate is the OpenJD cancelation mode that notifies the
// process before terminating it. Every other mode, and no method at all, is
// TERMINATE, the spec default.
const modeNotifyThenTerminate = "NOTIFY_THEN_TERMINATE"

// Resolve turns an OpenJD Action.cancelation method into whether to notify
// the tree before killing it and, if so, how long to wait before the kill.
//
// A nil method (no cancelation specified) yields (false, 0): TERMINATE, the
// OpenJD spec default, which kills immediately. Any mode other than
// NOTIFY_THEN_TERMINATE is likewise treated as TERMINATE and its period is
// ignored.
//
// For NOTIFY_THEN_TERMINATE the period is NotifyPeriodSeconds when it is
// positive, otherwise defaultPeriod (the spec's default depends on the kind of
// action, which only the caller knows), clamped to [MaxNotifyPeriod].
func Resolve(c *protocol.CancelationMethod, defaultPeriod time.Duration) (notify bool, period time.Duration) {
	if c == nil || c.Mode != modeNotifyThenTerminate {
		return false, 0
	}
	period = defaultPeriod
	if c.NotifyPeriodSeconds > 0 {
		period = time.Duration(c.NotifyPeriodSeconds) * time.Second
	}
	return true, min(period, MaxNotifyPeriod)
}
