// SPDX-License-Identifier: AGPL-3.0-or-later

package bus_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/bus/bustest"
)

const notMemberText = "and is not a member of this database: is this server pointed at the wrong database?"

// members is an IsMember that knows the given instances.
func members(ids ...string) func(string) bool {
	return func(id string) bool { return slices.Contains(ids, id) }
}

// guardKeyRevision is the guard key's revision, 0 when it is absent.
func guardKeyRevision(t *testing.T, js jetstream.JetStream) uint64 {
	t.Helper()
	kv, err := js.KeyValue(t.Context(), bus.GuardBucket)
	if err != nil {
		t.Fatalf("open guard bucket: %v", err)
	}
	entry, err := kv.Get(t.Context(), "active")
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return 0
	}
	if err != nil {
		t.Fatalf("read guard key: %v", err)
	}
	return entry.Revision()
}

// requireKeyHeldBy fails the test unless the guard key's value names holder.
func requireKeyHeldBy(t *testing.T, js jetstream.JetStream, holder bus.GuardHolder) {
	t.Helper()
	kv, err := js.KeyValue(t.Context(), bus.GuardBucket)
	if err != nil {
		t.Fatalf("open guard bucket: %v", err)
	}
	entry, err := kv.Get(t.Context(), "active")
	if err != nil {
		t.Fatalf("read guard key: %v", err)
	}
	var got bus.GuardHolder
	if err := json.Unmarshal(entry.Value(), &got); err != nil || got.InstanceID != holder.InstanceID {
		t.Errorf("guard key value = %q, want it to name %s (instance %s)", entry.Value(), holder.Host, holder.InstanceID)
	}
}

