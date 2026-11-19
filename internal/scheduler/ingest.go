// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"sync"
	"time"
)

// ingestRef names what a stream message changes once applied: its worker,
// and for a task status also its task and attempt. An empty field names
// nothing of that kind.
type ingestRef struct {
	worker, task, attempt string
}

// pendingEntry is one stream message the consumer has not yet applied.
type pendingEntry struct {
	stamp time.Time
	ref   ingestRef
}

// pendingNames is what the unapplied messages name, by kind. A nil map names
// nothing of that kind.
type pendingNames struct {
	workers, tasks, attempts map[string]bool
}

// addName returns set with id added, allocating it on first use. An empty id
// is not added.
func addName(set map[string]bool, id string) map[string]bool {
	if id == "" {
		return set
	}
	if set == nil {
		set = make(map[string]bool)
	}
	set[id] = true
	return set
}

// ingestTracker records how far one server consumer has applied its stream:
// the newest marker it has processed, the messages it has failed and will see
// again, and a temporary ceiling after an interruption. The liveness checks
// read it to avoid judging store state that queued messages are about to
// change. It is safe for concurrent use.
type ingestTracker struct {
	mu         sync.Mutex
	maxAge     time.Duration
	maxDeliver uint64

	caughtUpTo time.Time
	pendingMsg map[uint64]pendingEntry

	holdAt    time.Time
	holdUntil time.Time

	// gapRelease is set while a gap hold is in force (see [ingestTracker.gap]):
	// effective stays at or before gapAt until caughtUpTo reaches it.
	gapAt      time.Time
	gapRelease time.Time
}

// newIngestTracker returns a tracker that forgets a message once it is older
// than maxAge (the stream drops it then) or has had maxDeliver deliveries.
func newIngestTracker(maxAge time.Duration, maxDeliver uint64) *ingestTracker {
	return &ingestTracker{
		maxAge:     maxAge,
		maxDeliver: maxDeliver,
		pendingMsg: make(map[uint64]pendingEntry),
	}
}

// marker records that a marker published at stamp has been processed. It
// never moves backwards.
func (t *ingestTracker) marker(stamp time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if stamp.After(t.caughtUpTo) {
		t.caughtUpTo = stamp
	}
	t.releaseGapLocked()
}

// gap records that messages were removed from the stream before the consumer
// read them, and that the first message read after them is stamped after.
// Markers vouch only for the messages the stream still holds, so a later
// marker says nothing of what the removed ones carried. effective is held at
// the newest marker processed before the gap (zero when there was none) until
// the consumer has processed a marker stamped window or more after after:
// only then has it read a whole window of the stream past the gap. A gap seen
// while a hold is in force keeps that hold's time and moves its release to
// this gap's own after plus window. It reports whether it started a new hold.
func (t *ingestTracker) gap(after time.Time, window time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.releaseGapLocked()
	release := after.Add(window)
	if t.gapRelease.IsZero() {
		t.gapAt, t.gapRelease = t.caughtUpTo, release
		return true
	}
	if release.After(t.gapRelease) {
		t.gapRelease = release
	}
	return false
}

// releaseGapLocked ends a gap hold once caughtUpTo has reached its release.
// The caller holds t.mu.
func (t *ingestTracker) releaseGapLocked() {
	if !t.gapRelease.IsZero() && !t.caughtUpTo.Before(t.gapRelease) {
		t.gapAt, t.gapRelease = time.Time{}, time.Time{}
	}
}

// nak records that stream message seq, stamped stamp, failed on its
// delivered-th delivery and names ref. On its last delivery it will not
// return, so it is removed instead.
func (t *ingestTracker) nak(seq uint64, stamp time.Time, delivered uint64, ref ingestRef) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if delivered >= t.maxDeliver {
		delete(t.pendingMsg, seq)
		return
	}
	t.pendingMsg[seq] = pendingEntry{stamp: stamp, ref: ref}
}

// ack removes message seq. An unknown seq is a no-op.
func (t *ingestTracker) ack(seq uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.pendingMsg, seq)
}

// dropThrough removes every message at or below stream sequence seq: the
// consumer's ack floor, at or below which the broker delivers nothing more.
// That covers messages whose last delivery never reached the handler, whose
// ack was lost, or that were removed from the stream, none of which the
// handler would otherwise see again.
func (t *ingestTracker) dropThrough(seq uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for s := range t.pendingMsg {
		if s <= seq {
			delete(t.pendingMsg, s)
		}
	}
}

// hold caps effective at the time at until the clock reaches until. A hold that
// overlaps the one in force keeps the earlier at and the later until.
func (t *ingestTracker) hold(at, until time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.holdUntil.After(at) {
		if t.holdAt.Before(at) {
			at = t.holdAt
		}
		if t.holdUntil.After(until) {
			until = t.holdUntil
		}
	}
	t.holdAt, t.holdUntil = at, until
}

// effective is the time up to which the consumer has applied its stream: zero
// before the first marker, then the newest marker, capped by an active hold
// and by a gap hold (which can be zero).
func (t *ingestTracker) effective(now time.Time) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.caughtUpTo.IsZero() {
		return time.Time{}
	}
	t.releaseGapLocked()
	eff := t.caughtUpTo
	if now.Before(t.holdUntil) && t.holdAt.Before(eff) {
		eff = t.holdAt
	}
	if !t.gapRelease.IsZero() && t.gapAt.Before(eff) {
		eff = t.gapAt
	}
	return eff
}

// lag is how far the newest processed marker trails now. Before the first
// marker it is the time since started, when the consumer began reading: a
// promoted standby or a restarted server behind a backlog has processed no
// marker exactly while its lag is largest.
func (t *ingestTracker) lag(now, started time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.caughtUpTo.IsZero() {
		if started.IsZero() {
			return 0
		}
		return now.Sub(started)
	}
	return now.Sub(t.caughtUpTo)
}

// pending returns what the unapplied messages name, by kind.
func (t *ingestTracker) pending(now time.Time) pendingNames {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prune(now)
	var out pendingNames
	for _, e := range t.pendingMsg {
		out.workers = addName(out.workers, e.ref.worker)
		out.tasks = addName(out.tasks, e.ref.task)
		out.attempts = addName(out.attempts, e.ref.attempt)
	}
	return out
}

// pendingFromWorkerBy reports whether an unapplied message names worker and
// was stamped at or before by.
func (t *ingestTracker) pendingFromWorkerBy(worker string, by, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prune(now)
	for _, e := range t.pendingMsg {
		if e.ref.worker == worker && !e.stamp.After(by) {
			return true
		}
	}
	return false
}

// pendingCount is the number of unapplied messages.
func (t *ingestTracker) pendingCount(now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prune(now)
	return len(t.pendingMsg)
}

// prune drops entries older than maxAge; the stream has dropped those
// messages too, so they will not be delivered again. The caller holds t.mu.
func (t *ingestTracker) prune(now time.Time) {
	cutoff := now.Add(-t.maxAge)
	for seq, e := range t.pendingMsg {
		if e.stamp.Before(cutoff) {
			delete(t.pendingMsg, seq)
		}
	}
}
