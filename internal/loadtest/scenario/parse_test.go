// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"os"
	"strings"
	"testing"
	"time"
)

func exampleBytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func loadExample(t *testing.T) *Scenario {
	t.Helper()
	s, err := Parse(exampleBytes(t))
	if err != nil {
		t.Fatalf("Parse(example): %v", err)
	}
	return s
}

func TestParse_ExampleScenario(t *testing.T) {
	s := loadExample(t)

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"name", s.Name, "mixed-medium"},
		{"duration", s.Duration.Duration, 30 * time.Minute},
		{"warmup", s.Warmup.Duration, 2 * time.Minute},
		{"drain_timeout default", s.DrainTimeout.Duration, 10 * time.Minute},
		{"server.store", s.Server.Store, "sqlite"},
		{"server.http_auth", s.Server.HTTPAuth, false},
		{"farm.queues", len(s.Farm.Queues), 2},
		{"farm.usage_pools[0].limit", s.Farm.UsagePools[0].Limit, 200},
		{"workers", len(s.Workers), 2},
		{"workers[0].count", s.Workers[0].Count, 1800},
		{"workers[0].ramp", s.Workers[0].Ramp.PerSecond, 200.0},
		{"workers[0].cpu", s.Workers[0].CPU, 32},
		{"workers[0].tags", s.Workers[0].Tags["arnold"], "true"},
		{"workers[0].heartbeat", s.Workers[0].Heartbeat.Duration, 15 * time.Second},
		{"workers[0].churn.crash", s.Workers[0].Churn.CrashPerHour, 0.05},
		{"workers[0].churn.disconnect", s.Workers[0].Churn.DisconnectPerHour, 0.1},
		{"workers[0].churn.restart_after", s.Workers[0].Churn.RestartAfter.Duration, 30 * time.Second},
		{"workers[1].real", s.Workers[1].Real, 2},
		{"workers[1].count", s.Workers[1].Count, 0},
		{"jobs[0].rate", s.Jobs[0].Rate.PerSecond, 2.0},
		{"jobs[0].steps", s.Jobs[0].Steps, 2},
		{"jobs[0].cores", s.Jobs[0].Cores, 8},
		{"jobs[0].task.fail_rate", s.Jobs[0].Task.FailRate, 0.01},
		{"jobs[0].task.hang_rate", s.Jobs[0].Task.HangRate, 0.0005},
		{"jobs[0].task.log_line_bytes", s.Jobs[0].Task.LogLineBytes, 120},
		{"jobs[1].total", s.Jobs[1].Total, 3},
		{"jobs[1].tasks_per_step", s.Jobs[1].TasksPerStep, 250000},
		{"jobs[1].queue default", s.Jobs[1].Queue, "render"},
		{"jobs[1].steps default", s.Jobs[1].Steps, 1},
		{"clients.websocket.count", s.Clients.WebSocket.Count, 200},
		{"clients.websocket.subscribe", len(s.Clients.WebSocket.Subscribe), 4},
		{"clients.rest_pollers.interval", s.Clients.RESTPollers.Interval.Duration, 5 * time.Second},
		{"actions", len(s.Actions), 2},
		{"actions[0].at", s.Actions[0].At.Duration, 10 * time.Minute},
		{"actions[0].cancel_jobs", s.Actions[0].CancelJobs.Fraction, 0.1},
		{"actions[1].retry_failed", s.Actions[1].RetryFailed, true},
		{"assert", s.Assert["p95_submit_to_first_lease"].Duration, 2 * time.Second},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %v, want %v", c.got, c.want)
			}
		})
	}

	wantDur := Dist{Kind: DistLognormal, Median: 20, P95: 90, Unit: "s"}
	if s.Jobs[0].Task.Duration != wantDur {
		t.Errorf("jobs[0].task.duration = %+v, want %+v", s.Jobs[0].Task.Duration, wantDur)
	}
	wantLogs := Dist{Kind: DistLognormal, Median: 200, P95: 2000}
	if s.Jobs[0].Task.LogLines != wantLogs {
		t.Errorf("jobs[0].task.log_lines = %+v, want %+v", s.Jobs[0].Task.LogLines, wantLogs)
	}
}

func TestParse_Errors(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"empty document", "", "empty"},
		{"unknown top-level key", "name: x\nbogus: 1\n", "bogus"},
		{"misspelled nested key", "name: x\nserver:\n  stor: sqlite\n", "stor"},
		{"misspelled job key", "name: x\njobs:\n  - name: a\n    tasks_per_stp: 3\n", "tasks_per_stp"},
		{"duration without unit", "name: x\nduration: 30\n", "duration"},
		{"bad rate", "name: x\njobs:\n  - name: a\n    rate: fast\n", "rate"},
		{"bad syntax", "name: [\n", "scenario"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestParseRate(t *testing.T) {
	tests := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{"2/s", 2, false},
		{"120/m", 2, false},
		{"0.5/s", 0.5, false},
		{"7200/h", 2, false},
		{" 3 / s ", 3, false},
		{"2", 0, true},
		{"x/s", 0, true},
		{"-1/s", 0, true},
		{"0/s", 0, true},
		{"2/d", 0, true},
		{"2/", 0, true},
		{"", 0, true},
		{"NaN/s", 0, true},
		{"Inf/s", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseRate(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseRate(%q) = %v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRate(%q): %v", tc.in, err)
			}
			if got.PerSecond != tc.want {
				t.Errorf("ParseRate(%q) = %v, want %v", tc.in, got.PerSecond, tc.want)
			}
		})
	}
}
