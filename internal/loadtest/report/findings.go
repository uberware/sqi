// SPDX-License-Identifier: AGPL-3.0-or-later

package report

import (
	"fmt"
	"math"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/loadtest/measure"
	"github.com/uberware/sqi/internal/loadtest/scenario"
)

// Finding severities. An "error" is something that makes the run's result
// unreliable or points at a defect in sqi: dropped pushes, a throttled
// generator, workers lost without cause, or a leak large enough to end in an
// out-of-memory kill. A "warn" is something to look at that does not on its
// own invalidate the run.
const (
	SeverityWarn  = "warn"
	SeverityError = "error"
)

// Finding rule names.
const (
	RuleGoroutineGrowth = "goroutine-growth"
	RuleRSSGrowth       = "rss-growth"
	RuleWALNeverShrank  = "wal-never-shrank"
	RuleWSDrops         = "ws-drops"
	RuleRateLimited     = "rate-limited"
	RuleStreamAtCap     = "stream-at-cap"
	RuleWorkersOffline  = "workers-offline-under-load"
)

const (
	// minTrendPoints is the fewest post-warmup samples a growth rule needs
	// before it trusts a fitted slope.
	minTrendPoints = 10
	// goroutineR2 is the r² a goroutine trend must exceed to be flagged.
	goroutineR2 = 0.8
	// rssR2 is the r² an RSS trend must reach to be projected. RSS is noisier
	// than a goroutine count, so the bar is lower, but a flat series with a
	// few spikes must not be projected a week out.
	rssR2 = 0.5
	// rssProjectionHours is the 7 days the RSS slope is projected over.
	rssProjectionHours = 7 * 24
	// rssBudgetBytes is the projected 7-day RSS growth that is flagged.
	rssBudgetBytes = 1 << 30
	// walWindow is the span in which a WAL must be seen to shrink.
	walWindow = 5 * time.Minute
	// capFraction is the share of max_store_mb at which a stream is "at cap".
	capFraction = 0.95
)

// streamLogs is the JetStream stream of task logs, the one that fills first.
const streamLogs = bus.StreamLogs

// defaultMaxStoreMB is the server's own nats.embedded.max_store_mb default,
// used when the scenario does not override it.
var defaultMaxStoreMB = config.DefaultConfig().NATS.Embedded.MaxStoreMB

// Findings applies the rules to a record. steadyFrom is the end of warmup:
// the growth and worker-count rules look only at samples at or after it.
// Findings come back in a fixed rule order.
//
// No rule reads sched.queue_depth{queue}: the server keeps a drained queue's
// last non-zero depth, so a queue that looks stuck may simply be empty.
func Findings(r *Record, steadyFrom time.Time) []Finding {
	if r == nil {
		return nil
	}
	var out []Finding
	for _, rule := range []func(*Record, time.Time) *Finding{
		goroutineGrowth, rssGrowth, walNeverShrank, wsDrops, rateLimited, streamAtCap, workersOffline,
	} {
		if f := rule(r, steadyFrom); f != nil {
			out = append(out, *f)
		}
	}
	return out
}

func findSeries(r *Record, name string) *measure.Series {
	for i := range r.Series {
		if r.Series[i].Name == name {
			return &r.Series[i]
		}
	}
	return nil
}

// pointsFrom counts the points at or after from.
func pointsFrom(s *measure.Series, from time.Time) int {
	n := 0
	for _, p := range s.Points {
		if !p.T.Before(from) {
			n++
		}
	}
	return n
}

func goroutineGrowth(r *Record, from time.Time) *Finding {
	s := findSeries(r, "server.goroutines")
	if s == nil || pointsFrom(s, from) < minTrendPoints {
		return nil
	}
	slope, r2 := measure.Slope(s.Points, from)
	if slope <= 0 || r2 <= goroutineR2 {
		return nil
	}
	return &Finding{
		Rule: RuleGoroutineGrowth, Severity: SeverityWarn,
		Message: fmt.Sprintf("server goroutines grew steadily after warmup: +%.1f/h (r²=%.2f); a leak shows as growth that does not level off", slope, r2),
	}
}

func rssGrowth(r *Record, from time.Time) *Finding {
	s := findSeries(r, "server.rss_bytes")
	if s == nil || pointsFrom(s, from) < minTrendPoints {
		return nil
	}
	slope, r2 := measure.Slope(s.Points, from)
	projected := slope * rssProjectionHours
	if r2 < rssR2 || projected <= rssBudgetBytes {
		return nil
	}
	return &Finding{
		Rule: RuleRSSGrowth, Severity: SeverityWarn,
		Message: fmt.Sprintf("server RSS grew %s/h after warmup (r²=%.2f), projecting %s over 7 days (budget %s)",
			fmtBytes(slope), r2, fmtBytes(projected), fmtBytes(rssBudgetBytes)),
	}
}

