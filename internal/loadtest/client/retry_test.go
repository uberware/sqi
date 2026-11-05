// SPDX-License-Identifier: AGPL-3.0-or-later

package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
)

// throttler answers the first throttled requests with status (and a
// Retry-After of retryAfter, when set), then 201 with an id.
type throttler struct {
	throttled  int64
	status     int
	retryAfter string
	calls      atomic.Int64
}

func (th *throttler) start(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := th.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n <= th.throttled {
			if th.retryAfter != "" {
				w.Header().Set("Retry-After", th.retryAfter)
			}
			w.WriteHeader(th.status)
			w.Write([]byte(`{"detail":"slow down"}`)) //nolint:errcheck // test server
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"x1"}`)) //nolint:errcheck // test server
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestClient_RetriesThrottledCalls(t *testing.T) {
	defer client.SetRetryBackoff(5*time.Millisecond, 20*time.Millisecond)()
	type call func(context.Context, *client.Client) error
	createFarm := func(ctx context.Context, c *client.Client) error {
		_, err := c.CreateFarm(ctx, "f")
		return err
	}
	submit := func(ctx context.Context, c *client.Client) error {
		_, _, err := c.SubmitJob(ctx, client.SubmitRequest{Template: []byte("x")})
		return err
	}
	ready := func(ctx context.Context, c *client.Client) error { return c.Ready(ctx) }
	tests := []struct {
		name       string
		throttled  int64
		status     int
		budget     time.Duration
		fn         call
		wantStatus int // 0: success
		wantCalls  int64
	}{
		{"429s are retried until the call succeeds", 3, 429, time.Minute, createFarm, 0, 4},
		{"503s are retried too", 2, 503, time.Minute, createFarm, 0, 3},
		{"a 500 is not retried", 1, 500, time.Minute, createFarm, 500, 1},
		{"no budget, no retry", 1, 429, 0, createFarm, 429, 1},
		{"a submission's 429 is never retried", 1, 429, time.Minute, submit, 429, 1},
		{"a submission's 503 is never retried", 1, 503, time.Minute, submit, 503, 1},
		{"a readiness probe is never retried", 1, 503, time.Minute, ready, 503, 1},
		{"the budget runs out", 1000, 429, 50 * time.Millisecond, createFarm, 429, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			th := &throttler{throttled: tt.throttled, status: tt.status}
			ts := th.start(t)
			c := client.New(ts.URL, client.WithRetry(tt.budget))
			err := tt.fn(t.Context(), c)
			var apiErr *client.APIError
			switch {
			case tt.wantStatus == 0 && err != nil:
				t.Fatalf("err = %v, want success", err)
			case tt.wantStatus != 0 && (!errors.As(err, &apiErr) || apiErr.Status != tt.wantStatus):
				t.Fatalf("err = %v, want an HTTP %d", err, tt.wantStatus)
			}
			if tt.wantCalls != 0 && th.calls.Load() != tt.wantCalls {
				t.Errorf("server saw %d calls, want %d", th.calls.Load(), tt.wantCalls)
			}
		})
	}
}

func TestClient_RetryHonorsRetryAfter(t *testing.T) {
	th := &throttler{throttled: 1, status: 429, retryAfter: "1"}
	ts := th.start(t)
	start := time.Now()
	if _, err := client.New(ts.URL, client.WithRetry(time.Minute)).CreateFarm(t.Context(), "f"); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("retried after %s, want the 1 s Retry-After honored", elapsed)
	}
	if th.calls.Load() != 2 {
		t.Errorf("server saw %d calls, want 2", th.calls.Load())
	}
}

func TestClient_RetryStopsWithContext(t *testing.T) {
	th := &throttler{throttled: 1000, status: 429, retryAfter: "30"}
	ts := th.start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.New(ts.URL, client.WithRetry(time.Hour)).CreateFarm(ctx, "f")
	if err == nil {
		t.Fatal("CreateFarm succeeded against a server that only throttles")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("a canceled context took %s to end the retries", elapsed)
	}
}

func TestClient_WithoutRetry(t *testing.T) {
	defer client.SetRetryBackoff(5*time.Millisecond, 20*time.Millisecond)()
	th := &throttler{throttled: 1, status: http.StatusTooManyRequests}
	ts := th.start(t)
	c := client.New(ts.URL, client.WithRetry(time.Minute)).WithoutRetry()
	_, err := c.CreateFarm(t.Context(), "f")
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
		t.Fatalf("CreateFarm = %v, want the 429", err)
	}
	if n := th.calls.Load(); n != 1 {
		t.Errorf("server saw %d calls, want 1: the copy must not retry", n)
	}
}

func TestClient_RetryingReads(t *testing.T) {
	defer client.SetRetryBackoff(5*time.Millisecond, 20*time.Millisecond)()
	type call func(context.Context, *client.Client) error
	getJob := func(ctx context.Context, c *client.Client) error {
		_, err := c.GetJob(ctx, "j1")
		return err
	}
	createFarm := func(ctx context.Context, c *client.Client) error {
		_, err := c.CreateFarm(ctx, "f")
		return err
	}
	tests := []struct {
		name       string
		status     int
		reads      bool
		fn         call
		wantStatus int // 0: success
		wantCalls  int64
	}{
		{"a read's 500 is retried", 500, true, getJob, 0, 2},
		{"a read's 502 is retried", 502, true, getJob, 0, 2},
		{"a write's 500 is not", 500, true, createFarm, 500, 1},
		{"a write's 429 still is", 429, true, createFarm, 0, 2},
		{"without RetryingReads a read's 500 is not", 500, false, getJob, 500, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			th := &throttler{throttled: 1, status: tt.status}
			ts := th.start(t)
			c := client.New(ts.URL, client.WithRetry(time.Minute))
			if tt.reads {
				c = c.RetryingReads()
			}
			err := tt.fn(t.Context(), c)
			var apiErr *client.APIError
			switch {
			case tt.wantStatus == 0 && err != nil:
				t.Fatalf("err = %v, want success", err)
			case tt.wantStatus != 0 && (!errors.As(err, &apiErr) || apiErr.Status != tt.wantStatus):
				t.Fatalf("err = %v, want an HTTP %d", err, tt.wantStatus)
			}
			if n := th.calls.Load(); n != tt.wantCalls {
				t.Errorf("server saw %d calls, want %d", n, tt.wantCalls)
			}
		})
	}
}

// TestClient_RetryingReadsRetriesATransportError drops the first connection
// without an answer: a read from a RetryingReads client tries again.
func TestClient_RetryingReadsRetriesATransportError(t *testing.T) {
	defer client.SetRetryBackoff(5*time.Millisecond, 20*time.Millisecond)()
	var calls atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"j1"}`)) //nolint:errcheck // test server
	}))
	t.Cleanup(ts.Close)
	c := client.New(ts.URL, client.WithRetry(time.Minute)).RetryingReads()
	if _, err := c.GetJob(t.Context(), "j1"); err != nil {
		t.Fatalf("GetJob = %v, want success after one dropped connection", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server saw %d calls, want 2", n)
	}
}