func TestGuardHA_MemberKeyOverwrittenAtOnce(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	loggerA, logsA := bufLogger()
	loggerB, _ := bufLogger()

	lostA := make(chan error, 4)
	holderA := guardHolder("render-a", 1)
	var aRef atomic.Pointer[bus.Guard]
	a, err := bus.AcquireGuardWith(t.Context(), js, 3, holderA, loggerA, bus.GuardOptions{
		OnLost: func(err error) {
			// The callback may ask the guard about itself.
			if g := aRef.Load(); g == nil || g.Healthy() == nil {
				t.Error("OnLost ran while the guard reported itself healthy")
			}
			lostA <- err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	aRef.Store(a)
	t.Cleanup(func() { release(t, a) })

	start := time.Now()
	holderB := guardHolder("render-b", 2)
	b, err := bus.AcquireGuardWith(t.Context(), js, 3, holderB, loggerB, bus.GuardOptions{IsMember: members(holderA.InstanceID)})
	if err != nil {
		t.Fatalf("acquire over a member's key: %v", err)
	}
	t.Cleanup(func() { release(t, b) })
	requireKeyHeldBy(t, js, holderB)
	if took := time.Since(start); took >= bus.GuardTTL() {
		t.Errorf("overwriting a member's key took %s, want it at once", took)
	}

	select {
	case err := <-lostA:
		if !errors.Is(err, bus.ErrGuardHeld) || !strings.Contains(err.Error(), "render-b") {
			t.Errorf("OnLost error = %v, want ErrGuardHeld naming render-b", err)
		}
	case <-time.After(bus.GuardTTL() + 5*time.Second):
		t.Fatal("OnLost was not called after the key was overwritten")
	}
	if err := a.Healthy(); !errors.Is(err, bus.ErrGuardHeld) {
		t.Errorf("overwritten holder Healthy = %v, want ErrGuardHeld", err)
	}
	// With OnLost set its owner reports the loss at ERROR, so the guard says
	// it at WARN: a stepping-down leader logs one ERROR, not two.
	if got := logsA.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "no longer holds the broker account") ||
		!strings.Contains(got, "steps down") || strings.Contains(got, "level=ERROR") || strings.Contains(got, "stop one of the two servers") {
		t.Errorf("the lost holder's log = %s; want one WARN saying it steps down", got)
	}
	if err := b.Healthy(); err != nil {
		t.Errorf("new holder Healthy = %v, want nil", err)
	}

	// The refresh loop ends with the loss, so the callback is not repeated.
	release(t, a)
	if n := len(lostA); n != 0 {
		t.Errorf("OnLost called %d more times", n)
	}
}

func TestGuardHA_NonMemberLiveKeyRefused(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, logs := bufLogger()

	a, err := bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-a", 4242), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(t, a) })

	start := time.Now()
	_, err = bus.AcquireGuardWith(t.Context(), js, 3, guardHolder("render-b", 7), logger, bus.GuardOptions{IsMember: members()})
	took := time.Since(start)
	if !errors.Is(err, bus.ErrGuardHeld) {
		t.Fatalf("acquire over a live non-member = %v, want ErrGuardHeld", err)
	}
	for _, want := range []string{"render-a", "4242", "or exited less than 30s ago", notMemberText} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	// The refusal comes as soon as a refresh is seen, at most one TTL on.
	if took > bus.GuardTTL()+time.Second {
		t.Errorf("refused after %s, want within one TTL (%s)", took, bus.GuardTTL())
	}
	if !strings.Contains(logs.String(), "watching it for one TTL") {
		t.Errorf("the watch was not logged: %s", logs.String())
	}
	if err := a.Healthy(); err != nil {
		t.Errorf("holder Healthy = %v, want nil", err)
	}
}

func TestGuardHA_NonMemberDeadKeyTaken(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, _ := bufLogger()

	dead, err := bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-a", 1), logger)
	if err != nil {
		t.Fatal(err)
	}
	bus.StopGuardRefresh(dead)

	start := time.Now()
	holderB := guardHolder("render-b", 2)
	b, err := bus.AcquireGuardWith(t.Context(), js, 3, holderB, logger, bus.GuardOptions{IsMember: members()})
	if err != nil {
		t.Fatalf("acquire over a dead non-member's key: %v", err)
	}
	t.Cleanup(func() { release(t, b) })
	requireKeyHeldBy(t, js, holderB)
	if took := time.Since(start); took > bus.GuardTTL()+3*time.Second {
		t.Errorf("taking the dead key took %s, want about one TTL (%s)", took, bus.GuardTTL())
	}
	release(t, dead) // must not delete the new holder's key
	if err := b.Healthy(); err != nil {
		t.Errorf("new holder Healthy = %v, want nil", err)
	}
	if guardKeyRevision(t, js) == 0 {
		t.Error("the dead holder's Release deleted the new holder's key")
	}
}

func TestGuardHA_RestoreHolderNotMember(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, _ := bufLogger()

	restore := guardHolder("render-r", 99)
	restore.Purpose = bus.GuardPurposeRestore
	r, err := bus.AcquireGuardWith(t.Context(), js, 3, restore, logger, bus.GuardOptions{})
	if err != nil {
		t.Fatal(err)
	}

	opts := bus.GuardOptions{IsMember: members()}
	_, err = bus.AcquireGuardWith(t.Context(), js, 3, guardHolder("render-b", 2), logger, opts)
	if !errors.Is(err, bus.ErrGuardHeld) {
		t.Fatalf("acquire over a refreshing restore = %v, want ErrGuardHeld", err)
	}
	for _, want := range []string{"sqi-server restore", notMemberText} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}

	release(t, r)
	start := time.Now()
	b, err := bus.AcquireGuardWith(t.Context(), js, 3, guardHolder("render-b", 2), logger, opts)
	if err != nil {
		t.Fatalf("acquire after the restore released: %v", err)
	}
	t.Cleanup(func() { release(t, b) })
	if took := time.Since(start); took >= bus.GuardTTL() {
		t.Errorf("acquiring a free key took %s, want no wait", took)
	}
}

func TestGuardHA_WatchHonoursCancellation(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, _ := bufLogger()

	a, err := bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-a", 1), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(t, a) })
	// With the holder not refreshing, the key neither advances nor expires
	// inside the context's window, so only the context can end the watch.
	bus.StopGuardRefresh(a)

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = bus.AcquireGuardWith(ctx, js, 3, guardHolder("render-b", 2), logger, bus.GuardOptions{IsMember: members()})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire with an expiring context = %v, want the context's error", err)
	}
	if took := time.Since(start); took >= bus.GuardTTL() {
		t.Errorf("the watch ran %s past the context's deadline", took)
	}
}

