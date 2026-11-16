// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"
)

var ingestT0 = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func ingestAt(sec int) time.Time { return ingestT0.Add(time.Duration(sec) * time.Second) }

func TestIngestTracker_ZeroUntilFirstMarker(t *testing.T) {
	tr := newIngestTracker(2*time.Minute, 10)
	if got := tr.effective(ingestT0); !got.IsZero() {
		t.Fatalf("effective before first marker = %v, want zero", got)
	}
	if got := tr.lag(ingestT0); got != 0 {
		t.Fatalf("lag before first marker = %v, want 0", got)
	}

	tr.marker(ingestT0)
	if got := tr.effective(ingestT0); !got.Equal(ingestT0) {
		t.Fatalf("effective = %v, want %v", got, ingestT0)
	}
	if got := tr.lag(ingestAt(5)); got != 5*time.Second {
		t.Fatalf("lag = %v, want 5s", got)
	}
}

func TestIngestTracker_MarkerNeverMovesBack(t *testing.T) {
	tr := newIngestTracker(2*time.Minute, 10)
	tr.marker(ingestAt(10))
	tr.marker(ingestAt(0))
	if got := tr.effective(ingestAt(11)); !got.Equal(ingestAt(10)) {
		t.Fatalf("effective = %v, want %v", got, ingestAt(10))
	}
}

func TestIngestTracker_PendingPerMessage(t *testing.T) {
	tr := newIngestTracker(2*time.Minute, 10)
	k := []string{workerKey("w")}
	tr.nak(1, ingestAt(0), 1, k)
	tr.nak(2, ingestAt(1), 1, k)
	tr.ack(2)
	if !tr.pending(workerKey("w"), ingestAt(2)) {
		t.Fatal("key must stay pending while message 1 is unacked")
	}
	tr.ack(1)
	if tr.pending(workerKey("w"), ingestAt(2)) {
		t.Fatal("key must clear once both messages are acked")
	}
	tr.ack(99) // unknown: no-op
}

func TestIngestTracker_NakReplacesEntry(t *testing.T) {
	tr := newIngestTracker(2*time.Minute, 10)
	tr.nak(1, ingestAt(0), 1, []string{workerKey("a")})
	tr.nak(1, ingestAt(1), 2, []string{workerKey("b")})
	if tr.pending(workerKey("a"), ingestAt(2)) {
		t.Fatal("replaced entry's old key must not stay pending")
	}
	if !tr.pending(workerKey("b"), ingestAt(2)) {
		t.Fatal("replacement entry's key must be pending")
	}
	if got := tr.pendingCount(ingestAt(2)); got != 1 {
		t.Fatalf("pendingCount = %d, want 1", got)
	}
}

func TestIngestTracker_LastDeliveryClears(t *testing.T) {
	tr := newIngestTracker(2*time.Minute, 10)
	keys := []string{workerKey("w"), taskKey("t")}
	tr.nak(5, ingestAt(0), 9, keys)
	if got := tr.pendingCount(ingestAt(1)); got != 1 {
		t.Fatalf("pendingCount after delivery 9 = %d, want 1", got)
	}
	tr.nak(5, ingestAt(0), 10, keys)
	if got := tr.pendingCount(ingestAt(1)); got != 0 {
		t.Fatalf("pendingCount after final delivery = %d, want 0", got)
	}
	if tr.pending(workerKey("w"), ingestAt(1)) {
		t.Fatal("key pending after final delivery")
	}
	tr.nak(6, ingestAt(0), 10, keys) // never recorded
	if got := tr.pendingCount(ingestAt(1)); got != 0 {
		t.Fatalf("pendingCount after unrecorded final delivery = %d, want 0", got)
	}
}

