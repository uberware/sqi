// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"testing"
	"time"
)

func TestApplyDefaults(t *testing.T) {
	s := &Scenario{
		Workers: []WorkerGroup{{Name: "w", Count: 1}, {Name: "x", Count: 1, Heartbeat: Duration{5 * time.Second}}},
		Jobs: []JobClass{
			{Name: "a", Total: 1},
			{Name: "b", Total: 1, Queue: "other", Weight: 3, Steps: 4, Chunk: 5, Task: TaskSpec{
				LogLineBytes: 7,
				Duration:     Dist{Kind: DistUniform, Min: 1, Max: 2, Unit: "s"},
				LogLines:     Dist{Kind: DistConstant, Value: 9},
			}},
		},
	}
	s.ApplyDefaults()

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"drain_timeout", s.DrainTimeout.Duration, 10 * time.Minute},
		{"server.store", s.Server.Store, "sqlite"},
		{"empty queues gain default", len(s.Farm.Queues), 1},
		{"default queue name", s.Farm.Queues[0].Name, "default"},
		{"heartbeat", s.Workers[0].Heartbeat.Duration, 15 * time.Second},
		{"heartbeat kept", s.Workers[1].Heartbeat.Duration, 5 * time.Second},
		{"steps", s.Jobs[0].Steps, 1},
		{"chunk", s.Jobs[0].Chunk, 1},
		{"weight", s.Jobs[0].Weight, 1},
		{"log_line_bytes", s.Jobs[0].Task.LogLineBytes, 120},
		{"task duration", s.Jobs[0].Task.Duration, Dist{Kind: DistConstant, Value: 5, Unit: "s"}},
		{"task log_lines", s.Jobs[0].Task.LogLines, Dist{Kind: DistConstant}},
		{"task duration kept", s.Jobs[1].Task.Duration, Dist{Kind: DistUniform, Min: 1, Max: 2, Unit: "s"}},
		{"task log_lines kept", s.Jobs[1].Task.LogLines, Dist{Kind: DistConstant, Value: 9}},
		{"empty job queue", s.Jobs[0].Queue, "default"},
		{"queue kept", s.Jobs[1].Queue, "other"},
		{"weight kept", s.Jobs[1].Weight, 3},
		{"steps kept", s.Jobs[1].Steps, 4},
		{"chunk kept", s.Jobs[1].Chunk, 5},
		{"log_line_bytes kept", s.Jobs[1].Task.LogLineBytes, 7},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %v, want %v", c.got, c.want)
			}
		})
	}
}

func TestApplyDefaults_KeepsDeclaredQueues(t *testing.T) {
	s := &Scenario{
		Farm: FarmSpec{Queues: []QueueSpec{{Name: "render"}, {Name: "comp"}}},
		Jobs: []JobClass{{Name: "a", Total: 1}},
	}
	s.ApplyDefaults()
	if len(s.Farm.Queues) != 2 {
		t.Fatalf("queues = %v, want the two declared ones", s.Farm.Queues)
	}
	if s.Jobs[0].Queue != "render" {
		t.Errorf("job queue = %q, want the first declared queue", s.Jobs[0].Queue)
	}

	s.ApplyDefaults()
	if len(s.Farm.Queues) != 2 || s.Jobs[0].Queue != "render" {
		t.Error("ApplyDefaults is not idempotent")
	}
}
