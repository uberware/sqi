// SPDX-License-Identifier: AGPL-3.0-or-later

package observe_test

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/internal/testserver"
	"github.com/uberware/sqi/internal/loadtest/measure"
	"github.com/uberware/sqi/internal/loadtest/observe"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
)

// fakeTarget is a target.Target whose addresses and data directory the test
// chooses.
type fakeTarget struct {
	httpURL, natsURL, dataDir, apiKey string
	queueIDs                          map[string]string
}

func (f fakeTarget) HTTPURL() string                  { return f.httpURL }
func (f fakeTarget) NATSURL() string                  { return f.natsURL }
func (f fakeTarget) APIKey() string                   { return f.apiKey }
func (fakeTarget) FarmID() string                     { return "farm" }
func (f fakeTarget) QueueIDs() map[string]string      { return f.queueIDs }
func (f fakeTarget) DataDir() string                  { return f.dataDir }
func (fakeTarget) Credentials() simworker.Credentials { return simworker.NoCredentials{} }
func (fakeTarget) TLS() simworker.TLSFiles            { return simworker.TLSFiles{} }
func (fakeTarget) NoopCommand() scenario.Command      { return scenario.Command{} }
func (fakeTarget) Close(context.Context) error        { return nil }
func (fakeTarget) PoolName(name string) string        { return name }
func (fakeTarget) PostgresDSN() string                { return "" }
func (fakeTarget) StoreLabel() string                 { return "" }

func capture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/metrics.txt")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// serve answers every request with body.
func serve(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write(body); err != nil {
			return
		}
	}
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// deadPort returns a loopback address nothing listens on.
func deadPort(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func seriesNames(rec *measure.Recorder) []string {
	var names []string
	for _, s := range rec.Series() {
		names = append(names, s.Name)
	}
	return names
}

func hasPrefixed(names []string, prefix string) bool {
	return slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, prefix) })
}

func unavailableFor(list []string, measurement string) (string, bool) {
	for _, u := range list {
		if strings.HasPrefix(u, measurement+": ") {
			return u, true
		}
	}
	return "", false
}

