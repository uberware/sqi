// SPDX-License-Identifier: AGPL-3.0-or-later

package ha

// SetWaitHook installs f to be told, before each wait in Hold, whether a
// renewal is in flight, so a test clock can tell a Hold that has settled from
// one about to act on a renewal's answer.
func SetWaitHook(e *Elector, f func(inFlight bool)) { e.waitHook = f }

// StandbyHasBroker exposes standbyHasBroker to tests.
var StandbyHasBroker = standbyHasBroker