// TestClient_RetryingReadsStopsOnceTheServerIsDown pins the breaker: once one
// read has retried a 5xx for the whole budget, later reads from the same copy
// fail at once, while a fresh copy retries again.
func TestClient_RetryingReadsStopsOnceTheServerIsDown(t *testing.T) {
	defer client.SetRetryBackoff(5*time.Millisecond, 10*time.Millisecond)()
	th := &throttler{throttled: 1 << 30, status: http.StatusInternalServerError}
	ts := th.start(t)
	base := client.New(ts.URL, client.WithRetry(50*time.Millisecond))
	c := base.RetryingReads()

	if _, err := c.GetJob(t.Context(), "j1"); err == nil {
		t.Fatal("GetJob succeeded against a server that only fails")
	}
	if n := th.calls.Load(); n < 2 {
		t.Fatalf("the first read was tried %d times, want it retried", n)
	}
	before := th.calls.Load()
	if _, err := c.GetJob(t.Context(), "j1"); err == nil {
		t.Fatal("second GetJob succeeded")
	}
	if n := th.calls.Load() - before; n != 1 {
		t.Errorf("a read after the budget ran out was tried %d times, want 1", n)
	}
	before = th.calls.Load()
	base.RetryingReads().GetJob(t.Context(), "j1") //nolint:errcheck // only the call count matters
	if n := th.calls.Load() - before; n < 2 {
		t.Errorf("a fresh copy tried the read %d times, want it retried", n)
	}
}
