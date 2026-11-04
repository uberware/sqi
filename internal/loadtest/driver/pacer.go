// SPDX-License-Identifier: AGPL-3.0-or-later

package driver

import (
	"sync/atomic"
	"time"
)

// Pacer hands out the due times of an open-loop schedule: the n-th call to
// Next returns start + n/rate, whatever the clock says and however late the
// caller is. It never skips a slot and never sleeps, so a caller that falls
// behind sees due times in the past, acts at once, and records the gap as lag.
// That is what keeps a slow server from quietly lowering the offered load.
//
// A Pacer is safe for concurrent use.
type Pacer struct {
	start time.Time
	rate  float64
	n     atomic.Int64
}

// NewPacer returns a Pacer issuing rate due times per second from start. The
// first due time is start itself. A rate that is not positive is a caller
// error; the Pacer then returns start every time, which a caller must not use
// to loop.
func NewPacer(rate float64, start time.Time) *Pacer {
	return &Pacer{start: start, rate: rate}
}

// Next returns the due time of the next slot.
func (p *Pacer) Next() time.Time {
	n := p.n.Add(1) - 1
	if p.rate <= 0 {
		return p.start
	}
	return p.start.Add(time.Duration(float64(n) / p.rate * float64(time.Second)))
}
