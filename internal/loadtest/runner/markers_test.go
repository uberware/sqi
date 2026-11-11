// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/chaos"
	"github.com/uberware/sqi/internal/loadtest/marker"
	"github.com/uberware/sqi/internal/loadtest/target"
)

// markerTarget is a fault target that holds only what markerChecks reads.
type markerTarget struct {
	target.Faults

	dir  string
	incs []target.Incarnation
}

func (m markerTarget) MarkerDir() string                  { return m.dir }
func (m markerTarget) Incarnations() []target.Incarnation { return m.incs }

func TestMarkerChecks_RunWhenARealTaskHasMarkers(t *testing.T) {
	dir := t.TempDir()
	w, err := marker.Begin(dir, "key", "0.0", marker.RoleSilent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Finish(0) }) //nolint:errcheck // the test is over
	if err := marker.Output(dir, "key", w.ID(), time.Now()); err != nil {
		t.Fatal(err)
	}

	// The incarnation ended a minute ago and the exec is still alive.
	r := &run{faults: markerTarget{dir: dir, incs: []target.Incarnation{{ID: "0.0", Ended: time.Now().Add(-time.Minute)}}}}
	got := r.markerChecks()
	if len(got) != 2 || got[0].Name != chaos.NameNoOrphans || got[1].Name != chaos.NameSingleWriter {
		t.Fatalf("markerChecks = %+v, want no-orphans then single-writer", got)
	}
	if got[0].OK || !strings.Contains(strings.Join(got[0].Evidence, "\n"), w.ID()) {
		t.Errorf("no-orphans = %+v, want it to fail naming %s", got[0], w.ID())
	}
	if !got[1].OK {
		t.Errorf("single-writer failed: %q", got[1].Evidence)
	}

	r = &run{faults: markerTarget{dir: dir, incs: []target.Incarnation{{ID: "0.0"}}}}
	if got := r.markerChecks(); !got[0].OK {
		t.Errorf("no-orphans with a running incarnation failed: %q", got[0].Evidence)
	}
}

func TestMarkerChecks_NoneWithoutAMarkerDir(t *testing.T) {
	if got := (&run{faults: markerTarget{}}).markerChecks(); got != nil {
		t.Errorf("markerChecks = %+v, want none", got)
	}
	if got := (&run{}).markerChecks(); got != nil {
		t.Errorf("markerChecks outside chaos mode = %+v, want none", got)
	}
}

func TestMainMarkers_ReportsSkippedFiles(t *testing.T) {
	dir := t.TempDir()
	w, err := marker.Begin(dir, "key", "0.0", marker.RoleMain)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Finish(0); err != nil {
		t.Fatal(err)
	}
	torn := filepath.Join(dir, marker.ExecsDir, "torn.json")
	if err := os.WriteFile(torn, []byte(`{"id":"torn","key":"k`), 0o600); err != nil {
		t.Fatal(err)
	}
	execs, skipped, err := mainMarkers(dir)
	if err != nil || skipped != 1 || len(execs) != 1 || execs[0].ID != w.ID() {
		t.Errorf("mainMarkers = %+v, skipped %d, %v; want the one main exec and 1 skipped", execs, skipped, err)
	}
}
