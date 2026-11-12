// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// faultBase is a minimal valid managed scenario: one group with two real
// workers, one job class, three minutes long.
const faultBase = `
name: chaos
duration: 3m
workers:
  - name: render
    count: 4
    real: 2
jobs:
  - name: j
    rate: 1/s
    tasks_per_step: 1
`

// parseWith parses faultBase with extra top-level YAML appended.
func parseWith(t *testing.T, extra string) *Scenario {
	t.Helper()
	s, err := Parse([]byte(faultBase + extra))
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, extra)
	}
	return s
}

func TestFault_Kind(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"kill", `{kill: {server: true}}`, "kill"},
		{"term", `{term: {group: render}}`, "term"},
		{"stall", `{stall: {group: render}}`, "stall"},
		{"partition", `{partition: {}}`, "partition"},
		{"blackhole", `{blackhole: {}}`, "blackhole"},
		{"latency", `{latency: {delay: 1s}}`, "latency"},
		{"flap", `{flap: {period: 2s}}`, "flap"},
		{"power cut", `{power_cut: {order: server-first}}`, "power_cut"},
		{"two kinds", `{kill: {server: true}, partition: {}}`, ""},
		{"none", `{at: 5s}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f Fault
			if err := yaml.Unmarshal([]byte(tt.yaml), &f); err != nil {
				t.Fatal(err)
			}
			if got := f.Kind(); got != tt.want {
				t.Errorf("Kind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFault_End(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want time.Duration
	}{
		{"server kill and restart", `{at: 10s, kill: {server: true}, restart_after: 20s}`, 30 * time.Second},
		{"power cut", `{at: 5s, power_cut: {order: server-first, down: 20s, gap: 10s}}`, 35 * time.Second},
		{"partition", `{at: 1m, partition: {}, duration: 40s}`, 100 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f Fault
			if err := yaml.Unmarshal([]byte(tt.yaml), &f); err != nil {
				t.Fatal(err)
			}
			if got := f.End(); got != tt.want {
				t.Errorf("End() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidate_Faults(t *testing.T) {
	tests := []struct {
		name   string
		target TargetKind
		extra  string
		mutate func(*Scenario)
		want   string // substring of the error; empty means valid
	}{
		{name: "no faults", extra: ``},
		{
			name: "attach rejects faults", target: TargetAttach, extra: "faults:\n  - {at: 5s, partition: {}, duration: 5s}\n",
			want: "faults: fault injection needs a managed target",
		},
		{name: "no kind", extra: "faults:\n  - {at: 5s}\n", want: "faults[0]: set exactly one of"},
		{name: "two kinds", extra: "faults:\n  - {at: 5s, duration: 5s, partition: {}, blackhole: {}}\n", want: "faults[0]: set exactly one of"},
		{name: "negative at", extra: "faults:\n  - {at: -5s, partition: {}, duration: 5s}\n", want: "faults[0].at"},
		{name: "heals at exactly duration", extra: "faults:\n  - {at: 2m20s, partition: {}, duration: 40s}\n"},
		{name: "heals one second late", extra: "faults:\n  - {at: 2m21s, partition: {}, duration: 40s}\n", want: "faults[0].duration"},
		{
			name: "second fault named by index", extra: "faults:\n  - {at: 5s, partition: {}, duration: 5s}\n  - {at: 5s, kill: {group: nope}}\n",
			want: "faults[1].kill.group",
		},

		{name: "kill a group", extra: "faults:\n  - {at: 5s, kill: {group: render}}\n"},
		{name: "kill some of a group", extra: "faults:\n  - {at: 5s, kill: {group: render, real: 2}, restart_after: 10s}\n"},
		{
			name: "kill both server and group", extra: "faults:\n  - {at: 5s, kill: {server: true, group: render}, restart_after: 5s}\n",
			want: "faults[0].kill: exactly one of server and group",
		},
		{name: "kill neither", extra: "faults:\n  - {at: 5s, kill: {}}\n", want: "faults[0].kill: exactly one of server and group"},
		{name: "kill unknown group", extra: "faults:\n  - {at: 5s, kill: {group: nope}}\n", want: "faults[0].kill.group"},
		{name: "kill more real than the group has", extra: "faults:\n  - {at: 5s, kill: {group: render, real: 3}}\n", want: "faults[0].kill.real"},
		{name: "kill negative real", extra: "faults:\n  - {at: 5s, kill: {group: render, real: -1}}\n", want: "faults[0].kill.real"},
		{name: "term a group", extra: "faults:\n  - {at: 5s, term: {group: render}, restart_after: 5s}\n"},
		{name: "term unknown group", extra: "faults:\n  - {at: 5s, term: {group: nope}}\n", want: "faults[0].term.group"},
		{name: "kill server without restart", extra: "faults:\n  - {at: 5s, kill: {server: true}}\n", want: "faults[0].restart_after"},
		{name: "term server without restart", extra: "faults:\n  - {at: 5s, term: {server: true}}\n", want: "faults[0].restart_after"},
		{name: "kill server with restart", extra: "faults:\n  - {at: 5s, kill: {server: true}, restart_after: 5s}\n"},
		{name: "negative restart", extra: "faults:\n  - {at: 5s, kill: {group: render}, restart_after: -5s}\n", want: "faults[0].restart_after"},
		{name: "restart on a partition", extra: "faults:\n  - {at: 5s, partition: {}, duration: 5s, restart_after: 5s}\n", want: "faults[0].restart_after"},
		{name: "duration on a kill", extra: "faults:\n  - {at: 5s, kill: {group: render}, duration: 5s}\n", want: "faults[0].duration"},

		{name: "stall a group", extra: "faults:\n  - {at: 5s, stall: {group: render}, duration: 10s}\n"},
		{name: "stall the server", extra: "faults:\n  - {at: 5s, stall: {server: true}, duration: 10s}\n"},
		{name: "stall without duration", extra: "faults:\n  - {at: 5s, stall: {group: render}}\n", want: "faults[0].duration"},
		{name: "stall unknown group", extra: "faults:\n  - {at: 5s, stall: {group: nope}, duration: 5s}\n", want: "faults[0].stall.group"},

		{name: "partition everything", extra: "faults:\n  - {at: 5s, partition: {}, duration: 5s}\n"},
		{name: "partition without duration", extra: "faults:\n  - {at: 5s, partition: {}}\n", want: "faults[0].duration"},
		{
			name: "partition unknown group", extra: "faults:\n  - {at: 5s, partition: {groups: [render, nope]}, duration: 5s}\n",
			want: "faults[0].partition.groups[1]",
		},
		{name: "blackhole a group", extra: "faults:\n  - {at: 5s, blackhole: {groups: [render]}, duration: 5s}\n"},
		{name: "blackhole without duration", extra: "faults:\n  - {at: 5s, blackhole: {}}\n", want: "faults[0].duration"},
		{
			name: "blackhole unknown group", extra: "faults:\n  - {at: 5s, blackhole: {groups: [nope]}, duration: 5s}\n",
			want: "faults[0].blackhole.groups[0]",
		},

		{name: "latency delay", extra: "faults:\n  - {at: 5s, latency: {delay: 200ms, jitter: 50ms}, duration: 5s}\n"},
		{name: "latency bandwidth only", extra: "faults:\n  - {at: 5s, latency: {bytes_per_sec: 1000}, duration: 5s}\n"},
		{name: "latency does nothing", extra: "faults:\n  - {at: 5s, latency: {}, duration: 5s}\n", want: "faults[0].latency"},
		{
			name: "latency jitter above delay", extra: "faults:\n  - {at: 5s, latency: {delay: 100ms, jitter: 200ms}, duration: 5s}\n",
			want: "faults[0].latency.jitter",
		},
		{
			name: "latency negative bandwidth", extra: "faults:\n  - {at: 5s, latency: {delay: 1s, bytes_per_sec: -1}, duration: 5s}\n",
			want: "faults[0].latency.bytes_per_sec",
		},
		{name: "latency without duration", extra: "faults:\n  - {at: 5s, latency: {delay: 1s}}\n", want: "faults[0].duration"},
		{
			name: "latency unknown group", extra: "faults:\n  - {at: 5s, latency: {delay: 1s, groups: [nope]}, duration: 5s}\n",
			want: "faults[0].latency.groups[0]",
		},

		{name: "flap", extra: "faults:\n  - {at: 5s, flap: {period: 2s}, duration: 20s}\n"},
		{name: "flap period equals duration", extra: "faults:\n  - {at: 5s, flap: {period: 20s}, duration: 20s}\n"},
		{name: "flap period zero", extra: "faults:\n  - {at: 5s, flap: {}, duration: 20s}\n", want: "faults[0].flap.period"},
		{name: "flap period beyond duration", extra: "faults:\n  - {at: 5s, flap: {period: 30s}, duration: 20s}\n", want: "faults[0].flap.period"},
		{name: "flap without duration", extra: "faults:\n  - {at: 5s, flap: {period: 2s}}\n", want: "faults[0].duration"},
		{
			name: "flap unknown group", extra: "faults:\n  - {at: 5s, flap: {period: 2s, groups: [nope]}, duration: 20s}\n",
			want: "faults[0].flap.groups[0]",
		},

		{name: "power cut", extra: "faults:\n  - {at: 5s, power_cut: {order: server-first, down: 10s, gap: 5s}}\n"},
		{name: "power cut workers first", extra: "faults:\n  - {at: 5s, power_cut: {order: workers-first, down: 10s}}\n"},
		{name: "power cut bad order", extra: "faults:\n  - {at: 5s, power_cut: {order: both, down: 10s}}\n", want: "faults[0].power_cut.order"},
		{name: "power cut no order", extra: "faults:\n  - {at: 5s, power_cut: {down: 10s}}\n", want: "faults[0].power_cut.order"},
		{name: "power cut no down", extra: "faults:\n  - {at: 5s, power_cut: {order: server-first}}\n", want: "faults[0].power_cut.down"},
		{
			name: "power cut negative gap", extra: "faults:\n  - {at: 5s, power_cut: {order: server-first, down: 5s, gap: -1s}}\n",
			want: "faults[0].power_cut.gap",
		},
		{
			name: "power cut ends after the run", extra: "faults:\n  - {at: 2m, power_cut: {order: server-first, down: 50s, gap: 11s}}\n",
			want: "faults[0]",
		},

		{
			name: "server fault with websocket clients", extra: "faults:\n  - {at: 5s, kill: {server: true}, restart_after: 5s}\nclients: {websocket: {count: 2}}\n",
			want: "clients.websocket.count",
		},
		{
			name: "server fault with pollers", extra: "faults:\n  - {at: 5s, power_cut: {order: server-first, down: 5s}}\nclients: {rest_pollers: {count: 1, interval: 1s}}\n",
			want: "clients.rest_pollers.count",
		},
		{
			name: "worker fault with clients", extra: "faults:\n  - {at: 5s, kill: {group: render}}\nclients: {websocket: {count: 2}}\n",
		},
		{
			name: "network fault with clients", extra: "faults:\n  - {at: 5s, partition: {}, duration: 5s}\nclients: {websocket: {count: 2}}\n",
		},
		{
			name: "group with no real workers", extra: "faults:\n  - {at: 5s, kill: {group: sim}}\n", mutate: func(s *Scenario) {
				s.Workers = append(s.Workers, WorkerGroup{Name: "sim", Count: 3, Heartbeat: Duration{time.Second}})
			},
			want: "faults[0].kill.group",
		},
		{
			name: "attach rejects real_task", target: TargetAttach, extra: ``, mutate: func(s *Scenario) {
				s.Workers[0].Real = 0
				s.Jobs[0].RealTask = &RealTask{Duration: Duration{time.Second}}
			},
			want: "jobs[0].real_task: needs a managed target",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := parseWith(t, tt.extra)
			if tt.mutate != nil {
				tt.mutate(s)
			}
			err := s.Validate(tt.target)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("want valid, got %v", err)
			case tt.want != "" && err == nil:
				t.Errorf("want an error containing %q, got none", tt.want)
			case tt.want != "" && !strings.Contains(err.Error(), tt.want):
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestValidate_Expect(t *testing.T) {
	const good = `{check: terminal, reason: a reason, tracker: H13f1}`
	tests := []struct {
		name   string
		target TargetKind
		extra  string
		want   string
	}{
		{name: "valid entry", extra: "expect:\n  known_violations:\n    - " + good + "\n"},
		{name: "valid with kind and os", extra: "expect:\n  max_rework_attempts: 0\n  known_violations:\n    - {check: rework, kind: may, os: [linux, darwin, windows], max_attempts: 5, reason: r, tracker: H13f1}\n"},
		{name: "rework entry without max_attempts", extra: "expect:\n  max_rework_attempts: 0\n  known_violations:\n    - {check: rework, reason: r, tracker: t}\n", want: "expect.known_violations[0].max_attempts"},
		{name: "rework entry without a rework bound", extra: "expect:\n  known_violations:\n    - {check: rework, max_attempts: 5, reason: r, tracker: t}\n", want: "expect.max_rework_attempts"},
		{name: "max_attempts equal to the bound", extra: "expect:\n  max_rework_attempts: 5\n  known_violations:\n    - {check: rework, max_attempts: 5, reason: r, tracker: t}\n", want: "expect.known_violations[0].max_attempts"},
		{name: "max_attempts below the bound", extra: "expect:\n  max_rework_attempts: 5\n  known_violations:\n    - {check: rework, max_attempts: 3, reason: r, tracker: t}\n", want: "expect.known_violations[0].max_attempts"},
		{name: "max_attempts just above the bound", extra: "expect:\n  max_rework_attempts: 5\n  known_violations:\n    - {check: rework, max_attempts: 6, reason: r, tracker: t}\n"},
		{name: "max_attempts on another check", extra: "expect:\n  max_rework_attempts: 0\n  known_violations:\n    - {check: terminal, max_attempts: 5, reason: r, tracker: t}\n", want: "expect.known_violations[0].max_attempts"},
		{name: "unknown check", extra: "expect:\n  known_violations:\n    - {check: nope, reason: r, tracker: t}\n", want: "expect.known_violations[0].check"},
		{name: "unknown kind", extra: "expect:\n  known_violations:\n    - {check: terminal, kind: sometimes, reason: r, tracker: t}\n", want: "expect.known_violations[0].kind"},
		{name: "no reason", extra: "expect:\n  known_violations:\n    - {check: terminal, tracker: t}\n", want: "expect.known_violations[0].reason"},
		{name: "no tracker", extra: "expect:\n  known_violations:\n    - {check: terminal, reason: r}\n", want: "expect.known_violations[0].tracker"},
		{name: "unknown os", extra: "expect:\n  known_violations:\n    - {check: terminal, reason: r, tracker: t, os: [linux, plan9]}\n", want: "expect.known_violations[0].os[1]"},
		{name: "second entry named by index", extra: "expect:\n  known_violations:\n    - " + good + "\n    - {check: nope, reason: r, tracker: t}\n", want: "expect.known_violations[1].check"},
		{name: "rework budget", extra: "expect: {max_rework_attempts: 3}\n"},
		{name: "rework budget zero", extra: "expect: {max_rework_attempts: 0}\n"},
		{name: "negative rework budget", extra: "expect: {max_rework_attempts: -1}\n", want: "expect.max_rework_attempts"},
		{name: "attach rejects rework budget", target: TargetAttach, extra: "expect: {max_rework_attempts: 3}\n", want: "expect: needs a managed target"},
		{name: "attach rejects known violations", target: TargetAttach, extra: "expect:\n  known_violations:\n    - " + good + "\n", want: "expect: needs a managed target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := parseWith(t, tt.extra).Validate(tt.target)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("want valid, got %v", err)
			case tt.want != "" && err == nil:
				t.Errorf("want an error containing %q, got none", tt.want)
			case tt.want != "" && !strings.Contains(err.Error(), tt.want):
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestValidate_RealTask(t *testing.T) {
	tests := []struct {
		name string
		rt   RealTask
		want string
	}{
		{name: "valid", rt: RealTask{Duration: Duration{5 * time.Second}, LogEvery: Duration{time.Second}, ChattyChild: true}},
		{name: "no duration", rt: RealTask{}, want: "jobs[0].real_task.duration"},
		{name: "negative log_every", rt: RealTask{Duration: Duration{time.Second}, LogEvery: Duration{-time.Second}}, want: "jobs[0].real_task.log_every"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := parseWith(t, ``)
			s.Jobs[0].RealTask = &tt.rt
			err := s.Validate(TargetManaged)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("want valid, got %v", err)
			case tt.want != "" && err == nil:
				t.Errorf("want an error containing %q, got none", tt.want)
			case tt.want != "" && !strings.Contains(err.Error(), tt.want):
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestApplyDefaults_KnownViolationKindIsMust(t *testing.T) {
	s := parseWith(t, "expect:\n  known_violations:\n    - {check: terminal, reason: r, tracker: t}\n    - {check: logs, kind: may, reason: r, tracker: t}\n")
	if got := s.Expect.KnownViolations[0].Kind; got != "must" {
		t.Errorf("empty kind became %q, want must", got)
	}
	if got := s.Expect.KnownViolations[1].Kind; got != "may" {
		t.Errorf("declared kind became %q, want may", got)
	}
	s.ApplyDefaults()
	if got := s.Expect.KnownViolations[0].Kind; got != "must" {
		t.Errorf("second ApplyDefaults changed kind to %q", got)
	}
}

func TestHasNetworkFault(t *testing.T) {
	tests := []struct {
		name  string
		fault string
		want  bool
	}{
		{"partition", `{at: 5s, partition: {}, duration: 5s}`, true},
		{"blackhole", `{at: 5s, blackhole: {}, duration: 5s}`, true},
		{"latency", `{at: 5s, latency: {delay: 1s}, duration: 5s}`, true},
		{"flap", `{at: 5s, flap: {period: 1s}, duration: 5s}`, true},
		{"kill", `{at: 5s, kill: {group: render}}`, false},
		{"power cut", `{at: 5s, power_cut: {order: server-first, down: 5s}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := parseWith(t, "faults:\n  - "+tt.fault+"\n")
			if got := s.HasNetworkFault(); got != tt.want {
				t.Errorf("HasNetworkFault() = %v, want %v", got, tt.want)
			}
		})
	}
	if parseWith(t, ``).HasNetworkFault() {
		t.Error("a scenario with no faults has no network fault")
	}
}

