// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func TestTransientJetStreamError(t *testing.T) {
	apiErr := func(code jetstream.ErrorCode) error {
		return fmt.Errorf("stream SQI_TASK: %w", &jetstream.APIError{Code: 503, ErrorCode: code})
	}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"no peers yet", apiErr(10005), true},
		{"cluster temporarily unavailable", apiErr(10008), true},
		{"cluster cannot handle request", apiErr(10009), true},
		{"insufficient resources", apiErr(10023), true},
		{"request timed out", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), true},
		{"no responders", nats.ErrNoResponders, true},
		{"jetstream not enabled (no responders)", jetstream.ErrJetStreamNotEnabled, true},
		{"not enabled for the account", jetstream.ErrJetStreamNotEnabledForAccount, false},
		{"replicas not supported", apiErr(10074), false},
		{"unrelated", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := transientJetStreamError(tt.err); got != tt.want {
				t.Fatalf("transientJetStreamError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestRetryUntil(t *testing.T) {
	transient := errors.New("transient")
	definite := errors.New("definite")
	isTransient := func(err error) bool { return errors.Is(err, transient) }

	t.Run("retries a transient error until it succeeds", func(t *testing.T) {
		calls := 0
		err := retryUntil(t.Context(), time.Now().Add(5*time.Second), func(context.Context) error {
			calls++
			if calls < 3 {
				return transient
			}
			return nil
		}, isTransient)
		if err != nil || calls != 3 {
			t.Fatalf("retryUntil = %v after %d calls, want nil after 3", err, calls)
		}
	})

	t.Run("a definite error stops at once", func(t *testing.T) {
		calls := 0
		err := retryUntil(t.Context(), time.Now().Add(5*time.Second), func(context.Context) error {
			calls++
			return definite
		}, isTransient)
		if !errors.Is(err, definite) || calls != 1 {
			t.Fatalf("retryUntil = %v after %d calls, want definite after 1", err, calls)
		}
	})

	t.Run("the deadline ends the retries with the last error", func(t *testing.T) {
		began := time.Now()
		err := retryUntil(t.Context(), began.Add(600*time.Millisecond), func(context.Context) error { return transient }, isTransient)
		if !errors.Is(err, transient) || time.Since(began) > 3*time.Second {
			t.Fatalf("retryUntil = %v after %s, want transient soon after the deadline", err, time.Since(began))
		}
	})

	t.Run("op runs on a context that ends at the deadline", func(t *testing.T) {
		deadline := time.Now().Add(5 * time.Second)
		err := retryUntil(t.Context(), deadline, func(ctx context.Context) error {
			if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) {
				return fmt.Errorf("op context deadline = %v, %v; want %v", got, ok, deadline)
			}
			return nil
		}, isTransient)
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("no attempt starts after the deadline", func(t *testing.T) {
		deadline := time.Now().Add(600 * time.Millisecond)
		var late []time.Duration
		err := retryUntil(t.Context(), deadline, func(context.Context) error {
			if d := time.Since(deadline); d >= 0 {
				late = append(late, d)
			}
			return transient
		}, isTransient)
		if !errors.Is(err, transient) {
			t.Fatalf("retryUntil = %v, want the last transient error", err)
		}
		if len(late) > 0 {
			t.Fatalf("%d attempts started after the deadline, the first %s late", len(late), late[0])
		}
		if over := time.Since(deadline); over > 100*time.Millisecond {
			t.Fatalf("retryUntil returned %s after the deadline, want at it", over)
		}
	})

	t.Run("an attempt cut off at the deadline keeps the reason for retrying", func(t *testing.T) {
		calls := 0
		err := retryUntil(t.Context(), time.Now().Add(time.Second), func(ctx context.Context) error {
			calls++
			if calls == 1 {
				return transient
			}
			<-ctx.Done() // the slow attempt the deadline ends
			return ctx.Err()
		}, func(err error) bool { return errors.Is(err, transient) || errors.Is(err, context.DeadlineExceeded) })
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, transient) {
			t.Fatalf("retryUntil = %v, want the deadline wrapping the previous transient error", err)
		}
	})

	t.Run("a canceled context ends the retries", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := retryUntil(ctx, time.Now().Add(time.Minute), func(context.Context) error { return transient }, isTransient)
		if !errors.Is(err, context.Canceled) || !errors.Is(err, transient) {
			t.Fatalf("retryUntil = %v, want context.Canceled wrapping the last error", err)
		}
	})
}

// TestRestoreLock_TargetDeadline pins that each purge target gets its own
// budget of at least one full purge request, however short the connect
// timeout, so one slow purge cannot leave a later target no attempt.
func TestRestoreLock_TargetDeadline(t *testing.T) {
	for _, tt := range []struct {
		name    string
		connect time.Duration
		want    time.Duration
	}{
		{"a short connect timeout still allows one full purge", 5 * time.Second, purgeOpTimeout},
		{"a longer connect timeout is the budget", 2 * purgeOpTimeout, 2 * purgeOpTimeout},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := &RestoreLock{r: &Remote{cfg: RemoteConfig{ConnectTimeout: tt.connect}}}
			before := time.Now()
			first := l.targetDeadline()
			if got := first.Sub(before); got < tt.want || got > tt.want+time.Second {
				t.Fatalf("budget = %s, want %s", got, tt.want)
			}
			time.Sleep(10 * time.Millisecond)
			if second := l.targetDeadline(); !second.After(first) {
				t.Fatal("a later target got the earlier target's deadline; the budget must not be shared")
			}
		})
	}
}

// TestConnState_NilRecordsNothing: a connection whose state nobody reads is
// given a nil *connState, and its handlers must not panic on it.
func TestConnState_NilRecordsNothing(*testing.T) {
	var s *connState
	s.connected()
	s.up()
	s.down()
	s.initialUp()
}

// TestConnState_Records: a connect sets since, a drop clears it, and a
// connect found after a handler ran does not undo the handler.
func TestConnState_Records(t *testing.T) {
	var s connState
	s.initialUp()
	if s.get().IsZero() {
		t.Fatal("initialUp left since zero")
	}
	s.down()
	if !s.get().IsZero() {
		t.Fatal("down left since set")
	}
	s.initialUp()
	if !s.get().IsZero() {
		t.Fatal("initialUp after a handler undid its drop")
	}
	s.up()
	if s.get().IsZero() {
		t.Fatal("up left since zero")
	}
}