func TestGuardHA_OnLostNilIsSafe(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, _ := bufLogger()

	holderA := guardHolder("render-a", 1)
	a, err := bus.AcquireGuardWith(t.Context(), js, 3, holderA, logger, bus.GuardOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(t, a) })
	b, err := bus.AcquireGuardWith(t.Context(), js, 3, guardHolder("render-b", 2), logger, bus.GuardOptions{IsMember: members(holderA.InstanceID)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(t, b) })
	eventually(t, bus.GuardTTL()+5*time.Second, "the overwritten holder to notice", func() error {
		if a.Healthy() == nil {
			return errors.New("still healthy")
		}
		return nil
	})
}

func TestGuardHA_MemberContestedKeyEndsWithoutLooping(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, _ := bufLogger()

	// Two promoting servers race for one member's key: one wins, the other
	// ends with a result rather than retrying forever.
	holderA := guardHolder("render-a", 1)
	a, err := bus.AcquireGuard(t.Context(), js, 3, holderA, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(t, a) })
	opts := bus.GuardOptions{IsMember: members(holderA.InstanceID)}

	type result struct {
		g   *bus.Guard
		err error
	}
	results := make(chan result, 2)
	for _, h := range []bus.GuardHolder{guardHolder("render-b", 2), guardHolder("render-c", 3)} {
		go func() {
			g, err := bus.AcquireGuardWith(t.Context(), js, 3, h, logger, opts)
			results <- result{g, err}
		}()
	}
	var won int
	for range 2 {
		select {
		case r := <-results:
			if r.err == nil {
				won++
				t.Cleanup(func() { release(t, r.g) })
			}
		case <-time.After(3*bus.GuardTTL() + 10*time.Second):
			t.Fatal("a contested acquire did not end")
		}
	}
	if won == 0 {
		t.Error("no contender took the key")
	}
}

