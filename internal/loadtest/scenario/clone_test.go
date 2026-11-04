// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestMarshalYAML_RoundTrip writes every built-in scenario back out as YAML
// and parses it again: the result must equal the original, so a scenario
// printed by `sqi-load scenarios --show` can be saved, edited and run.
func TestMarshalYAML_RoundTrip(t *testing.T) {
	for _, name := range BuiltinNames() {
		t.Run(name, func(t *testing.T) {
			orig, err := Builtin(name)
			if err != nil {
				t.Fatal(err)
			}
			out, err := yaml.Marshal(orig)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			back, err := Parse(out)
			if err != nil {
				t.Fatalf("Parse of the marshaled scenario: %v\n%s", err, out)
			}
			// YAML writes a nil list or map as an empty one, so compare what
			// the two scenarios marshal to: a value lost on the way would
			// differ.
			again, err := yaml.Marshal(back)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(again) != string(out) {
				t.Errorf("round trip differs:\n got %s\nwant %s", again, out)
			}
			if back.Jobs[0].Task.Duration != orig.Jobs[0].Task.Duration || back.Duration != orig.Duration {
				t.Errorf("round trip changed a value: %+v vs %+v", back.Jobs[0].Task.Duration, orig.Jobs[0].Task.Duration)
			}
		})
	}
}

func TestMarshalYAML_Forms(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"duration", Duration{90_000_000_000}, "1m30s\n"},
		{"rate", Rate{PerSecond: 2}, "2/s\n"},
		{"slow rate", Rate{PerSecond: 0.25}, "0.25/s\n"},
		{"constant duration", Dist{Kind: DistConstant, Value: 20, Unit: "s"}, "20s\n"},
		{"constant count", Dist{Kind: DistConstant, Value: 200}, "200\n"},
		{"uniform", Dist{Kind: DistUniform, Min: 1, Max: 3, Unit: "s"}, "dist: uniform\nmin: 1s\nmax: 3s\n"},
		{"lognormal", Dist{Kind: DistLognormal, Median: 100, P95: 400}, "dist: lognormal\nmedian: 100\np95: 400\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := yaml.Marshal(tc.v)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.want {
				t.Errorf("Marshal = %q, want %q", out, tc.want)
			}
		})
	}
}

func TestClone(t *testing.T) {
	for _, name := range BuiltinNames() {
		t.Run(name, func(t *testing.T) {
			orig, err := Builtin(name)
			if err != nil {
				t.Fatal(err)
			}
			c := orig.Clone()
			if !reflect.DeepEqual(orig, c) {
				t.Fatalf("Clone differs from its source")
			}
		})
	}

	orig := &Scenario{
		Server:  ServerSpec{Config: map[string]any{"scheduler": map[string]any{"x": 1}, "list": []any{map[string]any{"a": 1}}}},
		Farm:    FarmSpec{Queues: []QueueSpec{{Name: "q"}}, UsagePools: []PoolSpec{{Name: "p"}}},
		Workers: []WorkerGroup{{Tags: map[string]string{"k": "v"}, Queues: []string{"q"}}},
		Jobs:    []JobClass{{Name: "j"}},
		Clients: ClientsSpec{WebSocket: WSSpec{Subscribe: []string{"jobs"}}, RESTPollers: PollSpec{Endpoints: []string{"jobs-list"}}},
		Actions: []Action{{CancelJobs: &Fraction{Fraction: 0.1}}},
		Assert:  map[string]Duration{"a": {}},
	}
	c := orig.Clone()
	nested(t, c.Server.Config, "scheduler")["x"] = 2
	listItem(t, c.Server.Config, "list")["a"] = 2
	c.Farm.Queues[0].Name = "changed"
	c.Farm.UsagePools[0].Name = "changed"
	c.Workers[0].Tags["k"] = "changed"
	c.Workers[0].Queues[0] = "changed"
	c.Jobs[0].Name = "changed"
	c.Clients.WebSocket.Subscribe[0] = "changed"
	c.Clients.RESTPollers.Endpoints[0] = "changed"
	c.Actions[0].CancelJobs.Fraction = 0.9
	c.Assert["b"] = Duration{}
	switch {
	case nested(t, orig.Server.Config, "scheduler")["x"] != 1,
		listItem(t, orig.Server.Config, "list")["a"] != 1,
		orig.Farm.Queues[0].Name != "q",
		orig.Farm.UsagePools[0].Name != "p",
		orig.Workers[0].Tags["k"] != "v",
		orig.Workers[0].Queues[0] != "q",
		orig.Jobs[0].Name != "j",
		orig.Clients.WebSocket.Subscribe[0] != "jobs",
		orig.Clients.RESTPollers.Endpoints[0] != "jobs-list",
		orig.Actions[0].CancelJobs.Fraction != 0.1,
		len(orig.Assert) != 1:
		t.Errorf("changing the clone changed the original: %+v", orig)
	}
	if (*Scenario)(nil).Clone() != nil {
		t.Error("Clone of nil is not nil")
	}
}

// nested returns m[key] as a map, failing the test when it is not one.
func nested(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, not a map", key, m[key])
	}
	return v
}

// listItem returns the first item of the list m[key] as a map.
func listItem(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	l, ok := m[key].([]any)
	if !ok || len(l) == 0 {
		t.Fatalf("%s is %T, not a non-empty list", key, m[key])
	}
	v, ok := l[0].(map[string]any)
	if !ok {
		t.Fatalf("%s[0] is %T, not a map", key, l[0])
	}
	return v
}