func TestIngestTracker_ExpiresByOwnStamp(t *testing.T) {
	tr := newIngestTracker(2*time.Minute, 10)
	k := []string{workerKey("w")}
	tr.nak(1, ingestAt(0), 1, k)
	if !tr.pending(workerKey("w"), ingestAt(119)) {
		t.Fatal("must be pending at t0+119s")
	}
	if tr.pending(workerKey("w"), ingestAt(121)) {
		t.Fatal("must not be pending at t0+121s")
	}
	if got := tr.pendingCount(ingestAt(121)); got != 0 {
		t.Fatalf("pendingCount = %d, want 0", got)
	}

	tr.nak(2, ingestAt(0), 1, k)
	tr.nak(3, ingestAt(60), 1, k)
	if !tr.pending(workerKey("w"), ingestAt(121)) {
		t.Fatal("the younger message must keep the key pending")
	}
	if got := tr.pendingCount(ingestAt(121)); got != 1 {
		t.Fatalf("pendingCount = %d, want 1", got)
	}
}

func TestIngestTracker_HoldClampsEffective(t *testing.T) {
	tr := newIngestTracker(2*time.Minute, 10)
	tr.marker(ingestAt(10))
	tr.hold(ingestAt(5), ingestAt(20))
	for _, c := range []struct {
		now  int
		want time.Time
	}{
		{15, ingestAt(5)},
		{21, ingestAt(10)},
	} {
		if got := tr.effective(ingestAt(c.now)); !got.Equal(c.want) {
			t.Fatalf("effective at %ds = %v, want %v", c.now, got, c.want)
		}
	}

	tr.hold(ingestAt(3), ingestAt(18))
	for _, c := range []struct {
		now  int
		want time.Time
	}{
		{15, ingestAt(3)},
		{19, ingestAt(3)}, // the later until is kept
		{21, ingestAt(10)},
	} {
		if got := tr.effective(ingestAt(c.now)); !got.Equal(c.want) {
			t.Fatalf("overlapping effective at %ds = %v, want %v", c.now, got, c.want)
		}
	}
}

func TestIngestTracker_HoldBeforeFirstMarkerStaysZero(t *testing.T) {
	tr := newIngestTracker(2*time.Minute, 10)
	tr.hold(ingestAt(5), ingestAt(20))
	if got := tr.effective(ingestAt(6)); !got.IsZero() {
		t.Fatalf("effective = %v, want zero", got)
	}
}

func TestIngestTracker_PendingIDsByKind(t *testing.T) {
	tr := newIngestTracker(2*time.Minute, 10)
	tr.nak(1, ingestAt(0), 1, []string{taskKey("t1"), workerKey("w1")})
	tr.nak(2, ingestAt(0), 1, []string{taskKey("t2"), attemptKey("a1")})
	if got, want := tr.pendingIDs(keyKindTask, ingestAt(1)), map[string]bool{"t1": true, "t2": true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("task ids = %v, want %v", got, want)
	}
	if got, want := tr.pendingIDs(keyKindWorker, ingestAt(1)), map[string]bool{"w1": true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("worker ids = %v, want %v", got, want)
	}
	if got := tr.pendingIDs(keyKindTask, ingestAt(200)); len(got) != 0 {
		t.Fatalf("expired task ids = %v, want none", got)
	}
}

func TestIngestTracker_ConcurrentUse(t *testing.T) {
	tr := newIngestTracker(2*time.Minute, 10)
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 200 {
				seq := uint64(g*1000 + i)
				now := ingestAt(i)
				tr.marker(now)
				tr.nak(seq, now, 1, []string{taskKey(strconv.FormatUint(seq, 10))})
				tr.hold(now, now.Add(time.Second))
				_ = tr.effective(now)
				_ = tr.pending(taskKey("x"), now)
				_ = tr.pendingIDs(keyKindTask, now)
				_ = tr.pendingCount(now)
				_ = tr.lag(now)
				_ = tr.gap(now, time.Second)
				tr.ack(seq)
			}
		}(g)
	}
	wg.Wait()
	if got := tr.pendingCount(ingestAt(200)); got != 0 {
		t.Fatalf("pendingCount = %d, want 0", got)
	}
}