func TestCheckGuard_Cases(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, _ := bufLogger()
	ctx := t.Context()

	if err := bus.CheckGuard(ctx, js, bus.GuardOptions{IsMember: members()}, logger); err != nil {
		t.Fatalf("no bucket: CheckGuard = %v, want nil", err)
	}

	holder := guardHolder("render-a", 4242)
	g, err := bus.AcquireGuard(ctx, js, 3, holder, logger)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("held by a member", func(t *testing.T) {
		start := time.Now()
		if err := bus.CheckGuard(ctx, js, bus.GuardOptions{IsMember: members(holder.InstanceID)}, logger); err != nil {
			t.Errorf("CheckGuard = %v, want nil", err)
		}
		if took := time.Since(start); took >= bus.GuardTTL() {
			t.Errorf("a member's key took %s to judge, want no wait", took)
		}
	})

	t.Run("advancing non-member", func(t *testing.T) {
		before := guardKeyRevision(t, js)
		err := bus.CheckGuard(ctx, js, bus.GuardOptions{IsMember: members()}, logger)
		if !errors.Is(err, bus.ErrGuardHeld) {
			t.Fatalf("CheckGuard = %v, want ErrGuardHeld", err)
		}
		for _, want := range []string{"render-a", "4242", notMemberText} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
		if after := guardKeyRevision(t, js); after < before {
			t.Errorf("key revision went from %d to %d: CheckGuard must not rewrite it", before, after)
		}
	})

	t.Run("nil IsMember treats every holder as a non-member", func(t *testing.T) {
		if err := bus.CheckGuard(ctx, js, bus.GuardOptions{}, logger); !errors.Is(err, bus.ErrGuardHeld) {
			t.Errorf("CheckGuard = %v, want ErrGuardHeld", err)
		}
	})

	t.Run("non-member that stopped refreshing", func(t *testing.T) {
		bus.StopGuardRefresh(g)
		if err := bus.CheckGuard(ctx, js, bus.GuardOptions{IsMember: members()}, logger); err != nil {
			t.Errorf("CheckGuard = %v, want nil", err)
		}
	})

	t.Run("key absent", func(t *testing.T) {
		eventually(t, bus.GuardTTL()+5*time.Second, "the dead key to expire", func() error {
			if guardKeyRevision(t, js) != 0 {
				return errors.New("key still present")
			}
			return nil
		})
		start := time.Now()
		if err := bus.CheckGuard(ctx, js, bus.GuardOptions{IsMember: members()}, logger); err != nil {
			t.Errorf("CheckGuard = %v, want nil", err)
		}
		if took := time.Since(start); took >= bus.GuardTTL() {
			t.Errorf("an absent key took %s to judge, want no wait", took)
		}
		if rev := guardKeyRevision(t, js); rev != 0 {
			t.Errorf("CheckGuard created the key (revision %d)", rev)
		}
	})

	t.Run("canceled while watching", func(t *testing.T) {
		live, err := bus.AcquireGuard(ctx, js, 3, guardHolder("render-b", 5), logger)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { release(t, live) })
		bus.StopGuardRefresh(live) // so only the context can end the watch
		cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		err = bus.CheckGuard(cctx, js, bus.GuardOptions{IsMember: members()}, logger)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("CheckGuard with an expiring context = %v, want the context's error", err)
		}
	})
}

func TestGuard_ZeroOptionsUnchanged(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, logs := bufLogger()

	first, err := bus.AcquireGuardWith(t.Context(), js, 3, guardHolder("render-a", 4242), logger, bus.GuardOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(t, first) })

	start := time.Now()
	_, err = bus.AcquireGuardWith(t.Context(), js, 3, guardHolder("render-b", 7), logger, bus.GuardOptions{})
	if !errors.Is(err, bus.ErrGuardHeld) {
		t.Fatalf("second acquire = %v, want ErrGuardHeld", err)
	}
	if took := time.Since(start); took >= bus.GuardTTL() {
		t.Errorf("zero options waited %s, want an immediate refusal", took)
	}
	if strings.Contains(logs.String(), "watching") {
		t.Errorf("zero options watched the key: %s", logs.String())
	}
	for _, want := range []string{"render-a", "4242", "it is still running, or exited less than 30s ago"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "not a member") {
		t.Errorf("error %q carries the HA suffix with zero options", err)
	}
}

