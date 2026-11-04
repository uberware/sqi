// SPDX-License-Identifier: AGPL-3.0-or-later

package observe

import (
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/measure"
	"github.com/uberware/sqi/internal/loadtest/target"
)

// noDataDir is a target that exposes no data directory; the sampler asks it
// nothing else.
type noDataDir struct{ target.Target }

func (noDataDir) DataDir() string { return "" }

// TestSampler_ReadsGeneratorBeforeSlowCalls checks that the generator's clock
// is read before the sample's network calls, so a slow scrape cannot stretch
// the interval a CPU rate is divided by.
func TestSampler_ReadsGeneratorBeforeSlowCalls(t *testing.T) {
	body := loadCapture(t)
	var scrapes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		scrapes.Add(1)
		time.Sleep(60 * time.Millisecond)
		if _, err := w.Write([]byte(body)); err != nil {
			return
		}
	}))
	defer srv.Close()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	rec := measure.NewRecorder()
	o := New(noDataDir{}, rec, Config{}, nil)
	gen := newGenSampler(t.Context(), rec, o.health, lagPeriod)
	defer gen.stop()
	var scrapesAtRead []int32
	gen.now = func() time.Time {
		scrapesAtRead = append(scrapesAtRead, scrapes.Load())
		return time.Now()
	}
	s := &sampler{
		o:      o,
		scrape: scraper{client: srv.Client(), url: srv.URL, timeout: 5 * time.Second},
		state:  newScrapeState(nil),
		js:     &jsReader{url: "nats://" + dead},
		gen:    gen,
	}
	s.sample(t.Context(), "periodic")
	s.sample(t.Context(), "periodic")

	if want := []int32{0, 1}; !slices.Equal(scrapesAtRead, want) {
		t.Fatalf("scrapes completed when the generator was read = %v, want %v (read first in each sample)", scrapesAtRead, want)
	}
}
