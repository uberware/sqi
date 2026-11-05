// SPDX-License-Identifier: AGPL-3.0-or-later

package driver

import "slices"

// All returns every attempt, accepted or not, in the order recorded.
func (l *JobLedger) All() []SubmittedJob {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.all)
}