// OnLost fires for a zero-IsMember guard too: it is about the key, not the watch.
func TestGuardHA_OnLostFiresOnceForPlainGuard(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, _ := bufLogger()

	var calls atomic.Int32
	a, err := bus.AcquireGuardWith(t.Context(), js, 3, guardHolder("render-a", 1), logger, bus.GuardOptions{
		OnLost: func(error) { calls.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(t, a) })

	// Another server takes the key by compare-and-set, as a promoting leader would.
	kv, err := js.KeyValue(t.Context(), bus.GuardBucket)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := kv.Get(t.Context(), "active")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Update(t.Context(), "active", []byte(`{"instance_id":"other","host":"render-x","pid":3}`), entry.Revision()); err != nil {
		t.Fatal(err)
	}
	eventually(t, bus.GuardTTL()+5*time.Second, "OnLost", func() error {
		if calls.Load() == 0 {
			return errors.New("not called yet")
		}
		return nil
	})
	release(t, a) // ends the refresh loop, so nothing can call OnLost again
	if n := calls.Load(); n != 1 {
		t.Errorf("OnLost called %d times, want 1", n)
	}
}

func TestGuardHA_OnLostMayRelease(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, _ := bufLogger()

	holderA := guardHolder("render-a", 1)
	var aRef atomic.Pointer[bus.Guard]
	released := make(chan error, 1)
	a, err := bus.AcquireGuardWith(t.Context(), js, 3, holderA, logger, bus.GuardOptions{
		// A demoting leader releases the guard it lost.
		OnLost: func(error) { released <- aRef.Load().Release(context.Background()) },
	})
	if err != nil {
		t.Fatal(err)
	}
	aRef.Store(a)

	b, err := bus.AcquireGuardWith(t.Context(), js, 3, guardHolder("render-b", 2), logger, bus.GuardOptions{IsMember: members(holderA.InstanceID)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(t, b) })

	select {
	case err := <-released:
		if err != nil {
			t.Errorf("Release from OnLost = %v, want nil", err)
		}
	case <-time.After(bus.GuardTTL() + 10*time.Second):
		t.Fatal("Release called from OnLost did not return")
	}
	if guardKeyRevision(t, js) == 0 {
		t.Error("the lost holder's Release deleted the new holder's key")
	}
	if err := b.Healthy(); err != nil {
		t.Errorf("new holder Healthy = %v, want nil", err)
	}
}

// memberTakesDeadKeyDuringWatch stops dead refreshing and, once logs show a
// watch of its key has begun, overwrites the key for member, as a server of
// this database starting during another's watch would. The returned
// channel receives the overwrite's error, nil when it succeeded.
func memberTakesDeadKeyDuringWatch(t *testing.T, js jetstream.JetStream, dead *bus.Guard, member bus.GuardHolder, logs *lockedBuf) (done <-chan error) {
	t.Helper()
	bus.StopGuardRefresh(dead)
	kv, err := js.KeyValue(t.Context(), bus.GuardBucket)
	if err != nil {
		t.Fatal(err)
	}
	value, err := json.Marshal(member)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		for !strings.Contains(logs.String(), "watching it for one TTL") {
			if t.Context().Err() != nil {
				result <- t.Context().Err()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		entry, err := kv.Get(t.Context(), "active")
		if err == nil {
			_, err = kv.Update(t.Context(), "active", value, entry.Revision())
		}
		result <- err
	}()
	return result
}

func TestGuardHA_NonMemberKeyTakenByMemberDuringWatch(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, logs := bufLogger()

	dead, err := bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-dead", 1), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(t, dead) })
	member := guardHolder("render-m", 2)
	took := memberTakesDeadKeyDuringWatch(t, js, dead, member, logs)

	holderB := guardHolder("render-b", 3)
	b, err := bus.AcquireGuardWith(t.Context(), js, 3, holderB, logger, bus.GuardOptions{IsMember: members(member.InstanceID)})
	if err != nil {
		t.Fatalf("acquire when a member took the dead key during the watch = %v, want it taken", err)
	}
	t.Cleanup(func() { release(t, b) })
	requireKeyHeldBy(t, js, holderB)
	if err := <-took; err != nil {
		t.Fatalf("the member's overwrite: %v", err)
	}
	if err := b.Healthy(); err != nil {
		t.Errorf("new holder Healthy = %v, want nil", err)
	}
}

func TestCheckGuard_NonMemberKeyTakenByMemberDuringWatch(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, logs := bufLogger()

	dead, err := bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-dead", 1), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(t, dead) })
	member := guardHolder("render-m", 2)
	took := memberTakesDeadKeyDuringWatch(t, js, dead, member, logs)

	if err := bus.CheckGuard(t.Context(), js, bus.GuardOptions{IsMember: members(member.InstanceID)}, logger); err != nil {
		t.Fatalf("CheckGuard when a member took the dead key during the watch = %v, want nil", err)
	}
	if err := <-took; err != nil {
		t.Fatalf("the member's overwrite: %v", err)
	}
}
