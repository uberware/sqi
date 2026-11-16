// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// Kinds of identifier an ingest message can leave unapplied.
const (
	keyKindWorker  = "worker"
	keyKindTask    = "task"
	keyKindAttempt = "attempt"
)

func ingestKey(kind, id string) string { return kind + "/" + id }

func workerKey(id string) string  { return ingestKey(keyKindWorker, id) }
func taskKey(id string) string    { return ingestKey(keyKindTask, id) }
func attemptKey(id string) string { return ingestKey(keyKindAttempt, id) }

// pendingEntry is one stream message the consumer has not yet applied.
type pendingEntry struct {
	stamp time.Time
	keys  []string
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
}

// nak records that stream message seq, stamped stamp, failed on its
// delivered-th delivery and names keys. On its last delivery it will not
// return, so it is removed instead.
func (t *ingestTracker) nak(seq uint64, stamp time.Time, delivered uint64, keys []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if delivered >= t.maxDeliver {
		delete(t.pendingMsg, seq)
		return
	}
	t.pendingMsg[seq] = pendingEntry{stamp: stamp, keys: append([]string(nil), keys...)}
}

// ack removes message seq. An unknown seq is a no-op.
func (t *ingestTracker) ack(seq uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.pendingMsg, seq)
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
// before the first marker, then the newest marker, capped by an active hold.
func (t *ingestTracker) effective(now time.Time) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.caughtUpTo.IsZero() {
		return time.Time{}
	}
	if now.Before(t.holdUntil) && t.holdAt.Before(t.caughtUpTo) {
		return t.holdAt
	}
	return t.caughtUpTo
}

// lag is how far the newest processed marker trails now, or 0 before the
// first marker.
func (t *ingestTracker) lag(now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.caughtUpTo.IsZero() {
		return 0
	}
	return now.Sub(t.caughtUpTo)
}

// pending reports whether an unapplied message names key.
func (t *ingestTracker) pending(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prune(now)
	for _, e := range t.pendingMsg {
		if slices.Contains(e.keys, key) {
			return true
		}
	}
	return false
}

// pendingIDs returns the identifiers of the pending keys of one kind.
func (t *ingestTracker) pendingIDs(kind string, now time.Time) map[string]bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prune(now)
	prefix := kind + "/"
	out := make(map[string]bool)
	for _, e := range t.pendingMsg {
		for _, k := range e.keys {
			if id, ok := strings.CutPrefix(k, prefix); ok {
				out[id] = true
			}
		}
	}
	return out
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
