// SPDX-License-Identifier: AGPL-3.0-or-later

package bus_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/bus/bustest"
)

// release gives a guard up and fails the test if it cannot.
func release(t *testing.T, g *bus.Guard) {
	t.Helper()
	if err := g.Release(context.Background()); err != nil {
		t.Errorf("Release: %v", err)
	}
}

func guardHolder(host string, pid int) bus.GuardHolder {
	return bus.GuardHolder{InstanceID: bus.NewInstanceID(), Host: host, PID: pid, StartedAt: time.Now()}
}

func TestGuard_SecondServerRefusedNamingHolder(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, _ := bufLogger()

	first, err := bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-a", 4242), logger)
	if err != nil {
		t.Fatalf("first AcquireGuard: %v", err)
	}
	t.Cleanup(func() { release(t, first) })

	_, err = bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-b", 7), logger)
	if !errors.Is(err, bus.ErrGuardHeld) {
		t.Fatalf("second AcquireGuard = %v, want it to wrap ErrGuardHeld", err)
	}
	for _, want := range []string{"render-a", "4242", "or exited less than 30s ago"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if err := first.Healthy(); err != nil {
		t.Errorf("first holder Healthy = %v, want nil", err)
	}
}

func TestGuard_KilledHolderExpires(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, _ := bufLogger()

	killed, err := bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-a", 1), logger)
	if err != nil {
		t.Fatal(err)
	}
	bus.StopGuardRefresh(killed)

	if _, err := bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-b", 2), logger); !errors.Is(err, bus.ErrGuardHeld) {
		t.Fatalf("acquire straight after the holder stopped refreshing = %v, want ErrGuardHeld", err)
	}
	var second *bus.Guard
	eventually(t, bus.GuardTTL()+10*time.Second, "acquire after the TTL", func() error {
		var err error
		second, err = bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-b", 2), logger)
		return err
	})
	t.Cleanup(func() { release(t, second) })
	release(t, killed) // must not delete the new holder's key
	if _, err := bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-c", 3), logger); !errors.Is(err, bus.ErrGuardHeld) {
		t.Fatalf("after the dead holder's Release = %v, want the live holder still in place", err)
	}
}

func TestGuard_CleanReleaseFreesImmediately(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, _ := bufLogger()

	first, err := bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-a", 1), logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := first.Release(t.Context()); err != nil {
		t.Fatalf("second Release = %v, want nil", err)
	}
	second, err := bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-b", 2), logger)
	if err != nil {
		t.Fatalf("acquire straight after a clean release: %v", err)
	}
	t.Cleanup(func() { release(t, second) })
}

func TestGuard_CASRefreshNeverOverwritesNewHolder(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	loggerA, logsA := bufLogger()
	loggerB, _ := bufLogger()

	a, err := bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-a", 1), loggerA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(t, a) })
	resume := bus.PauseGuardRefresh(a)

	var b *bus.Guard
	eventually(t, bus.GuardTTL()+10*time.Second, "B takes the expired key", func() error {
		var err error
		b, err = bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-b", 2), loggerB)
		return err
	})
	t.Cleanup(func() { release(t, b) })
	kv, err := js.KeyValue(t.Context(), bus.GuardBucket)
	if err != nil {
		t.Fatal(err)
	}
	before, err := kv.Get(t.Context(), "active")
	if err != nil {
		t.Fatal(err)
	}

	resume()
	eventually(t, 10*time.Second, "A learns it lost the account", func() error {
		if err := a.Healthy(); !errors.Is(err, bus.ErrGuardHeld) {
			return fmt.Errorf("A Healthy = %w, want it to wrap ErrGuardHeld", err)
		}
		return nil
	})

	after, err := kv.Get(t.Context(), "active")
	if err != nil {
		t.Fatal(err)
	}
	var holder bus.GuardHolder
	if err := json.Unmarshal(after.Value(), &holder); err != nil {
		t.Fatal(err)
	}
	if holder.Host != "render-b" || string(after.Value()) != string(before.Value()) {
		t.Errorf("key now holds %s, want B's value %s unchanged", after.Value(), before.Value())
	}
	if err := b.Healthy(); err != nil {
		t.Errorf("B Healthy = %v, want nil", err)
	}
	if got := logsA.String(); !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "no longer holds the broker account") {
		t.Errorf("A's log has no ERROR saying it lost the account:\n%s", got)
	}
	// A's release must not delete B's key.
	if err := a.Release(t.Context()); err != nil {
		t.Errorf("A Release = %v", err)
	}
	if _, err := kv.Get(t.Context(), "active"); err != nil {
		t.Errorf("B's key after A's Release: %v", err)
	}
}

func TestRemote_RefusesSecondServerOnSameAccount(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	logger, _ := bufLogger()
	first := startRemote(t, remoteConfig(c, bus.DefaultAccount, 3), logger)

	second := bus.NewRemote(remoteConfig(c, bus.DefaultAccount, 3), logger)
	host, herr := os.Hostname()
	if herr != nil {
		t.Fatal(herr)
	}
	err := second.Start(t.Context())
	if err == nil {
		second.Shutdown()
		t.Fatal("second Remote.Start on the same account succeeded, want it refused")
	}
	if !errors.Is(err, bus.ErrGuardHeld) || !strings.Contains(err.Error(), host) || !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Errorf("second Start = %v, want ErrGuardHeld naming host %q and pid %d", err, host, os.Getpid())
	}
	second.Shutdown()
	if err := first.Check(t.Context()); err != nil {
		t.Errorf("first Check after the refused second Start = %v, want nil", err)
	}

	// A clean shutdown frees the account at once.
	first.Shutdown()
	third := startRemote(t, remoteConfig(c, bus.DefaultAccount, 3), logger)
	if err := third.Check(t.Context()); err != nil {
		t.Errorf("Check after taking over a released account = %v", err)
	}
}

