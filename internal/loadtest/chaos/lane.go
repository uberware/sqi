// SPDX-License-Identifier: AGPL-3.0-or-later

package chaos

import (
	"sync"
	"time"

	"github.com/uberware/sqi/internal/loadtest/target"
)

// The states a lane can be in, in precedence order: when several faults hold
// a lane at once, the highest level wins.
const (
	levelOpen = iota
	levelShape
	levelBlackhole
	levelCut
)

// laneState is what a fault wants of a lane, or what a lane was last set to.
type laneState struct {
	level  int
	delay  time.Duration
	jitter time.Duration
	bps    int
}

// same reports whether s and o leave the lane the same.
func (s laneState) same(o laneState) bool {
	if s.level != o.level {
		return false
	}
	return s.level != levelShape || (s.delay == o.delay && s.jitter == o.jitter && s.bps == o.bps)
}

// laneCtl is the controller's bookkeeping for one lane. The proxy keeps one
// state at a time, and Heal clears all of it, so a fault cannot undo only its
// own change: the faults active on the lane are kept, and after each start or
// end the lane is set to the state they add up to.
type laneCtl struct {
	lane target.Lane

	mu sync.Mutex
	// active maps each active fault's token, which grows with the fault's
	// start, to what it wants.
	active  map[uint64]laneState
	applied laneState
	// healed is set by healAll; nothing changes the lane after it.
	healed bool
}

func newLaneCtl(l target.Lane) *laneCtl {
	return &laneCtl{lane: l, active: map[uint64]laneState{}}
}

// add records that the fault tok wants s, and applies the result.
func (l *laneCtl) add(tok uint64, s laneState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.healed {
		return
	}
	l.active[tok] = s
	l.applyLocked()
}

// remove records that the fault tok has ended, and applies the result.
func (l *laneCtl) remove(tok uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.active[tok]; !ok {
		return
	}
	delete(l.active, tok)
	l.applyLocked()
}

// healAll forgets every active fault, heals the lane when it is not open, and
// stops later faults from changing it.
func (l *laneCtl) healAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.healed = true
	clear(l.active)
	l.applyLocked()
}

// effectiveLocked is the state the active faults add up to: cut over
// blackhole over shape over open, and among shapes the one started last.
func (l *laneCtl) effectiveLocked() laneState {
	var best laneState
	var bestTok uint64
	for tok, s := range l.active {
		if s.level > best.level || (s.level == best.level && tok > bestTok) {
			best, bestTok = s, tok
		}
	}
	return best
}

// applyLocked moves the lane to the effective state, when it is not there.
func (l *laneCtl) applyLocked() {
	next := l.effectiveLocked()
	if next.same(l.applied) {
		return
	}
	switch next.level {
	case levelOpen:
		l.lane.Heal()
	case levelShape:
		// Only Heal leaves a cut or a blackhole, and it clears shaping too.
		if l.applied.level > levelShape {
			l.lane.Heal()
		}
		l.lane.Shape(next.delay, next.jitter, next.bps)
	case levelBlackhole:
		l.lane.Blackhole()
	case levelCut:
		l.lane.Cut()
	}
	l.applied = next
}
