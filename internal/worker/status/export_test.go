// SPDX-License-Identifier: AGPL-3.0-or-later

package status

import "time"

// MsgDropped is the message of the Warn record logged for each entry Flush
// drops.
const MsgDropped = msgDropped

// SetSince replaces the clock that measures how long ago a task ended
// (default [time.Since]). Call it before the first send.
func SetSince(p *Publisher, f func(time.Time) time.Duration) {
	p.since = f
}