// takeOverExpired waits for the key of a holder whose refresh is paused to
// expire, then has h take it. It returns the new guard.
func takeOverExpired(t *testing.T, js jetstream.JetStream, h bus.GuardHolder) *bus.Guard {
	t.Helper()
	logger, _ := bufLogger()
	var g *bus.Guard
	eventually(t, bus.GuardTTL()+10*time.Second, "take the expired key", func() error {
		var err error
		g, err = bus.AcquireGuard(t.Context(), js, 3, h, logger)
		return err
	})
	return g
}

func TestGuard_OwnKeyFromTimedOutCreateIsAcquired(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, _ := bufLogger()
	h := guardHolder("render-a", 1)

	// A Create that timed out client-side but committed leaves this
	// server's own value in the key, which the retry then finds.
	seeded, err := bus.AcquireGuard(t.Context(), js, 3, h, logger)
	if err != nil {
		t.Fatal(err)
	}
	bus.StopGuardRefresh(seeded)

	again, err := bus.AcquireGuard(t.Context(), js, 3, h, logger)
	if err != nil {
		t.Fatalf("AcquireGuard with the caller's own value already in the key = %v, want it acquired", err)
	}
	t.Cleanup(func() { release(t, again) })
	if err := again.Healthy(); err != nil {
		t.Errorf("Healthy = %v, want nil", err)
	}
}

func TestGuard_RefreshKeepsKeyPastTTL(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, _ := bufLogger()

	first, err := bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-a", 1), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(t, first) })

	time.Sleep(2*bus.GuardTTL() + time.Second)
	if _, err := bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-b", 2), logger); !errors.Is(err, bus.ErrGuardHeld) {
		t.Fatalf("AcquireGuard after more than two TTLs = %v, want ErrGuardHeld: the refresh must keep the key alive", err)
	}
	if err := first.Healthy(); err != nil {
		t.Errorf("Healthy after more than two TTLs = %v, want nil", err)
	}
}

func TestGuard_ExpiredKeyNobodyTookIsRetaken(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, logs := bufLogger()

	a, err := bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-a", 1), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(t, a) })
	resume := bus.PauseGuardRefresh(a)
	time.Sleep(bus.GuardTTL() + time.Second)
	resume()

	eventually(t, 10*time.Second, "A retakes the expired key", a.Healthy)
	if !strings.Contains(logs.String(), "retaken") {
		t.Errorf("no log line saying the guard was retaken:\n%s", logs)
	}
	if _, err := bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-b", 2), logger); !errors.Is(err, bus.ErrGuardHeld) {
		t.Errorf("AcquireGuard after A retook = %v, want ErrGuardHeld", err)
	}
}

func TestGuard_AccountHeldThenReleasedMeanwhileIsLost(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, logs := bufLogger()

	a, err := bus.AcquireGuard(t.Context(), js, 3, guardHolder("render-a", 1), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(t, a) })
	resume := bus.PauseGuardRefresh(a)

	// B holds the account while A is frozen, then leaves cleanly: the key is
	// gone when A wakes, but two servers did run on the account.
	b := takeOverExpired(t, js, guardHolder("render-b", 2))
	release(t, b)
	resume()

	eventually(t, 10*time.Second, "A learns it lost the account", func() error {
		if err := a.Healthy(); !errors.Is(err, bus.ErrGuardHeld) {
			return fmt.Errorf("A Healthy = %w, want it to wrap ErrGuardHeld", err)
		}
		return nil
	})
	if got := logs.String(); !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "no longer holds the broker account") {
		t.Errorf("A's log has no ERROR saying it lost the account:\n%s", got)
	}
	if strings.Contains(logs.String(), "retaken") {
		t.Errorf("A retook an account another server had held:\n%s", logs)
	}
}

func TestRemote_CheckFailsAfterGuardLost(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	logger, _ := bufLogger()
	r := startRemote(t, remoteConfig(c, bus.DefaultAccount, 3), logger)
	if err := r.Check(t.Context()); err != nil {
		t.Fatalf("Check = %v, want nil", err)
	}

	resume := bus.PauseGuardRefresh(bus.RemoteGuard(r))
	thief := takeOverExpired(t, js, guardHolder("render-b", 2))
	t.Cleanup(func() { release(t, thief) })
	resume()

	eventually(t, 10*time.Second, "Check reports the lost account", func() error {
		if err := r.Check(t.Context()); !errors.Is(err, bus.ErrGuardHeld) {
			return fmt.Errorf("Check = %w, want it to wrap ErrGuardHeld", err)
		}
		return nil
	})
}

func TestRemote_RefusedSecondServerLeavesReplicasAlone(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	logger, _ := bufLogger()
	startRemote(t, remoteConfig(c, bus.DefaultAccount, 3), logger)
	js := dataJS(t, c)

	// A staging server that differs only in stream_replicas.
	second := bus.NewRemote(remoteConfig(c, bus.DefaultAccount, 1), logger)
	err := second.Start(t.Context())
	if err == nil {
		second.Shutdown()
		t.Fatal("second Start with other stream_replicas succeeded, want it refused")
	}
	if !errors.Is(err, bus.ErrGuardHeld) {
		t.Fatalf("second Start = %v, want ErrGuardHeld", err)
	}
	second.Shutdown()

	for _, name := range append(allStreams, "KV_"+bus.GuardBucket) {
		if got := streamInfo(t, js, name).Config.Replicas; got != 3 {
			t.Errorf("%s: replicas = %d after a refused server configured for 1, want 3", name, got)
		}
	}
}
