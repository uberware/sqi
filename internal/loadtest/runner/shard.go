// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/uberware/sqi/internal/loadtest/scenario"
)

// ShardSpec names one slice of a scenario run from several generator
// machines: shard Index of Of, counting from 0. The zero value, like any Of
// below 2, is the whole scenario.
type ShardSpec struct{ Index, Of int }

// ParseShard reads "i/n", with 0 <= i < n.
func ParseShard(s string) (ShardSpec, error) {
	is, ns, ok := strings.Cut(s, "/")
	i, ierr := strconv.Atoi(is)
	n, nerr := strconv.Atoi(ns)
	if !ok || ierr != nil || nerr != nil {
		return ShardSpec{}, fmt.Errorf("shard %q: want i/n, like 0/2", s)
	}
	if n < 1 || i < 0 || i >= n {
		return ShardSpec{}, fmt.Errorf("shard %q: want 0 <= i < n", s)
	}
	return ShardSpec{Index: i, Of: n}, nil
}

// active reports whether s is a real slice of a run of several shards.
func (s ShardSpec) active() bool { return s.Of > 1 }

// String is "i/n", the record's Shard label, or "" for an unsharded run.
func (s ShardSpec) String() string {
	if !s.active() {
		return ""
	}
	return fmt.Sprintf("%d/%d", s.Index, s.Of)
}

// Prefix is "s<i>", which a shard puts in front of its worker IDs and job
// names so that shards never collide on one server, or "" when not sharded.
func (s ShardSpec) Prefix() string {
	if !s.active() {
		return ""
	}
	return "s" + strconv.Itoa(s.Index)
}

// share is this shard's part of n: n/Of, with the remainder going to shard 0.
func (s ShardSpec) share(n int) int {
	if !s.active() || n <= 0 {
		return n
	}
	q := n / s.Of
	if s.Index == 0 {
		q += n % s.Of
	}
	return q
}

// Apply returns this shard's slice of sc, leaving sc unchanged. Worker
// counts (simulated and real), ramp rates, job rates and totals, and the
// numbers of WebSocket clients and REST pollers are divided between the
// shards, remainders going to shard 0. Each shard creates its own farm, so a
// queue's concurrency cap is divided too, and a capped queue keeps a cap of at
// least 1 in every shard (0 is unlimited). Usage pools are global and shared
// by name across the shards, so their limits are not divided. A job class
// whose total divides to 0 in this shard, and that has no rate, is left out
// of it.
func (s ShardSpec) Apply(sc *scenario.Scenario) *scenario.Scenario {
	c := sc.Clone()
	if !s.active() {
		return c
	}
	for i := range c.Workers {
		g := &c.Workers[i]
		g.Count = s.share(g.Count)
		g.Real = s.share(g.Real)
		g.Ramp.PerSecond /= float64(s.Of)
	}
	jobs := c.Jobs[:0]
	for _, j := range c.Jobs {
		hadTotal := j.Total > 0
		j.Total = s.share(j.Total)
		j.Rate.PerSecond /= float64(s.Of)
		if hadTotal && j.Total == 0 && j.Rate.PerSecond == 0 {
			continue
		}
		jobs = append(jobs, j)
	}
	c.Jobs = jobs
	for i := range c.Farm.Queues {
		q := &c.Farm.Queues[i]
		if q.MaxConcurrentTasks > 0 {
			q.MaxConcurrentTasks = max(s.share(q.MaxConcurrentTasks), 1)
		}
	}
	c.Clients.WebSocket.Count = s.share(c.Clients.WebSocket.Count)
	c.Clients.RESTPollers.Count = s.share(c.Clients.RESTPollers.Count)
	return c
}
