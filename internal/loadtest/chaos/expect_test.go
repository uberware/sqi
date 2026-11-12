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

// reworkViolation is a known violation on rework with a ceiling.
func reworkViolation(kind string, ceiling int) scenario.KnownViolation {
	k := knownViolation(scenario.ReworkCheck, kind)
	k.MaxAttempts = &ceiling
	return k
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

	exp.KnownViolations = []scenario.KnownViolation{reworkViolation(scenario.KnownMust, 10)}
	r = resultByName(t, Apply(in, rw, exp, "linux"), "rework")
	if !r.OK || r.Known != "H13x" || len(r.Evidence) == 0 {
		t.Errorf("rework = %+v, want OK with Known and evidence kept", r)
	}

	atBound := Apply(in, &report.Rework{Attempts: 1}, scenario.Expect{MaxReworkAttempts: new(1)}, "linux")
	if r := resultByName(t, atBound, "rework"); !r.OK {
		t.Errorf("rework at the bound = %+v, want OK", r)
	}
}

func TestApply_ReworkKnownCeiling(t *testing.T) {
	tests := []struct {
		name     string
		attempts int
		kind     string
		wantOK   bool
		wantKnow bool // a failed known:rework result is expected
	}{
		{name: "within the ceiling", attempts: 6, kind: scenario.KnownMust, wantOK: true},
		{name: "at the ceiling", attempts: 10, kind: scenario.KnownMay, wantOK: true},
		{name: "over the ceiling, may", attempts: 11, kind: scenario.KnownMay},
		{name: "over the ceiling, must", attempts: 500, kind: scenario.KnownMust},
		{name: "within the bound, must", attempts: 2, kind: scenario.KnownMust, wantOK: true, wantKnow: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exp := scenario.Expect{
				MaxReworkAttempts: new(2),
				KnownViolations:   []scenario.KnownViolation{reworkViolation(tt.kind, 10)},
			}
			got := Apply(nil, &report.Rework{Attempts: tt.attempts, Seconds: 1}, exp, "linux")
			r := resultByName(t, got, "rework")
			if r.OK != tt.wantOK {
				t.Fatalf("rework = %+v, want OK=%v", r, tt.wantOK)
			}
			switch {
			case tt.wantOK && tt.attempts > 2 && r.Known != "H13x":
				t.Errorf("rework = %+v, want Known H13x", r)
			case tt.wantOK && tt.attempts > 2 && !slices.ContainsFunc(r.Evidence, func(s string) bool { return strings.Contains(s, "within the known ceiling of 10") }):
				t.Errorf("rework evidence = %q, want it to show the known ceiling", r.Evidence)
			case !tt.wantOK && r.Known != "":
				t.Errorf("rework = %+v, an over-ceiling result must not be known", r)
			case !tt.wantOK && !slices.ContainsFunc(r.Evidence, func(s string) bool { return strings.Contains(s, "known ceiling of 10") }):
				t.Errorf("rework evidence = %q, want it to say the known ceiling of 10 was exceeded", r.Evidence)
			}
			hasKnown := slices.Contains(resultNames(got), "known:rework")
			if hasKnown != tt.wantKnow {
				t.Errorf("results = %v, known:rework present = %v, want %v", resultNames(got), hasKnown, tt.wantKnow)
			}
		})
	}
}

func TestApply_ReworkEntryWithoutCeilingExcusesNothing(t *testing.T) {
	exp := scenario.Expect{
		MaxReworkAttempts: new(0),
		KnownViolations:   []scenario.KnownViolation{knownViolation(scenario.ReworkCheck, scenario.KnownMay)},
	}
	if r := resultByName(t, Apply(nil, &report.Rework{Attempts: 1}, exp, "linux"), "rework"); r.OK || r.Known != "" {
		t.Errorf("rework = %+v, want a failure", r)
	}
}

func TestApply_ReworkUnmeasuredFails(t *testing.T) {
	exp := scenario.Expect{MaxReworkAttempts: new(5)}
	r := resultByName(t, Apply(nil, nil, exp, "linux"), "rework")
	if r.OK || len(r.Evidence) != 1 || r.Evidence[0] != "rework could not be measured" {
		t.Errorf("rework = %+v", r)
	}
	// A known violation never covers a measurement that was not made.
	for _, kind := range []string{scenario.KnownMay, scenario.KnownMust} {
		exp.KnownViolations = []scenario.KnownViolation{reworkViolation(kind, 60)}
		got := Apply(nil, nil, exp, "linux")
		if r := resultByName(t, got, "rework"); r.OK || r.Known != "" {
			t.Errorf("%s: rework = %+v, want it failed and not known", kind, r)
		}
		hasKnown := slices.Contains(resultNames(got), "known:rework")
		if kind == scenario.KnownMust && !hasKnown {
			t.Errorf("must: results = %v, want a failed known:rework, the entry did not fire", resultNames(got))
		}
		if kind == scenario.KnownMay && hasKnown {
			t.Errorf("may: results = %v, want no known:rework", resultNames(got))
		}
		if hasKnown {
			k := resultByName(t, got, "known:rework")
			if k.OK || !strings.Contains(k.Evidence[0], "could not be measured") {
				t.Errorf("known:rework = %+v, want a failure saying it could not be measured", k)
			}
		}
	}
}

func TestApply_UnmeasuredResultIsNeverKnown(t *testing.T) {
	unmeasured := verify.Result{Name: "store-integrity", Evidence: []string{"open the store: boom"}, Unmeasured: true}
	for _, kind := range []string{scenario.KnownMay, scenario.KnownMust} {
		exp := scenario.Expect{KnownViolations: []scenario.KnownViolation{knownViolation("store-integrity", kind)}}
		got := Apply([]verify.Result{unmeasured}, nil, exp, "linux")
		r := resultByName(t, got, "store-integrity")
		if r.OK || r.Known != "" {
			t.Errorf("%s: store-integrity = %+v, want it failed and not known", kind, r)
		}
		hasKnown := slices.Contains(resultNames(got), "known:store-integrity")
		if kind == scenario.KnownMust {
			if !hasKnown || !strings.Contains(resultByName(t, got, "known:store-integrity").Evidence[0], "could not be measured") {
				t.Errorf("must: results = %+v, want a failed known:store-integrity naming the missing measurement", got)
			}
		} else if hasKnown {
			t.Errorf("may: results = %v, want no known:store-integrity", resultNames(got))
		}
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