func TestMarshalYAML_FaultsRoundTrip(t *testing.T) {
	src := parseWith(t, `
faults:
  - {at: 5s, kill: {group: render, real: 1}, restart_after: 10s}
  - {at: 30s, latency: {delay: 200ms, jitter: 50ms, bytes_per_sec: 1000}, duration: 20s}
expect:
  max_rework_attempts: 4
  known_violations:
    - {check: terminal, kind: may, os: [windows], reason: r, tracker: H13f1}
`)
	src.Jobs[0].RealTask = &RealTask{Duration: Duration{time.Second}, SilentChild: true}
	out, err := yaml.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse of the marshaled scenario: %v\n%s", err, out)
	}
	again, err := yaml.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(out) {
		t.Errorf("round trip differs:\n got %s\nwant %s", again, out)
	}
	if back.Faults[0].Kill == nil || back.Faults[0].Kill.Real != 1 || *back.Expect.MaxReworkAttempts != 4 || back.Jobs[0].RealTask == nil {
		t.Errorf("round trip lost a value:\n%s", out)
	}
}

func TestClone_CopiesFaults(t *testing.T) {
	s := parseWith(t, "faults:\n  - {at: 5s, kill: {group: render}}\nexpect: {max_rework_attempts: 2}\n")
	c := s.Clone()
	c.Faults[0].Kill.Group = "other"
	*c.Expect.MaxReworkAttempts = 9
	if s.Faults[0].Kill.Group != "render" || *s.Expect.MaxReworkAttempts != 2 {
		t.Error("changing the clone changed the original")
	}
}
