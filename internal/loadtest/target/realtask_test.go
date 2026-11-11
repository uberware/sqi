// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/marker"
	"github.com/uberware/sqi/internal/loadtest/scenario"
)

func realTaskScenario() *scenario.Scenario {
	sc := testScenario(scenario.ServerSpec{Store: "sqlite"})
	sc.Jobs = []scenario.JobClass{
		{Name: "slow", RealTask: &scenario.RealTask{
			Duration: scenario.Duration{Duration: 1500 * time.Millisecond}, LogEvery: scenario.Duration{Duration: 250 * time.Millisecond},
			SilentChild: true,
		}},
		{Name: "chatty", RealTask: &scenario.RealTask{Duration: scenario.Duration{Duration: time.Second}, ChattyChild: true}},
		{Name: "plain"},
	}
	return sc
}

func TestManaged_NoopConfig(t *testing.T) {
	m := newTestManaged(t, realTaskScenario())
	m.cfg.NoopBin = "/opt/sqi/noop"
	if err := m.writeNoopConfig(); err != nil {
		t.Fatalf("writeNoopConfig: %v", err)
	}
	path := filepath.Join(m.settings.RunDir, "noop.json")
	cmd := m.NoopCommand()
	if cmd.Path != "/opt/sqi/noop" || !slices.Equal(cmd.Args, []string{marker.ConfigArg(path)}) {
		t.Errorf("NoopCommand = %+v, want the no-op with %s first", cmd, marker.ConfigArg(path))
	}
	got, err := marker.ReadConfig(path)
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	want := marker.NoopConfig{MarkerDir: m.MarkerDir(), Classes: map[string]marker.NoopClass{
		"slow":   {DurationMS: 1500, LogEveryMS: 250, SilentChild: true},
		"chatty": {DurationMS: 1000, ChattyChild: true},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("noop.json = %+v, want %+v", got, want)
	}

	plain := newTestManaged(t, laneScenario(false))
	plain.cfg.NoopBin = "/opt/sqi/noop"
	if err := plain.writeNoopConfig(); err != nil {
		t.Fatalf("writeNoopConfig: %v", err)
	}
	if cmd := plain.NoopCommand(); len(cmd.Args) != 0 {
		t.Errorf("NoopCommand without real_task = %+v, want no arguments", cmd)
	}
	if _, err := os.Stat(filepath.Join(plain.settings.RunDir, "noop.json")); !os.IsNotExist(err) {
		t.Errorf("noop.json written without real_task (stat err %v)", err)
	}
}

func TestManaged_LiveTaskGroups(t *testing.T) {
	m := newTestManaged(t, realTaskScenario())
	dir := filepath.Join(m.MarkerDir(), marker.ExecsDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ended := now.Add(-time.Second)
	for _, e := range []marker.Exec{
		{ID: "a", Incarnation: "0.0", PGID: 100, LastSeen: now},
		{ID: "b", Incarnation: "0.0", PGID: 100, LastSeen: now},                 // the same group again
		{ID: "c", Incarnation: "0.0", PGID: 200, LastSeen: ended, End: &ended},  // ended
		{ID: "d", Incarnation: "0.0", PGID: 300, LastSeen: now.Add(-time.Hour)}, // killed long ago
		{ID: "e", Incarnation: "0.1", PGID: 400, LastSeen: now},                 // another incarnation
		{ID: "f", Incarnation: "0.0", PGID: 0, LastSeen: now},                   // no group (Windows)
	} {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.ID+".json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := m.liveTaskGroups("0.0")
	if err != nil || !slices.Equal(got, []int{100}) {
		t.Errorf("liveTaskGroups(0.0) = %v, %v; want [100]", got, err)
	}
	got, err = m.liveTaskGroups("")
	if err != nil || !slices.Equal(got, []int{100, 400}) {
		t.Errorf("liveTaskGroups(any) = %v, %v; want [100 400]", got, err)
	}
}