func walNeverShrank(r *Record, _ time.Time) *Finding {
	s := findSeries(r, "disk.sqlite_wal_bytes")
	if s == nil || len(s.Points) < 2 {
		return nil
	}
	first, last := s.Points[0], s.Points[len(s.Points)-1]
	if last.T.Sub(first.T) < walWindow {
		return nil
	}
	peak := first.V
	for i := 1; i < len(s.Points); i++ {
		if s.Points[i].V < s.Points[i-1].V {
			return nil
		}
		peak = math.Max(peak, s.Points[i].V)
	}
	if peak <= first.V {
		return nil // it never grew, so there was nothing to checkpoint
	}
	return &Finding{
		Rule: RuleWALNeverShrank, Severity: SeverityWarn,
		Message: fmt.Sprintf("the SQLite WAL grew from %s to %s over %s without shrinking once; checkpoints are not reclaiming it",
			fmtBytes(first.V), fmtBytes(peak), fmtDur(last.T.Sub(first.T).Round(time.Second))),
	}
}

func wsDrops(r *Record, _ time.Time) *Finding {
	n := r.Counters[CounterWSDrops]
	if n <= 0 {
		return nil
	}
	return &Finding{
		Rule: RuleWSDrops, Severity: SeverityError,
		Message: fmt.Sprintf("WebSocket clients missed %d pushes (sequence gaps)", n),
	}
}

func rateLimited(r *Record, _ time.Time) *Finding {
	n := r.Counters[CounterSubmitStatusPrefix+"429"]
	if n <= 0 {
		return nil
	}
	return &Finding{
		Rule: RuleRateLimited, Severity: SeverityError,
		Message: fmt.Sprintf("%d submissions were answered 429: the server's rate limiter throttled the run; set http.rate_limit.enabled: false or use more source addresses", n),
	}
}

// streamCapMB is the JetStream store cap the run's server was configured
// with: nats.embedded.max_store_mb from the scenario's server config, else the
// server default.
func streamCapMB(sc *scenario.Scenario) float64 {
	def := float64(defaultMaxStoreMB)
	if sc == nil {
		return def
	}
	switch v := nested(sc.Server.Config, "nats", "embedded")["max_store_mb"].(type) {
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case uint64:
		return float64(v)
	case float64:
		return v
	}
	return def
}

func streamAtCap(r *Record, _ time.Time) *Finding {
	s := findSeries(r, "js.bytes{"+streamLogs+"}")
	if s == nil || len(s.Points) == 0 {
		return nil
	}
	capBytes := streamCapMB(r.Scenario) * (1 << 20)
	peak := 0.0
	for _, p := range s.Points {
		peak = math.Max(peak, p.V)
	}
	if capBytes <= 0 || peak < capFraction*capBytes {
		return nil
	}
	return &Finding{
		Rule: RuleStreamAtCap, Severity: SeverityWarn,
		Message: fmt.Sprintf("stream %s reached %s of its %s cap (nats.embedded.max_store_mb); the broker evicts the oldest log chunks from here on",
			streamLogs, fmtBytes(peak), fmtBytes(capBytes)),
	}
}

// nested returns the map at path under m, or nil when any step is missing or
// not a map. Indexing a nil map is safe, so callers need no check.
func nested(m map[string]any, path ...string) map[string]any {
	for _, k := range path {
		next, ok := m[k].(map[string]any)
		if !ok {
			return nil
		}
		m = next
	}
	return m
}

// hasChurn reports whether any worker group crashes or disconnects.
func hasChurn(sc *scenario.Scenario) bool {
	for _, g := range sc.Workers {
		if g.Churn.CrashPerHour != 0 || g.Churn.DisconnectPerHour != 0 {
			return true
		}
	}
	return false
}

func workersOffline(r *Record, from time.Time) *Finding {
	live := r.Counters[CounterWorkersLive]
	s := findSeries(r, "workers.online")
	if r.Scenario == nil || hasChurn(r.Scenario) || live <= 0 || s == nil {
		return nil
	}
	lowest, below := math.Inf(1), 0
	for _, p := range s.Points {
		if p.T.Before(from) {
			continue
		}
		lowest = math.Min(lowest, p.V)
		if p.V < float64(live) {
			below++
		}
	}
	if below == 0 {
		return nil
	}
	return &Finding{
		Rule: RuleWorkersOffline, Severity: SeverityError,
		Message: fmt.Sprintf("workers.online fell below the %d live workers in %d samples after warmup (lowest %.0f) although no churn ran",
			live, below, lowest),
	}
}
