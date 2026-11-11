// SPDX-License-Identifier: AGPL-3.0-or-later

package chaos

import (
	"slices"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

func okResult(name string) verify.Result { return verify.Result{Name: name, OK: true} }

func failedResult(name string, evidence ...string) verify.Result {
	return verify.Result{Name: name, Evidence: evidence}
}

func knownViolation(check, kind string, os ...string) scenario.KnownViolation {
	return scenario.KnownViolation{Check: check, Kind: kind, OS: os, Reason: "because", Tracker: "H13x"}
}

func resultByName(t *testing.T, rs []verify.Result, name string) verify.Result {
	t.Helper()
	for _, r := range rs {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result named %q in %v", name, resultNames(rs))
	return verify.Result{}
}

func resultNames(rs []verify.Result) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}

func TestApply_KnownMustThatFiredBecomesOK(t *testing.T) {
	in := []verify.Result{okResult("terminal"), failedResult("caps", "pool over")}
	exp := scenario.Expect{KnownViolations: []scenario.KnownViolation{knownViolation("caps", scenario.KnownMust)}}
	got := Apply(in, nil, exp, "linux")
	c := resultByName(t, got, "caps")
	if !c.OK || c.Known != "H13x" || !slices.Equal(c.Evidence, []string{"pool over"}) {
		t.Errorf("caps = %+v, want OK with Known H13x and its evidence", c)
	}
	if len(got) != 2 {
		t.Errorf("results = %v, want no extra result", resultNames(got))
	}
	if in[1].OK {
		t.Error("Apply modified its input")
	}
}

func TestApply_KnownMustThatHeldFails(t *testing.T) {
	in := []verify.Result{okResult("terminal"), okResult("caps")}
	exp := scenario.Expect{KnownViolations: []scenario.KnownViolation{knownViolation("caps", scenario.KnownMust)}}
	got := Apply(in, nil, exp, "linux")
	if !slices.Equal(resultNames(got), []string{"terminal", "caps", "known:caps"}) {
		t.Fatalf("results = %v", resultNames(got))
	}
	k := resultByName(t, got, "known:caps")
	if k.OK || len(k.Evidence) != 1 || k.Evidence[0] != "expected caps to fail (H13x: because), it held" {
		t.Errorf("known:caps = %+v", k)
	}
}

func TestApply_KnownMayThatHeldAddsNothing(t *testing.T) {
	in := []verify.Result{okResult("caps")}
	exp := scenario.Expect{KnownViolations: []scenario.KnownViolation{knownViolation("caps", scenario.KnownMay)}}
	if got := Apply(in, nil, exp, "linux"); len(got) != 1 || !got[0].OK || got[0].Known != "" {
		t.Errorf("results = %+v, want the input unchanged", got)
	}
}

func TestApply_KnownMayThatFiredBecomesOK(t *testing.T) {
	in := []verify.Result{failedResult("caps", "x")}
	exp := scenario.Expect{KnownViolations: []scenario.KnownViolation{knownViolation("caps", scenario.KnownMay)}}
	if c := Apply(in, nil, exp, "linux")[0]; !c.OK || c.Known != "H13x" {
		t.Errorf("caps = %+v", c)
	}
}

func TestApply_OSFilter(t *testing.T) {
	in := []verify.Result{failedResult("caps", "x"), okResult("logs")}
	exp := scenario.Expect{KnownViolations: []scenario.KnownViolation{
		knownViolation("caps", scenario.KnownMay, "linux"),
		knownViolation("logs", scenario.KnownMust, "linux"),
	}}
	got := Apply(in, nil, exp, "windows")
	if len(got) != 2 || got[0].OK || got[0].Known != "" || !got[1].OK {
		t.Errorf("results = %+v, want the input unchanged on windows", got)
	}
	// An empty OS list applies on every OS.
	exp.KnownViolations[0].OS = nil
	if got := Apply(in, nil, exp, "windows"); !got[0].OK || got[0].Known == "" {
		t.Errorf("caps = %+v, want a known violation on every OS", got[0])
	}
}

func TestApply_ReworkBound(t *testing.T) {
	in := []verify.Result{okResult("terminal")}
	rw := &report.Rework{Attempts: 1, Seconds: 2.5}
	exp := scenario.Expect{MaxReworkAttempts: new(0)}
	got := Apply(in, rw, exp, "linux")
	if !slices.Equal(resultNames(got), []string{"terminal", "rework"}) {
		t.Fatalf("results = %v", resultNames(got))
	}
	r := resultByName(t, got, "rework")
	if r.OK || len(r.Evidence) == 0 || !strings.Contains(r.Evidence[0], "1 attempt") {
		t.Errorf("rework = %+v, want a failure naming the attempts", r)
	}

	exp.KnownViolations = []scenario.KnownViolation{knownViolation("rework", scenario.KnownMust)}
	r = resultByName(t, Apply(in, rw, exp, "linux"), "rework")
	if !r.OK || r.Known != "H13x" || len(r.Evidence) == 0 {
		t.Errorf("rework = %+v, want OK with Known and evidence kept", r)
	}

	atBound := Apply(in, &report.Rework{Attempts: 1}, scenario.Expect{MaxReworkAttempts: new(1)}, "linux")
	if r := resultByName(t, atBound, "rework"); !r.OK {
		t.Errorf("rework at the bound = %+v, want OK", r)
	}
}

func TestApply_ReworkUnmeasuredFails(t *testing.T) {
	exp := scenario.Expect{MaxReworkAttempts: new(5)}
	r := resultByName(t, Apply(nil, nil, exp, "linux"), "rework")
	if r.OK || len(r.Evidence) != 1 || r.Evidence[0] != "rework could not be measured" {
		t.Errorf("rework = %+v", r)
	}
	// A known violation covers it like any failed check.
	exp.KnownViolations = []scenario.KnownViolation{knownViolation("rework", scenario.KnownMay)}
	if r := resultByName(t, Apply(nil, nil, exp, "linux"), "rework"); !r.OK || r.Known == "" {
		t.Errorf("rework = %+v", r)
	}
}

func TestApply_NoBoundNoReworkResult(t *testing.T) {
	got := Apply([]verify.Result{okResult("terminal")}, &report.Rework{Attempts: 9}, scenario.Expect{}, "linux")
	if len(got) != 1 {
		t.Errorf("results = %v, want only terminal", resultNames(got))
	}
}

func TestApply_MustForACheckThatDidNotRunFails(t *testing.T) {
	exp := scenario.Expect{KnownViolations: []scenario.KnownViolation{
		knownViolation("rework", scenario.KnownMust), knownViolation("no-orphans", scenario.KnownMay),
	}}
	got := Apply([]verify.Result{okResult("terminal")}, &report.Rework{}, exp, "linux")
	if !slices.Equal(resultNames(got), []string{"terminal", "known:rework"}) {
		t.Fatalf("results = %v", resultNames(got))
	}
	k := got[1]
	if k.OK || len(k.Evidence) != 1 || !strings.Contains(k.Evidence[0], "the check did not run") {
		t.Errorf("known:rework = %+v", k)
	}
}

func TestApply_ReworkResultBeforeKnownFailures(t *testing.T) {
	exp := scenario.Expect{
		MaxReworkAttempts: new(0),
		KnownViolations:   []scenario.KnownViolation{knownViolation("caps", scenario.KnownMust)},
	}
	got := Apply([]verify.Result{okResult("caps")}, &report.Rework{}, exp, "linux")
	if !slices.Equal(resultNames(got), []string{"caps", "rework", "known:caps"}) {
		t.Errorf("order = %v", resultNames(got))
	}
}