// TestIngestTracker_GapHoldsUntilAWholeWindowIsRead: after a gap, effective
// stays at the newest marker processed before it until the consumer has
// processed a marker a whole window past the first message after the gap.
func TestIngestTracker_GapHoldsUntilAWholeWindowIsRead(t *testing.T) {
	const window = 30 * time.Second
	tr := newIngestTracker(2*time.Minute, 10)
	tr.marker(ingestAt(10))

	// Messages stored after the marker at 10 s expired unread; the first
	// read after them is stamped 150 s.
	if started := tr.gap(ingestAt(150), window); !started {
		t.Fatal("first gap did not start a hold")
	}
	tr.marker(ingestAt(152))
	if got := tr.effective(ingestAt(153)); !got.Equal(ingestAt(10)) {
		t.Fatalf("effective just after the gap = %v, want %v (the marker before it)", got, ingestAt(10))
	}
	tr.marker(ingestAt(179))
	if got := tr.effective(ingestAt(180)); !got.Equal(ingestAt(10)) {
		t.Fatalf("effective at 179 s, short of 150 s + window = %v, want %v", got, ingestAt(10))
	}

	// A second gap before the release keeps the earlier hold time and moves
	// the release to its own first message plus the window.
	if started := tr.gap(ingestAt(175), window); started {
		t.Fatal("a gap inside a hold reported a new hold")
	}
	tr.marker(ingestAt(180))
	if got := tr.effective(ingestAt(181)); !got.Equal(ingestAt(10)) {
		t.Fatalf("effective at 180 s after a second gap = %v, want %v", got, ingestAt(10))
	}
	tr.marker(ingestAt(205))
	if got := tr.effective(ingestAt(206)); !got.Equal(ingestAt(205)) {
		t.Fatalf("effective at 175 s + window = %v, want %v (released)", got, ingestAt(205))
	}

	// Released: a later gap holds at the marker in force when it is seen.
	tr.gap(ingestAt(300), window)
	tr.marker(ingestAt(301))
	if got := tr.effective(ingestAt(302)); !got.Equal(ingestAt(205)) {
		t.Fatalf("effective after a new gap = %v, want %v", got, ingestAt(205))
	}
}

// TestIngestTracker_GapBeforeFirstMarkerHoldsAtZero: a gap seen before any
// marker leaves effective zero until the window after it has been read.
func TestIngestTracker_GapBeforeFirstMarkerHoldsAtZero(t *testing.T) {
	const window = 30 * time.Second
	tr := newIngestTracker(2*time.Minute, 10)
	tr.gap(ingestAt(100), window)
	tr.marker(ingestAt(101))
	if got := tr.effective(ingestAt(102)); !got.IsZero() {
		t.Fatalf("effective after a gap with no earlier marker = %v, want zero", got)
	}
	tr.marker(ingestAt(130))
	if got := tr.effective(ingestAt(131)); !got.Equal(ingestAt(130)) {
		t.Fatalf("effective at the release = %v, want %v", got, ingestAt(130))
	}
}

// TestIngestTracker_GapAndHoldTakeTheEarlier: a pause or interruption hold and
// a gap hold together cap effective at the earlier of the two.
func TestIngestTracker_GapAndHoldTakeTheEarlier(t *testing.T) {
	tr := newIngestTracker(2*time.Minute, 10)
	tr.marker(ingestAt(20))
	tr.hold(ingestAt(5), ingestAt(60))
	tr.gap(ingestAt(30), 30*time.Second)
	tr.marker(ingestAt(40))
	if got := tr.effective(ingestAt(41)); !got.Equal(ingestAt(5)) {
		t.Fatalf("effective under both = %v, want %v", got, ingestAt(5))
	}
	if got := tr.effective(ingestAt(61)); !got.Equal(ingestAt(20)) {
		t.Fatalf("effective once the hold ends = %v, want %v (the gap still holds)", got, ingestAt(20))
	}
}
