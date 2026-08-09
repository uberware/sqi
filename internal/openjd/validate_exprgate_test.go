// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for the EXPR expression walk's registry-status gate: while the EXPR
// extension is registered but not StatusSupported, ValidateWithOptions must
// reject an EXPR-declaring template WITHOUT running checkTemplateExpressions
// over it.
//
// This is a cost property, not only a behavior one. The walk's operation and
// byte budgets are per expression position, with no template-wide cap and no
// bound on the number of positions, so its cost is linear in template size --
// measured at ~134 microseconds of CPU per template byte before the gate
// existed. POST /api/v1/jobs accepts a 4 MiB body, anonymously when auth is
// off (the default), and answers an EXPR template with a single
// "registered but not yet supported" error: without the gate, that error costs
// minutes of server CPU to produce. Gating restores the pre-E2 cost.

import (
	"runtime"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// exprGatePointer and exprGateMessage are the exact error the status gate
// (validateExtensions) produces for a template declaring EXPR while its
// registry entry is
// StatusInProgress. Pinned byte-for-byte here because the whole point of the
// fix is that the verdict is UNCHANGED -- only the cost of reaching it moves.
const (
	exprGatePointer = "/extensions/0"
	exprGateMessage = `extension "EXPR" is registered but not yet supported (status "in-progress")`
)

// exprGateHostileTemplate builds an EXPR-declaring template with n action
// argument positions, each holding an expression that is cheap to parse and
// very expensive to evaluate (it materializes two 400 KB strings), plus one
// host-requirement value holding a reference that is out of scope at ScopeJob
// and that the walk -- and only the walk -- reports for an EXPR template.
//
// The two halves test different things: the expensive positions make the walk
// visible in allocation volume, the out-of-scope reference makes it visible in
// the error set. Neither depends on wall-clock time.
func exprGateHostileTemplate(n int) string {
	var b strings.Builder
	b.WriteString(`specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: TestJob
steps:
- name: Step1
  hostRequirements:
    attributes:
      - name: attr.custom.dir
        anyOf:
          - "{{ Session.WorkingDirectory }}"
  script:
    actions:
      onRun:
        command: echo
        args:
`)
	for range n {
		b.WriteString(`        - "{{ len(('a' * 400000).upper()) }}"` + "\n")
	}
	return b.String()
}

// totalAllocDelta returns the number of bytes fn allocated on the heap.
//
// runtime.MemStats.TotalAlloc is cumulative and never decreases, so garbage
// collection cannot perturb the delta -- unlike a wall-clock measurement,
// which is what makes this assertable on shared CI. It is a proxy for "did the
// evaluator run", chosen because the evaluator's work is dominated by building
// large string values, and the skipped path allocates three to four orders of
// magnitude less.
func totalAllocDelta(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestValidate_EXPRWalkSkippedWhileExtensionUnsupported is the regression test
// for the walk's reachability from production. It asserts three things:
//
//  1. VERDICT UNCHANGED. The template is still rejected, with exactly the
//     status-gate error, at exactly the pointer it had before the gate --
//     byte-for-byte. Weakening the status gate would be a far worse bug than
//     the cost it exists to bound.
//  2. WALK DID NOT RUN. The out-of-scope Session reference in the template is
//     reported by checkTemplateExpressions and by nothing else on an
//     EXPR-declaring template, so its absence from the error set is direct
//     evidence the walk was skipped -- not merely that it was fast.
//  3. COST BOUNDED. The default path allocates a trivial amount, and the
//     opt-in path (what test/conformance uses) allocates orders of magnitude
//     more for the same input. The ratio is self-calibrating: it stays true on
//     any machine, because it compares the two paths against each other rather
//     than against a fixed budget.
func TestValidate_EXPRWalkSkippedWhileExtensionUnsupported(t *testing.T) {
	// 100 expensive positions: enough that a walk allocates tens of MB, few
	// enough that the opt-in half of this test stays well under a second.
	const positions = 100

	tmpl := mustParse(t, exprGateHostileTemplate(positions))

	var gated openjd.ValidationErrors
	gatedAlloc := totalAllocDelta(func() {
		gated = openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: true})
	})

	// (1) The verdict, byte for byte.
	if len(gated) != 1 {
		t.Fatalf("expected exactly the status-gate error and nothing else; got %d: %v", len(gated), gated)
	}
	if gated[0].Pointer != exprGatePointer || gated[0].Message != exprGateMessage {
		t.Fatalf("status-gate error changed:\n got pointer=%q message=%q\nwant pointer=%q message=%q",
			gated[0].Pointer, gated[0].Message, exprGatePointer, exprGateMessage)
	}

	// (2) No expression error, therefore no walk.
	if containsMessage(gated, "Session") {
		t.Fatalf("the expression walk ran for a template the status gate already rejects; got: %v", gated)
	}

	// (3) The same input, with the conformance harness's opt-in.
	var walked openjd.ValidationErrors
	walkedAlloc := totalAllocDelta(func() {
		walked = openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{
			EnforceLimits:                        true,
			CheckEXPRExpressionsWhileUnsupported: true,
		})
	})
	if !containsMessage(walked, "Session") {
		t.Fatalf("CheckEXPRExpressionsWhileUnsupported must still run the walk "+
			"(test/conformance depends on it); got: %v", walked)
	}

	// Each walked position materializes at least one 400 KB string, so the
	// walked path must allocate at least ~40 MB for 100 positions. The gated
	// path only re-reads an already-parsed template. A 32x floor on the ratio
	// leaves an enormous margin either way while still failing loudly if the
	// gate is removed and the walk silently comes back.
	const minRatio = 32
	if walkedAlloc < gatedAlloc*minRatio {
		t.Errorf("expected the walked path to allocate at least %dx the gated path, "+
			"which would show the gate is really skipping the evaluator; "+
			"gated=%d bytes walked=%d bytes", minRatio, gatedAlloc, walkedAlloc)
	}
	// Absolute ceiling as well: independent of the ratio, skipping the walk
	// must not cost anything like one position's worth of evaluation.
	const maxGatedAlloc = 8 << 20 // 8 MiB
	if gatedAlloc > maxGatedAlloc {
		t.Errorf("validating a %d-position EXPR template with the walk gated off "+
			"allocated %d bytes, over the %d-byte ceiling; the walk is probably running",
			positions, gatedAlloc, maxGatedAlloc)
	}
	t.Logf("gated=%d bytes, walked=%d bytes (%d expression positions)", gatedAlloc, walkedAlloc, positions)
}

// TestValidate_EXPRWalkGate_LeavesNonEXPRTemplatesAlone confirms the gate did
// not change anything for a template that does not declare EXPR: the base-spec
// format-string path (validateFormatString) is not gated by it, so the same
// out-of-scope reference is still rejected either way.
func TestValidate_EXPRWalkGate_LeavesNonEXPRTemplatesAlone(t *testing.T) {
	tmpl := mustParse(t, `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
- name: Step1
  hostRequirements:
    attributes:
      - name: attr.custom.dir
        anyOf:
          - "{{Session.WorkingDirectory}}"
  script:
    actions:
      onRun:
        command: echo
`)
	for _, optIn := range []bool{false, true} {
		errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{
			EnforceLimits:                        true,
			CheckEXPRExpressionsWhileUnsupported: optIn,
		})
		if !containsMessage(errs, "Session.WorkingDirectory") {
			t.Fatalf("optIn=%v: a base-spec out-of-scope reference must still be rejected; got: %v", optIn, errs)
		}
	}
}
