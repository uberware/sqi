// SPDX-License-Identifier: AGPL-3.0-or-later

package preempt_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/preempt"
)

const testInterval = 10 * time.Millisecond

// syncBuffer is a bytes.Buffer safe for the logger and the test to share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// reply writes body to the response; a write error to a test client is not
// something a handler can act on.
func reply(w http.ResponseWriter, body string) {
	if _, err := io.WriteString(w, body); err != nil {
		return
	}
}

// loadString reads a string stored in v, or "" when nothing was stored.
func loadString(v *atomic.Value) string {
	s, ok := v.Load().(string)
	if !ok {
		return ""
	}
	return s
}

func debugLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// runWatcher runs a watcher against srv until it reports a notice or wait
// elapses, and returns how many times onNotice ran.
func runWatcher(t *testing.T, vendor string, srv *httptest.Server, logger *slog.Logger, wait time.Duration) int {
	t.Helper()
	w, err := preempt.New(vendor, srv.URL, testInterval, srv.Client(), logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx, func() { calls.Add(1) })
	}()
	select {
	case <-done:
	case <-time.After(wait + 5*time.Second):
		t.Fatal("Run did not return")
	}
	return int(calls.Load())
}

func TestWatcher_AWSNotice(t *testing.T) {
	var sawTTL, sawToken atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			if r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") != "" {
				sawTTL.Store(true)
			}
			reply(w, "tok-123")
		case r.Method == http.MethodGet && r.URL.Path == "/latest/meta-data/spot/instance-action":
			if r.Header.Get("X-aws-ec2-metadata-token") == "tok-123" {
				sawToken.Store(true)
			}
			reply(w, `{"action":"terminate","time":"2026-11-08T12:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if got := runWatcher(t, "aws", srv, debugLogger(io.Discard), 5*time.Second); got != 1 {
		t.Fatalf("onNotice calls = %d, want exactly 1", got)
	}
	if !sawTTL.Load() {
		t.Error("the token request carried no X-aws-ec2-metadata-token-ttl-seconds header")
	}
	if !sawToken.Load() {
		t.Error("the instance-action request did not carry the token it was issued")
	}
}

func TestWatcher_AWSNoNotice(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest/api/token" {
			reply(w, "tok")
			return
		}
		polls.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	if got := runWatcher(t, "aws", srv, debugLogger(io.Discard), 300*time.Millisecond); got != 0 {
		t.Fatalf("onNotice calls = %d, want 0 while the endpoint answers 404", got)
	}
	if polls.Load() < 2 {
		t.Errorf("instance-action polled %d times, want it polled repeatedly", polls.Load())
	}
}

func TestWatcher_GCPNotice(t *testing.T) {
	var calls atomic.Int32
	var flavor atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/computeMetadata/v1/instance/preempted" {
			http.NotFound(w, r)
			return
		}
		flavor.Store(r.Header.Get("Metadata-Flavor"))
		// FALSE for the first two polls, then TRUE.
		if calls.Add(1) < 3 {
			reply(w, "FALSE")
			return
		}
		reply(w, "TRUE\n")
	}))
	defer srv.Close()

	if got := runWatcher(t, "gcp", srv, debugLogger(io.Discard), 5*time.Second); got != 1 {
		t.Fatalf("onNotice calls = %d, want exactly 1", got)
	}
	if f := loadString(&flavor); f != "Google" {
		t.Errorf("Metadata-Flavor = %q, want Google", f)
	}
	if calls.Load() < 3 {
		t.Errorf("polled %d times, want the FALSE answers to have been ignored", calls.Load())
	}
}

func azureBody(types ...string) string {
	events := make([]string, 0, len(types))
	for i, ty := range types {
		events = append(events, fmt.Sprintf(`{"EventId":"e%d","EventType":%q,"Resources":["vm1"],"EventStatus":"Scheduled"}`, i, ty))
	}
	return `{"DocumentIncarnation":1,"Events":[` + strings.Join(events, ",") + `]}`
}

func azureServer(body string, header *atomic.Value, queries *atomic.Value) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metadata/scheduledevents" {
			http.NotFound(w, r)
			return
		}
		if header != nil {
			header.Store(r.Header.Get("Metadata"))
		}
		if queries != nil {
			queries.Store(r.URL.Query().Get("api-version"))
		}
		reply(w, body)
	}))
}

func TestWatcher_AzurePreemptEvent(t *testing.T) {
	var header, version atomic.Value
	srv := azureServer(azureBody("Freeze", "Preempt"), &header, &version)
	defer srv.Close()

	if got := runWatcher(t, "azure", srv, debugLogger(io.Discard), 5*time.Second); got != 1 {
		t.Fatalf("onNotice calls = %d, want exactly 1", got)
	}
	if h := loadString(&header); h != "true" {
		t.Errorf("Metadata header = %q, want true", h)
	}
	if v := loadString(&version); v != "2020-07-01" {
		t.Errorf("api-version = %q, want 2020-07-01", v)
	}
}

func TestWatcher_AzureOtherEventIgnored(t *testing.T) {
	// Reboot, Redeploy, Freeze and Terminate are the platform's own business,
	// not a spot eviction: only Preempt is a notice.
	srv := azureServer(azureBody("Reboot", "Redeploy", "Freeze", "Terminate"), nil, nil)
	defer srv.Close()

	if got := runWatcher(t, "azure", srv, debugLogger(io.Discard), 300*time.Millisecond); got != 0 {
		t.Fatalf("onNotice calls = %d, want 0 for events that are not Preempt", got)
	}
}

// TestWatcher_FailuresDoNotStop pins that a metadata endpoint that errors, hangs
// up or returns garbage never ends the watcher, that the failures are logged,
// and that the notice is still delivered once the endpoint recovers.
func TestWatcher_FailuresDoNotStop(t *testing.T) {
	const failures = 12
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n := calls.Add(1); n <= failures {
			if n%2 == 0 {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			reply(w, "not json") // an unparsable answer
			return
		}
		reply(w, azureBody("Preempt"))
	}))
	defer srv.Close()

	logs := &syncBuffer{}
	if got := runWatcher(t, "azure", srv, debugLogger(logs), 10*time.Second); got != 1 {
		t.Fatalf("onNotice calls = %d, want 1 after the endpoint recovered", got)
	}
	out := logs.String()
	if !strings.Contains(out, "level=DEBUG") {
		t.Errorf("failed polls were not logged at debug:\n%s", out)
	}
	if n := strings.Count(out, "metadata service has not answered"); n != 1 {
		t.Errorf("warnings = %d, want exactly one after 10 consecutive failures (rate limited):\n%s", n, out)
	}
}

func TestWatcher_RequestTimeout(t *testing.T) {
	// A hung endpoint must not hold a poll forever: the request times out and
	// the next poll goes out.
	release := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		reply(w, azureBody("Preempt"))
	}))
	defer srv.Close()
	defer close(release)

	start := time.Now()
	if got := runWatcher(t, "azure", srv, debugLogger(io.Discard), 10*time.Second); got != 1 {
		t.Fatalf("onNotice calls = %d, want 1", got)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("recovery took %s, want about the 2 s request timeout", d)
	}
}

func TestWatcher_StopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	w, err := preempt.New("gcp", srv.URL, testInterval, srv.Client(), debugLogger(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx, func() { calls.Add(1) })
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was canceled")
	}
	if calls.Load() != 0 {
		t.Errorf("onNotice ran %d times on cancel, want 0", calls.Load())
	}
}

func TestNew(t *testing.T) {
	tests := []struct {
		name     string
		vendor   string
		interval time.Duration
		wantErr  string
	}{
		{"aws", "aws", time.Second, ""},
		{"gcp", "gcp", time.Second, ""},
		{"azure", "azure", time.Second, ""},
		{"none is not a watcher", "none", time.Second, `unknown preemption vendor "none"`},
		{"unknown", "oracle", time.Second, `unknown preemption vendor "oracle"`},
		{"zero interval", "aws", 0, "poll interval"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := preempt.New(tt.vendor, "", tt.interval, nil, debugLogger(io.Discard))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("New: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("New error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestNew_DefaultBases pins the documented defaults without reaching the
// network: the watcher exposes the base it will poll.
func TestNew_DefaultBases(t *testing.T) {
	tests := []struct {
		vendor, endpoint, want string
	}{
		{"aws", "", "http://169.254.169.254"},
		{"azure", "", "http://169.254.169.254"},
		{"gcp", "", "http://metadata.google.internal"},
		{"aws", "http://proxy.example:8080/", "http://proxy.example:8080"},
	}
	for _, tt := range tests {
		w, err := preempt.New(tt.vendor, tt.endpoint, time.Second, nil, debugLogger(io.Discard))
		if err != nil {
			t.Fatal(err)
		}
		if got := w.Base(); got != tt.want {
			t.Errorf("Base(%s, %q) = %q, want %q", tt.vendor, tt.endpoint, got, tt.want)
		}
	}
}

// TestNew_DefaultClientIgnoresProxy checks that the default client never sends
// a metadata poll through a proxy named in the environment, and that an
// injected client is used as given.
func TestNew_DefaultClientIgnoresProxy(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	proxyURL := closed.URL
	closed.Close()
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		t.Setenv(k, proxyURL)
	}
	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reply(w, "TRUE")
	}))
	defer srv.Close()

	w, err := preempt.New("gcp", srv.URL, testInterval, nil, debugLogger(io.Discard))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tr, ok := w.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default client transport = %T, want *http.Transport", w.Client().Transport)
	}
	if tr.Proxy != nil {
		t.Error("default client transport has a Proxy function, want none")
	}
	if tr == http.DefaultTransport {
		t.Error("default client shares http.DefaultTransport, want a copy")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var calls atomic.Int32
	w.Run(ctx, func() { calls.Add(1) })
	if calls.Load() != 1 {
		t.Fatalf("onNotice calls = %d, want 1 with HTTP_PROXY pointing at a closed port", calls.Load())
	}

	injected := srv.Client()
	w, err = preempt.New("gcp", srv.URL, testInterval, injected, debugLogger(io.Discard))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if w.Client() != injected {
		t.Error("an injected client was replaced")
	}
}

// TestWatcher_AWSStopOrHibernateIsNoReclaim pins that a spot notice whose
// action keeps the instance (stop, hibernate) does not fire, while terminate,
// an action the watcher does not know and a document that does not parse all
// do: only a notice known to keep the machine is ignored.
func TestWatcher_AWSStopOrHibernateIsNoReclaim(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		want      int
	}{
		{"stop", `{"action":"stop","time":"2026-11-08T12:00:00Z"}`, 0},
		{"hibernate", `{"action":"hibernate","time":"2026-11-08T12:00:00Z"}`, 0},
		{"terminate", `{"action":"terminate","time":"2026-11-08T12:00:00Z"}`, 1},
		{"unknown action", `{"action":"evaporate"}`, 1},
		{"unparsable", `not json`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/latest/api/token" {
					reply(w, "tok")
					return
				}
				reply(w, tc.doc)
			}))
			defer srv.Close()
			wait := 300 * time.Millisecond
			if tc.want > 0 {
				wait = 5 * time.Second
			}
			if got := runWatcher(t, "aws", srv, debugLogger(io.Discard), wait); got != tc.want {
				t.Fatalf("onNotice calls = %d, want %d", got, tc.want)
			}
		})
	}
}