func TestObserver_ReportsUnavailable(t *testing.T) {
	metrics := capture(t)
	var pprofHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", serve(metrics))
	mux.HandleFunc("/debug/pprof/", func(w http.ResponseWriter, _ *http.Request) {
		pprofHits.Add(1)
		http.NotFound(w, nil)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rec := measure.NewRecorder()
	tgt := fakeTarget{httpURL: srv.URL, natsURL: "nats://" + deadPort(t)}
	o := observe.New(tgt, rec, observe.Config{
		Interval:     30 * time.Millisecond,
		ProfileEvery: 40 * time.Millisecond,
		ProfileDir:   t.TempDir(),
	}, quiet())

	ctx, cancel := context.WithTimeout(t.Context(), 600*time.Millisecond)
	defer cancel()
	if err := o.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil when measurements are unavailable", err)
	}

	got := o.Unavailable()
	for _, m := range []string{"disk", "jetstream", "pprof"} {
		u, ok := unavailableFor(got, m)
		if !ok {
			t.Errorf("Unavailable() = %q, missing an entry for %q", got, m)
			continue
		}
		if strings.TrimPrefix(u, m+": ") == "" {
			t.Errorf("entry %q has no reason", u)
		}
	}
	if !slices.IsSorted(got) || len(slices.Compact(slices.Clone(got))) != len(got) {
		t.Errorf("Unavailable() = %q, want sorted and deduplicated", got)
	}
	if n := pprofHits.Load(); n > 2 {
		t.Errorf("pprof was requested %d times after a 404; want it recorded once and left alone", n)
	}
	// The scrape still worked, and the generator was observed.
	names := seriesNames(rec)
	for _, want := range []string{"server.rss_bytes", "gen.goroutines"} {
		if !slices.Contains(names, want) {
			t.Errorf("series %q missing from %q", want, names)
		}
	}
}

func TestObserver_Disk(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, n int) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("sqi.db", 1000)
	write("sqi.db-wal", 250)
	write("nats/jetstream/a", 300)
	write("nats/jetstream/sub/b", 200)

	srv := httptest.NewServer(serve(capture(t)))
	defer srv.Close()

	rec := measure.NewRecorder()
	o := observe.New(fakeTarget{httpURL: srv.URL, natsURL: "nats://" + deadPort(t), dataDir: dir}, rec,
		observe.Config{Interval: time.Hour}, quiet())
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // Run takes its first and its final sample, and returns.
	if err := o.Run(ctx); err != nil {
		t.Fatal(err)
	}

	want := map[string]float64{"disk.sqlite_db_bytes": 1000, "disk.sqlite_wal_bytes": 250, "disk.nats_bytes": 500}
	for _, s := range rec.Series() {
		if w, ok := want[s.Name]; ok {
			if got := s.Points[len(s.Points)-1].V; got != w {
				t.Errorf("%s = %v, want %v", s.Name, got, w)
			}
			delete(want, s.Name)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing series %v; have %v", want, seriesNames(rec))
	}
	if u, ok := unavailableFor(o.Unavailable(), "disk"); ok {
		t.Errorf("disk reported unavailable with a data dir: %s", u)
	}
}

func TestObserver_AgainstServer(t *testing.T) {
	srv := testserver.Start(t, testserver.Options{Pprof: true})
	rec := measure.NewRecorder()
	profiles := t.TempDir()
	o := observe.New(fakeTarget{httpURL: srv.HTTPURL(), natsURL: srv.NATSURL()}, rec,
		observe.Config{Interval: 100 * time.Millisecond, ProfileDir: profiles}, quiet())

	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()

	// Something for the HTTP latency histogram to see.
	var traffic sync.WaitGroup
	traffic.Go(func() {
		for ctx.Err() == nil {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.HTTPURL()+"/api/v1/workers", http.NoBody)
			if err != nil {
				return
			}
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_ = resp.Body.Close()
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	err := o.Run(ctx)
	traffic.Wait()
	if err != nil {
		t.Fatal(err)
	}

	names := seriesNames(rec)
	for _, want := range []string{
		"server.rss_bytes", "server.goroutines", "server.heap_bytes", "server.cpu_cores",
		"workers.online",
		"js.bytes{SQI_TASK}", "js.msgs{SQI_TASK}", "js.bytes{SQI_LOGS}", "js.bytes{SQI_WORKER}", "js.bytes{SQI_CANCEL}",
		"gen.cpu_cores", "gen.goroutines", "gen.sched_lag_p99_s",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("series %q missing; have %q", want, names)
		}
	}
	if !hasPrefixed(names, "js.pending{SQI_") {
		t.Errorf("no js.pending{stream/consumer} series; have %q", names)
	}
	if !hasPrefixed(names, "http.p95_s{/") {
		t.Errorf("no http.p95_s series; have %q", names)
	}
	if u, ok := unavailableFor(o.Unavailable(), "jetstream"); ok {
		t.Errorf("jetstream unavailable on an auth-off server: %s", u)
	}
	if u, ok := unavailableFor(o.Unavailable(), "pprof"); ok {
		t.Errorf("pprof unavailable on a server with it enabled: %s", u)
	}

	// Start and end each leave a heap and a goroutine profile.
	for _, kind := range []string{"heap", "goroutine"} {
		for _, phase := range []string{"start", "end"} {
			m, err := filepath.Glob(filepath.Join(profiles, kind+"-*-"+phase+".pb.gz"))
			if err != nil || len(m) != 1 {
				t.Errorf("%s %s profiles = %v, want exactly one", kind, phase, m)
			}
		}
	}

	if err := o.CPUProfile(t.Context(), time.Second); err != nil {
		t.Fatalf("CPUProfile: %v", err)
	}
	if m, err := filepath.Glob(filepath.Join(profiles, "cpu-*.pb.gz")); err != nil || len(m) != 1 {
		t.Errorf("cpu profiles = %v, want one", m)
	}
}

func TestObserver_BrokerAuthMakesJetStreamUnavailable(t *testing.T) {
	srv := testserver.Start(t, testserver.Options{BrokerAuth: true})
	rec := measure.NewRecorder()
	o := observe.New(fakeTarget{httpURL: srv.HTTPURL(), natsURL: srv.NATSURL()}, rec,
		observe.Config{Interval: 100 * time.Millisecond}, quiet())

	ctx, cancel := context.WithTimeout(t.Context(), 800*time.Millisecond)
	defer cancel()
	if err := o.Run(ctx); err != nil {
		t.Fatal(err)
	}
	u, ok := unavailableFor(o.Unavailable(), "jetstream")
	if !ok {
		t.Fatalf("Unavailable() = %q, want a jetstream entry on a broker with auth on", o.Unavailable())
	}
	if !strings.Contains(strings.ToLower(u), "auth") {
		t.Errorf("reason %q does not say the broker refused the connection", u)
	}
	if hasPrefixed(seriesNames(rec), "js.") {
		t.Errorf("recorded JetStream series without access: %q", seriesNames(rec))
	}
	// The rest of the observation went on.
	if !slices.Contains(seriesNames(rec), "server.rss_bytes") {
		t.Errorf("scrape series missing: %q", seriesNames(rec))
	}
}

func TestObserver_CPUProfileUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	o := observe.New(fakeTarget{httpURL: srv.URL}, measure.NewRecorder(),
		observe.Config{ProfileDir: t.TempDir()}, quiet())
	if err := o.CPUProfile(t.Context(), time.Second); err == nil {
		t.Fatal("CPUProfile succeeded against a server without pprof")
	}
	if _, ok := unavailableFor(o.Unavailable(), "pprof"); !ok {
		t.Fatalf("Unavailable() = %q, want a pprof entry", o.Unavailable())
	}
}

func TestObserver_SelfHealthAccessor(t *testing.T) {
	o := observe.New(fakeTarget{}, measure.NewRecorder(), observe.Config{}, quiet())
	if o.SelfHealth() == nil {
		t.Fatal("SelfHealth() is nil")
	}
}

func TestObserver_PostgresUnreachableIsUnavailableAndRedacted(t *testing.T) {
	srv := httptest.NewServer(serve(capture(t)))
	defer srv.Close()

	dsn := "postgres://sqi:hunter2secret@" + deadPort(t) + "/sqi?sslmode=disable&connect_timeout=2"
	rec := measure.NewRecorder()
	o := observe.New(fakeTarget{httpURL: srv.URL, natsURL: "nats://" + deadPort(t)}, rec,
		observe.Config{Interval: time.Hour, PostgresDSN: dsn}, quiet())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := o.Run(ctx); err != nil {
		t.Fatal(err)
	}
	u, ok := unavailableFor(o.Unavailable(), "pg.db_bytes")
	if !ok {
		t.Fatalf("Unavailable() = %q, want a pg.db_bytes entry", o.Unavailable())
	}
	if strings.Contains(u, "hunter2secret") {
		t.Errorf("reason leaks the DSN password: %s", u)
	}
	if slices.Contains(seriesNames(rec), "pg.db_bytes") {
		t.Error("recorded pg.db_bytes without a database")
	}
}

func TestObserver_EmptyDataDirIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(serve(capture(t)))
	defer srv.Close()
	o := observe.New(fakeTarget{httpURL: srv.URL, natsURL: "nats://" + deadPort(t), dataDir: t.TempDir()},
		measure.NewRecorder(), observe.Config{Interval: time.Hour}, quiet())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := o.Run(ctx); err != nil {
		t.Fatal(err)
	}
	u, ok := unavailableFor(o.Unavailable(), "disk")
	if !ok || !strings.Contains(u, "sqi.db") {
		t.Fatalf("Unavailable() = %q, want a disk entry naming the files it looked for", o.Unavailable())
	}
}
