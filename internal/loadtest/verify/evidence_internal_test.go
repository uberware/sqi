// SPDX-License-Identifier: AGPL-3.0-or-later

package verify

import (
	"fmt"
	"slices"
	"testing"
)

// TestEvidence_ResultKeepsEveryViolation pins that a result carries every
// violation beside the evidence it reports, which MaxEvidence caps, and no
// note: a known violation scoped by a pattern judges all of them.
func TestEvidence_ResultKeepsEveryViolation(t *testing.T) {
	var ev Evidence
	var want []string
	for i := range MaxEvidence + 5 {
		s := fmt.Sprintf("violation %d", i)
		want = append(want, s)
		ev.Add(s)
	}
	ev.Note("a note")
	r := ev.Result("x")
	if len(r.Evidence) != MaxEvidence+1 { // the capped lines, then the note
		t.Errorf("evidence has %d lines, want %d", len(r.Evidence), MaxEvidence+1)
	}
	if !slices.Equal(r.Violations, want) || r.ViolationsCut {
		t.Errorf("violations = %d lines (cut %v), want all %d and not cut", len(r.Violations), r.ViolationsCut, len(want))
	}
}

func TestEvidence_ResultCutsViolationsPastTheKeepLimit(t *testing.T) {
	var ev Evidence
	for i := range MaxKeptViolations + 1 {
		ev.Add(fmt.Sprintf("violation %d", i))
	}
	r := ev.Result("x")
	if !r.ViolationsCut || r.Violations != nil {
		t.Errorf("violations = %d lines (cut %v), want none kept and cut", len(r.Violations), r.ViolationsCut)
	}
}
