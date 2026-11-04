// SPDX-License-Identifier: AGPL-3.0-or-later

package report

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/uberware/sqi/internal/loadtest/measure"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

// maxEvidence is the most evidence entries a merged invariant carries.
const maxEvidence = verify.MaxEvidence

// shardEntry is a record with its parsed shard number.
type shardEntry struct {
	rec      *Record
	idx, num int
}

// parseShards reads the "i/n" labels of rs, rejects duplicates and mixed
// totals, and returns the records ordered by shard number. A single record
// with no label is shard 0 of 1.
func parseShards(rs []*Record) ([]shardEntry, error) {
	if len(rs) == 0 {
		return nil, errors.New("report: merge: no records")
	}
	out := make([]shardEntry, 0, len(rs))
	seen := map[int]bool{}
	total := 0
	for _, r := range rs {
		if r == nil {
			return nil, errors.New("report: merge: nil record")
		}
		if r.RunID != rs[0].RunID {
			return nil, fmt.Errorf("report: merge: run id %q does not match %q; shards of one run share a --run-id", r.RunID, rs[0].RunID)
		}
		i, n := 0, 1
		if r.Shard != "" || len(rs) > 1 {
			if _, err := fmt.Sscanf(r.Shard, "%d/%d", &i, &n); err != nil || i < 0 || n < 1 || i >= n {
				return nil, fmt.Errorf("report: merge: shard %q is not a label like 0/2", r.Shard)
			}
		}
		if total != 0 && n != total {
			return nil, fmt.Errorf("report: merge: shard %q disagrees with the others about the shard count (%d)", r.Shard, total)
		}
		total = n
		if seen[i] {
			return nil, fmt.Errorf("report: merge: shard %d/%d given twice", i, n)
		}
		seen[i] = true
		out = append(out, shardEntry{rec: r, idx: i, num: n})
	}
	slices.SortFunc(out, func(a, b shardEntry) int { return a.idx - b.idx })
	if out[0].idx != 0 {
		return nil, fmt.Errorf("report: merge: no record for shard 0/%d, which holds the server's series", total)
	}
	return out, nil
}

// Merge combines the records of one run's shards into one record. The records
// must share a run ID. Histograms merge and counters add; series, environment
// and scenario come from shard 0/n, which is the one to carry server
// observations (so the merged record's scenario has shard 0's counts and rates,
// which are per shard, not for the whole farm); the run is Valid and Complete
// only if every shard is; an invariant holds only if it held in every shard,
// with the shards' evidence concatenated; findings are recomputed from the
// merged data. SteadyCounters and WindowCounters add like Counters, unless a
// shard has none, in which case the merged record has none (no
// SteadyCounters makes its rates run averages); WindowEnd is the latest
// shard's. The inputs are not modified.
func Merge(rs []*Record) (*Record, error) {
	shards, err := parseShards(rs)
	if err != nil {
		return nil, err
	}
	total := shards[0].num
	m := *shards[0].rec
	m.Shard = fmt.Sprintf("merged/%d", total)
	m.Histograms = map[string]*measure.Histogram{}
	m.Counters = map[string]int64{}
	m.SteadyCounters = map[string]int64{}
	m.WindowCounters = map[string]int64{}
	steadyAll, windowAll := true, true
	m.Series = slices.Clone(shards[0].rec.Series)
	m.Complete, m.Valid, m.InvalidReason = true, true, ""
	unavailable := map[string]bool{}
	var reasons []string
	for _, s := range shards {
		r := s.rec
		for k, h := range r.Histograms {
			if m.Histograms[k] == nil {
				m.Histograms[k] = measure.NewHistogram()
			}
			m.Histograms[k].Merge(h)
		}
		for k, v := range r.Counters {
			m.Counters[k] += v
		}
		if r.SteadyCounters == nil {
			steadyAll = false
		}
		for k, v := range r.SteadyCounters {
			m.SteadyCounters[k] += v
		}
		if r.WindowCounters == nil {
			windowAll = false
		}
		for k, v := range r.WindowCounters {
			m.WindowCounters[k] += v
		}
		if r.WindowEnd.After(m.WindowEnd) {
			m.WindowEnd = r.WindowEnd
		}
		if r.Started.Before(m.Started) {
			m.Started = r.Started
		}
		if r.Ended.After(m.Ended) {
			m.Ended = r.Ended
		}
		m.Complete = m.Complete && r.Complete
		if !r.Valid {
			m.Valid = false
			reasons = append(reasons, fmt.Sprintf("shard %d/%d: %s", s.idx, s.num, orDash(r.InvalidReason)))
		}
		for _, u := range r.Unavailable {
			unavailable[u] = true
		}
	}
	if !steadyAll {
		m.SteadyCounters = nil
	}
	if !windowAll {
		m.WindowCounters = nil
	}
	m.Backlog = mergeBacklog(shards)
	m.InvalidReason = strings.Join(reasons, "; ")
	if len(shards) < total {
		unavailable[fmt.Sprintf("merge: only %d of %d shards were given, so totals are understated", len(shards), total)] = true
		m.Complete = false
	}
	m.Unavailable = slices.Sorted(maps.Keys(unavailable))
	m.Invariants = mergeInvariants(shards)
	m.Findings = Findings(&m, m.SteadyFrom())
	return &m, nil
}

// mergeBacklog adds up the shards' backlogs, or returns nil when a shard has
// none. Each shard counts the tasks of its own farm.
func mergeBacklog(shards []shardEntry) *Backlog {
	var b Backlog
	for i, s := range shards {
		sb := s.rec.Backlog
		if sb == nil {
			return nil
		}
		b.Open += sb.Open
		b.TailCompleted += sb.TailCompleted
		if i == 0 || sb.TailFrom.Before(b.TailFrom) {
			b.TailFrom = sb.TailFrom
		}
	}
	return &b
}

// mergeInvariants combines the shards' results by invariant name, in order of
// first appearance. With more than one shard each evidence line is prefixed
// with its shard.
func mergeInvariants(shards []shardEntry) []verify.Result {
	var out []verify.Result
	at := map[string]int{}
	extra := map[string]int{}
	for _, s := range shards {
		for _, inv := range s.rec.Invariants {
			i, ok := at[inv.Name]
			if !ok {
				i = len(out)
				at[inv.Name] = i
				out = append(out, verify.Result{Name: inv.Name, OK: true})
			}
			out[i].OK = out[i].OK && inv.OK
			for _, e := range inv.Evidence {
				if len(shards) > 1 {
					e = fmt.Sprintf("shard %d/%d: %s", s.idx, s.num, e)
				}
				if len(out[i].Evidence) < maxEvidence {
					out[i].Evidence = append(out[i].Evidence, e)
				} else {
					extra[inv.Name]++
				}
			}
		}
	}
	for name, n := range extra {
		ev := out[at[name]].Evidence
		// Keep the last slot for the count of what was left out.
		left := n + 1
		ev[len(ev)-1] = fmt.Sprintf("... and %d more", left)
	}
	return out
}
