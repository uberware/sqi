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
		err := retryUntil(t.Context(), time.Now().Add(5*time.Second), func() error {
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
		err := retryUntil(t.Context(), time.Now().Add(5*time.Second), func() error {
			calls++
			return definite
		}, isTransient)
		if !errors.Is(err, definite) || calls != 1 {
			t.Fatalf("retryUntil = %v after %d calls, want definite after 1", err, calls)
		}
	})

	t.Run("the deadline ends the retries with the last error", func(t *testing.T) {
		began := time.Now()
		err := retryUntil(t.Context(), began.Add(600*time.Millisecond), func() error { return transient }, isTransient)
		if !errors.Is(err, transient) || time.Since(began) > 3*time.Second {
			t.Fatalf("retryUntil = %v after %s, want transient soon after the deadline", err, time.Since(began))
		}
	})

	t.Run("a canceled context ends the retries", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := retryUntil(ctx, time.Now().Add(time.Minute), func() error { return transient }, isTransient)
		if !errors.Is(err, context.Canceled) || !errors.Is(err, transient) {
			t.Fatalf("retryUntil = %v, want context.Canceled wrapping the last error", err)
		}
	})
}
