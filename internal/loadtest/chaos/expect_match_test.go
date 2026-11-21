// SPDX-License-Identifier: AGPL-3.0-or-later

package chaos

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

// scopedViolation is a known violation that excuses only the violations
// matching pattern.
func scopedViolation(check, kind, tracker, pattern string) scenario.KnownViolation {
	k := knownViolation(check, kind)
	k.Tracker, k.Match = tracker, pattern
	return k
}

// violationsResult is a failed result built the way the checks build one, so
// it carries every violation beside its capped evidence.
func violationsResult(name string, lines ...string) verify.Result {
	var ev verify.Evidence
	ev.AddAll(lines)
	return ev.Result(name)
}

func TestApply_ScopedEntryCoversEveryViolation(t *testing.T) {
	in := []verify.Result{violationsResult("single-attempt", "task t1: attempts a1 and a2 overlap", "task t2: attempts a3 and a4 overlap")}
	exp := scenario.Expect{KnownViolations: []scenario.KnownViolation{
		scopedViolation("single-attempt", scenario.KnownMust, "H13x", `attempts \S+ and \S+ overlap`),
	}}
	got := Apply(in, nil, exp, "linux")
	r := resultByName(t, got, "single-attempt")
	if !r.OK || r.Known != "H13x" {
		t.Errorf("single-attempt = %+v, want OK and known", r)
	}
	if len(got) != 1 {
		t.Errorf("results = %v, want no extra result", resultNames(got))
	}
}

func TestApply_ScopedEntryLeavesAnUnmatchedViolationFailed(t *testing.T) {
	in := []verify.Result{violationsResult("single-attempt", "task t1: attempts a1 and a2 overlap", "task t9: leased to two workers at once")}
	exp := scenario.Expect{KnownViolations: []scenario.KnownViolation{
		scopedViolation("single-attempt", scenario.KnownMay, "H13x", `overlap$`),
	}}
	r := resultByName(t, Apply(in, nil, exp, "linux"), "single-attempt")
	if r.OK || r.Known != "" {
		t.Fatalf("single-attempt = %+v, want it failed and not known", r)
	}
	all := strings.Join(r.Evidence, "\n")
	for _, want := range []string{"1 of 2 violations match no known violation's pattern", "task t9: leased to two workers at once"} {
		if !strings.Contains(all, want) {
			t.Errorf("evidence %q does not contain %q", r.Evidence, want)
		}
	}
}

// TestApply_ScopedEntryJudgesViolationsPastTheEvidenceCap pins that the
// pattern is checked against every violation, including those past the
// evidence cap, so a new defect whose violation sorts after the cap is not
// hidden behind the recorded one.
func TestApply_ScopedEntryJudgesViolationsPastTheEvidenceCap(t *testing.T) {
	var lines []string
	for i := range verify.MaxEvidence + 5 {
		lines = append(lines, fmt.Sprintf("task t%d: attempts overlap", i))
	}
	lines = append(lines, "task tz: leased to two workers at once")
	in := []verify.Result{violationsResult("single-attempt", lines...)}
	if slices.Contains(in[0].Evidence, lines[len(lines)-1]) {
		t.Fatal("the new violation is in the capped evidence; the test proves nothing")
	}
	exp := scenario.Expect{KnownViolations: []scenario.KnownViolation{
		scopedViolation("single-attempt", scenario.KnownMay, "H13x", `attempts overlap$`),
	}}
	r := resultByName(t, Apply(in, nil, exp, "linux"), "single-attempt")
	if r.OK {
		t.Fatalf("single-attempt = %+v, want the violation past the cap to keep it failed", r)
	}
	if !strings.Contains(strings.Join(r.Evidence, "\n"), "task tz: leased to two workers at once") {
		t.Errorf("evidence %q does not name the unmatched violation", r.Evidence)
	}
}

func TestApply_ScopedEntriesCoverTogether(t *testing.T) {
	in := []verify.Result{violationsResult("terminal", "job j1 (running): stream broken", "job j2 (pending): never leased")}
	exp := scenario.Expect{KnownViolations: []scenario.KnownViolation{
		scopedViolation("terminal", scenario.KnownMust, "H13x", `stream broken`),
		scopedViolation("terminal", scenario.KnownMust, "H13y", `never leased`),
	}}
	got := Apply(in, nil, exp, "linux")
	r := resultByName(t, got, "terminal")
	if !r.OK || r.Known != "H13x, H13y" {
		t.Errorf("terminal = %+v, want OK and known to both", r)
	}
	if len(got) != 1 {
		t.Errorf("results = %v, want both must entries fired", resultNames(got))
	}
}

func TestApply_ScopedMustThatMatchedNothingFails(t *testing.T) {
	in := []verify.Result{violationsResult("terminal", "job j1 (running): never leased")}
	exp := scenario.Expect{KnownViolations: []scenario.KnownViolation{
		knownViolation("terminal", scenario.KnownMay),
		scopedViolation("terminal", scenario.KnownMust, "H13y", `stream broken`),
	}}
	got := Apply(in, nil, exp, "linux")
	if r := resultByName(t, got, "terminal"); !r.OK || r.Known != "H13x" {
		t.Errorf("terminal = %+v, want it excused by the unscoped entry alone", r)
	}
	k := resultByName(t, got, "known:terminal")
	if k.OK || !strings.Contains(k.Evidence[0], "it failed, but no violation matches its pattern") {
		t.Errorf("known:terminal = %+v, want a failure saying the pattern matched nothing", k)
	}
}

func TestApply_ScopedEntryCannotJudgeTooManyViolations(t *testing.T) {
	res := failedResult("logs", "chunk missing")
	res.ViolationsCut = true
	exp := scenario.Expect{KnownViolations: []scenario.KnownViolation{
		scopedViolation("logs", scenario.KnownMay, "H13x", `chunk missing`),
	}}
	r := resultByName(t, Apply([]verify.Result{res}, nil, exp, "linux"), "logs")
	if r.OK || !strings.Contains(strings.Join(r.Evidence, "\n"), "too many violations") {
		t.Errorf("logs = %+v, want it failed, saying there were too many violations to judge", r)
	}
}

// TestApply_ScopedEntryWithoutViolationsJudgesEvidence covers a result not
// built from verify.Evidence: its evidence lines are the violations.
func TestApply_ScopedEntryWithoutViolationsJudgesEvidence(t *testing.T) {
	exp := scenario.Expect{KnownViolations: []scenario.KnownViolation{
		scopedViolation("caps", scenario.KnownMay, "H13x", `^pool over`),
	}}
	if r := Apply([]verify.Result{failedResult("caps", "pool over by 2")}, nil, exp, "linux")[0]; !r.OK {
		t.Errorf("caps = %+v, want OK", r)
	}
	if r := Apply([]verify.Result{failedResult("caps", "pool over by 2", "queue over by 1")}, nil, exp, "linux")[0]; r.OK {
		t.Errorf("caps = %+v, want it failed on the unmatched line", r)
	}
}
