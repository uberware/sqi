// SPDX-License-Identifier: AGPL-3.0-or-later

package report

import (
	"testing"

	"github.com/uberware/sqi/internal/loadtest/scenario"
)

func shardScenario() *scenario.Scenario {
	return &scenario.Scenario{
		Name: "shard",
		Farm: scenario.FarmSpec{
			Queues:     []scenario.QueueSpec{{Name: "render", MaxConcurrentTasks: 7}, {Name: "comp"}},
			UsagePools: []scenario.PoolSpec{{Name: "lic", Limit: 5}},
		},
		Workers: []scenario.WorkerGroup{
			{Name: "sim", Count: 1801, Ramp: scenario.Rate{PerSecond: 100}, Tags: map[string]string{"gpu": "1"}},
			{Name: "real", Real: 3},
		},
		Jobs: []scenario.JobClass{
			{Name: "paced", Rate: scenario.Rate{PerSecond: 3}},
			{Name: "burst", Total: 5},
			{Name: "single", Total: 1},
		},
		Clients: scenario.ClientsSpec{
			WebSocket:   scenario.WSSpec{Count: 11},
			RESTPollers: scenario.PollSpec{Count: 4},
		},
	}
}

func TestShard_Apply(t *testing.T) {
	cases := []struct {
		name        string
		shard       ShardSpec
		prefix      string
		workers     int
		real        int
		ramp        float64
		rate        float64
		burstTotal  int
		singleTotal int // 0: the class is dropped from the shard
		ws, pollers int
		queueCap    int
	}{
		{name: "shard 0 of 2", shard: ShardSpec{0, 2}, prefix: "s0", workers: 901, real: 2, ramp: 50, rate: 1.5, burstTotal: 3, singleTotal: 1, ws: 6, pollers: 2, queueCap: 4},
		{name: "shard 1 of 2", shard: ShardSpec{1, 2}, prefix: "s1", workers: 900, real: 1, ramp: 50, rate: 1.5, burstTotal: 2, singleTotal: 0, ws: 5, pollers: 2, queueCap: 3},
		{name: "shard 2 of 3", shard: ShardSpec{2, 3}, prefix: "s2", workers: 600, real: 1, ramp: 100.0 / 3, rate: 1, burstTotal: 1, singleTotal: 0, ws: 3, pollers: 1, queueCap: 2},
		{name: "not sharded", shard: ShardSpec{}, prefix: "", workers: 1801, real: 3, ramp: 100, rate: 3, burstTotal: 5, singleTotal: 1, ws: 11, pollers: 4, queueCap: 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := shardScenario()
			got := tc.shard.Apply(orig)
			if got == orig {
				t.Fatal("Apply returned its input; it must return a copy")
			}
			if p := tc.shard.Prefix(); p != tc.prefix {
				t.Errorf("Prefix = %q, want %q", p, tc.prefix)
			}
			if g := got.Workers[0]; g.Count != tc.workers || g.Ramp.PerSecond != tc.ramp {
				t.Errorf("workers[0] = count %d ramp %v, want %d, %v", g.Count, g.Ramp.PerSecond, tc.workers, tc.ramp)
			}
			if r := got.Workers[1].Real; r != tc.real {
				t.Errorf("workers[1].real = %d, want %d", r, tc.real)
			}
			if r := got.Jobs[0].Rate.PerSecond; r != tc.rate {
				t.Errorf("jobs[0].rate = %v/s, want %v/s", r, tc.rate)
			}
			if tot := got.Jobs[1].Total; tot != tc.burstTotal {
				t.Errorf("jobs[1].total = %d, want %d", tot, tc.burstTotal)
			}
			single := 0
			for _, j := range got.Jobs {
				if j.Name == "single" {
					single = j.Total
				}
			}
			if single != tc.singleTotal {
				t.Errorf("single class total = %d, want %d (0 means dropped)", single, tc.singleTotal)
			}
			if c := got.Clients; c.WebSocket.Count != tc.ws || c.RESTPollers.Count != tc.pollers {
				t.Errorf("clients = ws %d pollers %d, want %d, %d", c.WebSocket.Count, c.RESTPollers.Count, tc.ws, tc.pollers)
			}
			if q := got.Farm.Queues; q[0].MaxConcurrentTasks != tc.queueCap || q[1].MaxConcurrentTasks != 0 {
				t.Errorf("queue caps = %d, %d, want %d, 0 (unlimited stays unlimited)", q[0].MaxConcurrentTasks, q[1].MaxConcurrentTasks, tc.queueCap)
			}
			if l := got.Farm.UsagePools[0].Limit; l != 5 {
				t.Errorf("usage pool limit = %d, want 5: pools are shared by name across shards", l)
			}
			// The input is untouched.
			if o := orig.Workers[0].Count; o != 1801 {
				t.Errorf("input workers[0].count changed to %d", o)
			}
			got.Workers[0].Tags["gpu"] = "changed"
			if orig.Workers[0].Tags["gpu"] != "1" {
				t.Error("the result shares the input's tags map")
			}
		})
	}
}

func TestParseShard(t *testing.T) {
	cases := []struct {
		in      string
		want    ShardSpec
		wantErr bool
	}{
		{in: "0/2", want: ShardSpec{0, 2}},
		{in: "3/4", want: ShardSpec{3, 4}},
		{in: "0/1", want: ShardSpec{0, 1}},
		{in: "2/2", wantErr: true},
		{in: "-1/2", wantErr: true},
		{in: "1/0", wantErr: true},
		{in: "1", wantErr: true},
		{in: "a/b", wantErr: true},
		{in: "1/2x", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseShard(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseShard(%q) error = %v, want error %v", tc.in, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("ParseShard(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestShard_String(t *testing.T) {
	cases := []struct {
		s    ShardSpec
		want string
	}{
		{ShardSpec{}, ""},
		{ShardSpec{0, 1}, ""},
		{ShardSpec{1, 3}, "1/3"},
	}
	for _, tc := range cases {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("%+v.String() = %q, want %q", tc.s, got, tc.want)
		}
	}
}
