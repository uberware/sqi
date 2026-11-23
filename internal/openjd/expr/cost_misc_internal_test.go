// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"fmt"
	"strings"
	"testing"
)

// The tests in this file pin section 1.3.10's charges for the regex, repr_*,
// math and path groups. The reasoning for each Cost declaration, including
// where sqi's count differs from the reference implementation's, is in the
// comment beside the row in funcsre.go, funcsreprshell.go, funcsreprdata.go,
// funcsmath.go, funcspath.go and pathmapping.go. A long string in a case below
// is built in Go and spliced into the source as a literal, never with the
// expression language's own "*" operator, whose rule-3 charge would enter the
// count.

// opsForOpts is opsFor (meter_internal_test.go) with Option support, needed
// for apply_path_mapping's own test below (the only row here that needs
// WithPathMapping threaded through).
func opsForOpts(t *testing.T, src string, opts ...Option) int64 {
	t.Helper()
	e, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	ec := newEvalCtx(src, MapSymbols(nil), opts)
	if _, err := evalNode(e.root, ec, TAny, 0); err != nil {
		t.Fatalf("eval(%q): %v", src, err)
	}
	return ec.m.ops
}

// TestOperationCount_RegexReprMathPath is the main per-group table, one row
// per function name in the regex, repr, math and path groups (funcsre.go,
// funcsreprshell.go, funcsreprdata.go, funcsmath.go, funcspath.go), covering the ordinary (non-divergent, non-boundary) case for
// each. The divergences and discriminating boundary cases each get their own
// dedicated test below.
func TestOperationCount_RegexReprMathPath(t *testing.T) {
	a300 := strings.Repeat("a", 300)
	p10 := "/" + strings.Repeat("a", 10)   // 11 bytes of path text
	p299 := "/" + strings.Repeat("a", 298) // 299 bytes of path text

	tests := []struct {
		name string
		src  string
		want int64
	}{
		// --- funcsre.go: subject-string (arg 0) ArgBytes ---
		{"re_match", fmt.Sprintf("re_match('%s', 'a')", a300), 3},
		{"re_search", fmt.Sprintf("re_search('%s', 'a')", a300), 3},
		{"re_findall", fmt.Sprintf("re_findall('%s', 'a')", a300), 3},
		{"re_sub", fmt.Sprintf("re_sub('%s', 'a', 'x')", a300), 3},
		{"re_escape", fmt.Sprintf("re_escape('%s')", a300), 3},
		{"re_split (2-arg)", fmt.Sprintf("re_split('%s', 'a')", a300), 3},
		{"re_split (3-arg)", fmt.Sprintf("re_split('%s', 'a', 2)", a300), 3},

		// --- funcsreprshell.go: repr_sh, repr_cmd, repr_pwsh ---
		{"repr_sh(string)", fmt.Sprintf("repr_sh('%s')", a300), 3},
		{"repr_sh(path)", fmt.Sprintf("repr_sh(path('%s'))", p299), 6},
		{"repr_cmd(string)", fmt.Sprintf("repr_cmd('%s')", a300), 3},
		{"repr_pwsh(string)", fmt.Sprintf("repr_pwsh('%s')", a300), 3},
		{"repr_pwsh(path)", fmt.Sprintf("repr_pwsh(path('%s'))", p299), 6},
		{"repr_pwsh(int)", "repr_pwsh(5)", 1},
		{"repr_pwsh(float)", "repr_pwsh(5.0)", 1},
		{"repr_pwsh(bool)", "repr_pwsh(true)", 1},

		// --- funcsreprdata.go: repr_py, repr_json ---
		{"repr_py(string)", fmt.Sprintf("repr_py('%s')", a300), 3},
		{"repr_py(path)", fmt.Sprintf("repr_py(path('%s'))", p299), 6},
		{"repr_py(null)", "repr_py(null)", 1},
		{"repr_json(string)", fmt.Sprintf("repr_json('%s')", a300), 3},
		{"repr_json(path)", fmt.Sprintf("repr_json(path('%s'))", p299), 6},
		{"repr_json(null)", "repr_json(null)", 1},

		// --- funcsmath.go: abs, floor, ceil, round scalar rows ---
		{"abs(int)", "abs(1)", 1},
		{"abs(float)", "abs(1.5)", 1},
		{"floor(float)", "floor(1.5)", 1},
		{"floor(int)", "floor(1)", 1},
		{"ceil(float)", "ceil(1.5)", 1},
		{"ceil(int)", "ceil(1)", 1},
		{"round(1-arg)", "round(1.5)", 1},
		{"round(float,0)", "round(2.0,0)", 1},
		{"round(int,int)", "round(2,1)", 1},

		// --- funcsmath.go: min/max/sum over a list ---
		{"min(list)", "min([3,1,2])", 4},
		{"max(list)", "max([3,1,2])", 4},
		{"sum(list)", "sum([1,2,3,4,5])", 6},

		// --- funcspath.go: path(), as_posix, is_absolute, properties ---
		{"path(string)", fmt.Sprintf("path('%s')", p299), 3},
		{"as_posix", fmt.Sprintf("path('%s').as_posix()", p10), 4},
		{"is_absolute", fmt.Sprintf("path('%s').is_absolute()", p10), 3},
		{"name", fmt.Sprintf("path('%s').name", p10), 4},
		{"stem", fmt.Sprintf("path('%s').stem", p10), 4},
		{"suffix", fmt.Sprintf("path('%s').suffix", p10), 4},
		{"suffixes", fmt.Sprintf("path('%s').suffixes", p10), 4},
		{"parent", fmt.Sprintf("path('%s').parent", p10), 4},
		{"parts", fmt.Sprintf("path('%s').parts", p10), 4},
		{"with_name", fmt.Sprintf("path('%s').with_name('x')", p10), 4},
		{"with_stem", "path('/a.txt').with_stem('x')", 4},
		{"with_suffix", fmt.Sprintf("path('%s').with_suffix('.png')", p10), 4},
		{"with_number(path,int)", fmt.Sprintf("path('%s').with_number(5)", p10), 4},
		{"with_number(string,int)", "with_number('a', 5)", 2},
		{"is_relative_to", "path('/a/b').is_relative_to(path('/a'))", 7},
		{"relative_to", "path('/a/b').relative_to(path('/a'))", 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := opsFor(t, tt.src); got != tt.want {
				t.Errorf("ops(%s) = %d; want %d", tt.src, got, tt.want)
			}
		})
	}
}

// TestOperationCount_ApplyPathMapping is apply_path_mapping's own test.
//
// It has no oracle coverage: scripts/expr-oracle.py invokes the reference
// with only `src` and `target_type` (openjd.expr's evaluate_with_metrics and
// evaluate take values, profile, target_type, path_format, memory_limit,
// operation_limit, with no host_context parameter), so the function is not
// resolvable through the oracle's entry point ("Unknown function:
// 'apply_path_mapping'"). This charge is pinned by this unit test alone, per
// pathmapping.go's own cost comment.
func TestOperationCount_ApplyPathMapping(t *testing.T) {
	rules := []PathMapRule{{PathMapPOSIX, "/projects", "/mnt/projects"}}

	t.Run("short input, passthrough (no rule matches)", func(t *testing.T) {
		got := opsForOpts(t, "apply_path_mapping('/a/b')", WithPathMapping(rules))
		if got != 2 {
			t.Errorf("ops = %d; want 2 (1 call + ceil(4/256)=1)", got)
		}
	})

	t.Run("300-byte input scales", func(t *testing.T) {
		long := "/projects/" + strings.Repeat("a", 300)
		src := fmt.Sprintf("apply_path_mapping('%s')", long)
		got := opsForOpts(t, src, WithPathMapping(rules))
		want := int64(1 + (len(long)+255)/256) // 1 call + ceil(len/256)
		if got != want {
			t.Errorf("ops(%s) = %d; want %d", src, got, want)
		}
	})

	t.Run("rule matching does not change the charge", func(t *testing.T) {
		matched := opsForOpts(t, "apply_path_mapping('/projects/shot01/out.exr')", WithPathMapping(rules))
		unmatched := opsForOpts(t, "apply_path_mapping('/projects/shot01/out.exr')", WithPathMapping(nil))
		if matched != unmatched {
			t.Errorf("ops = %d (matched), %d (passthrough); want equal -- the charge tracks the INPUT's bytes, not whether a rule fired", matched, unmatched)
		}
	})

	t.Run("method-call form charges identically to function-call form", func(t *testing.T) {
		fn := opsForOpts(t, "apply_path_mapping('/a/b')", WithPathMapping(rules))
		method := opsForOpts(t, "'/a/b'.apply_path_mapping()", WithPathMapping(rules))
		if fn != method {
			t.Errorf("ops = %d (function form), %d (method form); want equal", fn, method)
		}
	})
}

// TestOperationCount_RegexFunctionsChargeSubjectOnly pins that the pattern
// (arg 1) and, for re_sub, the replacement (arg 2) never contribute to the
// charge, no matter how large — only the subject's (arg 0) byte length does.
func TestOperationCount_RegexFunctionsChargeSubjectOnly(t *testing.T) {
	a300 := strings.Repeat("a", 300)
	longPattern := "a" + strings.Repeat("|b", 150)

	if got := opsFor(t, fmt.Sprintf("re_match('abc', '%s')", longPattern)); got != 2 {
		t.Errorf("ops(re_match, long pattern) = %d; want 2 -- a 3-byte subject charges 1+ceil(3/256)=2 regardless of the 300+-byte pattern", got)
	}
	if got := opsFor(t, fmt.Sprintf("re_sub('abc', 'a', '%s')", a300)); got != 2 {
		t.Errorf("ops(re_sub, long repl) = %d; want 2 -- the replacement's length does not matter", got)
	}
	if got := opsFor(t, fmt.Sprintf("re_sub('%s', 'a', 'x')", a300)); got != 3 {
		t.Errorf("ops(re_sub, long subject) = %d; want 3 -- the SUBJECT's length is what matters", got)
	}
}

// TestOperationCount_RegexFindallSplitDoNotChargeElements pins the
// ResultElements question for re_findall and re_split: both produce a list,
// but the charge tracks the subject's byte
// length, not the number of matches/parts produced. Discriminated by holding
// match/split density fixed and growing the subject: an element-count charge
// would move in lockstep with the match count; it does not.
func TestOperationCount_RegexFindallSplitDoNotChargeElements(t *testing.T) {
	subj200 := strings.Repeat("a1", 100) // 200 bytes, 100 matches of [0-9]
	subj800 := strings.Repeat("a1", 400) // 800 bytes, 400 matches of [0-9]
	if got := opsFor(t, fmt.Sprintf("re_findall('%s', '[0-9]')", subj200)); got != 2 {
		t.Errorf("ops(re_findall, 100 matches/200B) = %d; want 2 (1+ceil(200/256)=2), not 101 (1+100 matches)", got)
	}
	if got := opsFor(t, fmt.Sprintf("re_findall('%s', '[0-9]')", subj800)); got != 5 {
		t.Errorf("ops(re_findall, 400 matches/800B) = %d; want 5 (1+ceil(800/256)=4), not 401 (1+400 matches)", got)
	}
}

// TestOperationCount_ReprShListChargesElementsNotBytes pins repr_sh's list
// rows on ArgElements alone: growing each element from 10 to 300 bytes at a
// fixed element count (5) must not move the charge, but growing the element
// count (5 to 20) must.
func TestOperationCount_ReprShListChargesElementsNotBytes(t *testing.T) {
	elem10 := strings.Repeat("a", 10)
	elem300 := strings.Repeat("a", 300)
	lst := func(elem string, n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = "'" + elem + "'"
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	small := opsFor(t, "repr_sh("+lst(elem10, 5)+")")
	big := opsFor(t, "repr_sh("+lst(elem300, 5)+")")
	if small != 6 || big != 6 {
		t.Errorf("ops = %d, %d; want 6, 6 -- growing each of 5 elements from 10 to 300 bytes must not move the count (ArgElements only, no bytes)", small, big)
	}
	if got := opsFor(t, "repr_sh("+lst(elem10, 20)+")"); got != 21 {
		t.Errorf("ops(20 elements) = %d; want 21 (1 call + 20 elements)", got)
	}
}

// TestOperationCount_ReprFunctionsListChargesElements checks that repr_py,
// repr_json, repr_pwsh and repr_cmd charge ArgElements over a list, as rule 2,
// which names all five repr_* functions, requires. The reference also adds a
// unit for the string it builds, which sqi does not; see
// test/oracle/baseline-ops.txt.
func TestOperationCount_ReprFunctionsListChargesElements(t *testing.T) {
	lst5 := "['a','a','a','a','a']"
	lst20 := "[" + strings.Repeat("'a',", 19) + "'a']"
	for _, fn := range []string{"repr_py", "repr_json", "repr_pwsh", "repr_cmd"} {
		t.Run(fn, func(t *testing.T) {
			if got := opsFor(t, fn+"("+lst5+")"); got != 6 {
				t.Errorf("ops(%s, 5 elems) = %d; want 6 (1 call + 5 elements)", fn, got)
			}
			if got := opsFor(t, fn+"("+lst20+")"); got != 21 {
				t.Errorf("ops(%s, 20 elems) = %d; want 21", fn, got)
			}
		})
	}
}

// TestOperationCount_ReprFunctionsRangeExprAndScalarsChargeNothing pins the
// other side of the same three functions' Cost: a range_expr or a bare
// int/float/bool/null argument charges nothing beyond rule 1, matching the
// reference exactly (rule 3 covers only "a string or path value", and a
// range_expr is neither).
func TestOperationCount_ReprFunctionsRangeExprAndScalarsChargeNothing(t *testing.T) {
	bigRange := "range_expr(\"" + bigNonOverlappingRangeText() + "\")"
	for _, fn := range []string{"repr_py", "repr_json", "repr_pwsh"} {
		if got := opsFor(t, fn+"("+bigRange+")"); got != 2 {
			t.Errorf("ops(%s(range_expr, 4444-byte text)) = %d; want 2 (1 [range_expr()] + 1 [%s]) -- must not scale with the range text's length", fn, got, fn)
		}
	}
	if got := opsFor(t, "repr_py(5)"); got != 1 {
		t.Errorf("ops(repr_py(int)) = %d; want 1", got)
	}
	if got := opsFor(t, "repr_json(true)"); got != 1 {
		t.Errorf("ops(repr_json(bool)) = %d; want 1", got)
	}
}

// bigNonOverlappingRangeText builds a 4444-byte range_expr text (1000
// non-overlapping odd single values, comma-separated) by host-language
// construction, not the expression
// language's own repetition operator, so no operator charge is baked into a
// caller's reading of the number this produces.
func bigNonOverlappingRangeText() string {
	var b strings.Builder
	for n := 1; n < 2000; n += 2 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d", n)
	}
	return b.String()
}

// TestOperationCount_MathScalarRowsChargeNothing pins abs/floor/ceil/round's
// scalar rows at rule 1 only, isolated from unary minus's own separate
// charge: "-1" alone already costs 1 (the operator's own charge, confirmed
// here only to show abs's own share is exactly 1, not 2).
func TestOperationCount_MathScalarRowsChargeNothing(t *testing.T) {
	neg := opsFor(t, "-1")
	absNeg := opsFor(t, "abs(-1)")
	if neg != 1 {
		t.Fatalf("ops(-1) = %d; want 1 (unary minus's own charge)", neg)
	}
	if absNeg-neg != 1 {
		t.Errorf("abs's own share = %d; want 1 (abs(-1) totals %d, unary minus already accounts for %d of it)", absNeg-neg, absNeg, neg)
	}
	for _, src := range []string{"floor(1.5)", "floor(1)", "ceil(1.5)", "ceil(1)", "round(1.5)", "round(2.0,0)", "round(2,1)"} {
		if got := opsFor(t, src); got != 1 {
			t.Errorf("ops(%s) = %d; want 1", src, got)
		}
	}
}

// TestOperationCount_RoundNonzeroNdigitsDivergesFromReference is round's own
// divergence test. The reference charges ceil(ndigits/256) for a positive
// ndigits (round(2.0,1) costs 2 there, round(2.0,300) 3, round(2.0,600) 4) and
// stays flat for ndigits <= 0 regardless of magnitude. sqi does not reproduce
// the positive-ndigits scaling: it tracks a rendered presentation string
// (Value.fs, value.go) that Cost's ArgBytes/ResultBytes never read (neither
// index the rendered text lives on nor rule 3's own text -- "a string or path
// value" -- names an int argument like ndigits), so round's (float,int) row
// stays Cost{} and sqi's own count for round(2.0,1) is 1, not the reference's
// 2. See funcsmath.go's "round" cost comment for the full reasoning.
func TestOperationCount_RoundNonzeroNdigitsDivergesFromReference(t *testing.T) {
	if got := opsFor(t, "round(2.0,1)"); got != 1 {
		t.Errorf("ops(round(2.0,1)) = %d; want 1 (rule 1 only) -- the reference measures 2 here, not reproduced", got)
	}
	// 300 keeps math.Pow(10, ndigits) within float64's range and still
	// discriminates (the reference charges 3 here).
	if got := opsFor(t, "round(2.0,300)"); got != 1 {
		t.Errorf("ops(round(2.0,300)) = %d; want 1 -- the reference measures 3 here (ceil(300/256)=2 added), not reproduced", got)
	}
	// want 2, not 1: "-600" is itself a unary-minus operation (its own
	// charge, 1 op, isolated the same way TestOperationCount_MathScalarRowsChargeNothing
	// isolates abs(-1)'s), so round's own share is still 1 (2 total - 1 for the
	// negation) -- matching the positive-ndigits rows above, where no negation
	// is present to subtract.
	if got := opsFor(t, "round(2.0,-600)"); got != 2 {
		t.Errorf("ops(round(2.0,-600)) = %d; want 2 (1 for unary minus on 600 + round's own 1) -- the reference ALSO does not scale with ndigits here (flat at 2), confirming the reference's own charge is specific to POSITIVE ndigits", got)
	}
}

// TestOperationCount_MinMaxScalarFormsDivergeFromReference is min/max's own
// divergence test for the fixed-arity (non-list) rows: the reference charges
// N (the argument count) even for two or three bare scalars with no list
// value anywhere in the call, treating every min/max call as "iterate a slice
// of N values" regardless of where the N came from. sqi does not reproduce
// this: rule 2 requires iterating "every element of A list", and these rows
// take no list-typed parameter at all -- RFC 0006 declares min(T,T) and
// min(T,T,T) as overloads distinct from min(list[T]). See funcsmath.go's
// "min and max together" cost comment.
func TestOperationCount_MinMaxScalarFormsDivergeFromReference(t *testing.T) {
	tests := []struct{ src string }{
		{"min(1,2)"}, {"min(1,2,3)"}, {"max(1.0,2.0)"}, {"max(1.0,2.0,3.0)"},
	}
	for _, tt := range tests {
		if got := opsFor(t, tt.src); got != 1 {
			t.Errorf("ops(%s) = %d; want 1 (rule 1 only) -- the reference charges 1+N here (min(1,2)=3, min(1,2,3)=4, ...), not reproduced: no list value is ever passed", tt.src, got)
		}
	}
}

// TestOperationCount_MinMaxRangeExprChargesItsExpansion pins that min/max's
// range_expr row charges Cost{ArgElements: {0}} -- the range's own element
// count -- because sqi's min and max both call rangeInts(args[0]) and expand
// the range in full, satisfying rule 2's "a function ... iterates through
// every element of a list" outright.
//
// The reference's count does not scale here -- flat at the same total for a
// 5-value and a 100,000-value range, which is baselined -- but the
// reference's behavior is subordinate to the spec text by this package's
// standing rule. The 'in' operator's range_expr row charges the same way.
// Left uncharged, min(Param.R) on a million-element range would charge 1
// operation while expanding a million integers.
func TestOperationCount_MinMaxRangeExprChargesItsExpansion(t *testing.T) {
	// 1 (range_expr call) + 1 (min call) + N (the expansion).
	if got := opsFor(t, `min(range_expr("1-5"))`); got != 7 {
		t.Errorf("ops(min, 5-value range) = %d; want 7 (1 + 1 + 5 elements)", got)
	}
	if got := opsFor(t, `min(range_expr("1-100000"))`); got != 100002 {
		t.Errorf("ops(min, 100000-value range) = %d; want 100002 -- must scale with the range's size", got)
	}
	if got := opsFor(t, `max(range_expr("1-100000"))`); got != 100002 {
		t.Errorf("ops(max, 100000-value range) = %d; want 100002, same reasoning as min", got)
	}
}

// TestOperationCount_SumRangeExprCharges is sum's own range_expr test: unlike
// min/max's range_expr row above, this one scales as the reference does
// (sum(range_expr("1-100")) costs 102 on both sides: range_expr()'s own
// construction charges nothing, sum's own share is 1 [rule 1] + 100
// [ArgElements, via elementCount's rangeExprCount], total 101, plus
// range_expr()'s own 1 = 102).
func TestOperationCount_SumRangeExprCharges(t *testing.T) {
	if got := opsFor(t, `sum(range_expr("1-100"))`); got != 102 {
		t.Errorf("ops(sum, 100-value range) = %d; want 102 (matches the reference exactly)", got)
	}
	if got := opsFor(t, `sum(range_expr("1-1000"))`); got != 1002 {
		t.Errorf("ops(sum, 1000-value range) = %d; want 1002", got)
	}
}

// TestOperationCount_PathConstructionChargesInputBytesNotNormalizedOutput
// pins path(string)'s ArgBytes on the raw input text, not the normalized
// result: a 302-byte input full of redundant separators that normalizes down
// to the 2-byte path "/a" still charges based on 302 bytes, not 2.
func TestOperationCount_PathConstructionChargesInputBytesNotNormalizedOutput(t *testing.T) {
	redundant := "/" + strings.Repeat("/", 300) + "a" // 302 bytes, normalizes to "/a"
	if got := opsFor(t, fmt.Sprintf("path('%s')", redundant)); got != 3 {
		t.Errorf("ops = %d; want 3 (1 call + ceil(302/256)=2) -- a result-bytes reading would give 2 (ceil(2/256)=1)", got)
	}
}

// TestOperationCount_PathListConstructionChargesResultBytes pins path(list[
// string])'s Cost{ResultBytes: true}: element count alone does not move the
// charge (1, 4 and 20-element lists whose joined text stays under 256 bytes
// all cost the same), but the joined text's byte length does.
func TestOperationCount_PathListConstructionChargesResultBytes(t *testing.T) {
	if got := opsFor(t, "path(['/'])"); got != 2 {
		t.Errorf("ops(1-elem) = %d; want 2", got)
	}
	if got := opsFor(t, "path(['/','a','b','c'])"); got != 2 {
		t.Errorf("ops(4-elem) = %d; want 2 -- same as the 1-elem case, both join to well under 256 bytes", got)
	}
	lst20 := "['/'," + strings.Repeat("'p',", 19) + "'p']"
	if got := opsFor(t, "path("+lst20+")"); got != 2 {
		t.Errorf("ops(20-elem, still <256B joined) = %d; want 2 -- element COUNT alone does not move the charge", got)
	}
	big := "['/','" + strings.Repeat("a", 300) + "']"
	if got := opsFor(t, "path("+big+")"); got != 3 {
		t.Errorf("ops(list with one 300-byte element) = %d; want 3 -- the JOINED text (301+ bytes) is what moves the charge", got)
	}
}

// TestOperationCount_PathPropertiesChargeReceiverBytes discriminates the six
// property accesses' own share from path()'s construction charge by
// comparing an 11-byte and a 299-byte receiver, for every property.
func TestOperationCount_PathPropertiesChargeReceiverBytes(t *testing.T) {
	p10 := "/" + strings.Repeat("a", 10)
	p299 := "/" + strings.Repeat("a", 298)
	for _, prop := range []string{"name", "stem", "suffix", "suffixes", "parent", "parts"} {
		t.Run(prop, func(t *testing.T) {
			small := opsFor(t, fmt.Sprintf("path('%s').%s", p10, prop))
			big := opsFor(t, fmt.Sprintf("path('%s').%s", p299, prop))
			if small != 4 {
				t.Errorf("ops(11-byte receiver) = %d; want 4 (path()=2 + property=2)", small)
			}
			if big != 6 {
				t.Errorf("ops(299-byte receiver) = %d; want 6 (path()=3 + property=3)", big)
			}
		})
	}
}

// TestOperationCount_PathWithFunctionsIgnoreReplacementLength pins that
// with_name/with_stem's second (replacement) argument never contributes,
// however large -- only the receiver's bytes do, the same pattern as strip()'s
// cutset and removeprefix()'s affix arguments.
func TestOperationCount_PathWithFunctionsIgnoreReplacementLength(t *testing.T) {
	big := strings.Repeat("x", 300)
	if got := opsFor(t, fmt.Sprintf("path('/a').with_name('%s')", big)); got != 4 {
		t.Errorf("ops(short receiver, 300-byte replacement) = %d; want 4 (path('/a')=2 + with_name=2, ArgBytes(1B)); the replacement's length must not move it", got)
	}
	p299 := "/" + strings.Repeat("a", 298)
	if got := opsFor(t, fmt.Sprintf("path('%s').with_name('x')", p299)); got != 6 {
		t.Errorf("ops(299-byte receiver, short replacement) = %d; want 6 (path()=3 + with_name=3)", got)
	}
}

// TestOperationCount_IsRelativeToChargesBothOperands is is_relative_to's and
// relative_to's own divergence test. Cost{ArgBytes: {0,1}} charges each
// operand's own bytes independently (ceiled and summed by chargeArgs), which
// over-counts relative to the reference's max(ceil0,ceil1) formula, which the
// Cost mechanism cannot express. See funcspath.go's "is_relative_to and
// relative_to" cost comment.
func TestOperationCount_IsRelativeToChargesBothOperands(t *testing.T) {
	p299 := "/" + strings.Repeat("a", 298)
	longRecvShortOther := opsFor(t, fmt.Sprintf("path('%s').is_relative_to(path('/a'))", p299))
	shortRecvLongOther := opsFor(t, fmt.Sprintf("path('/a').is_relative_to(path('%s'))", p299))
	// Both operands are ArgBytes-charged, so swapping which one is long must
	// not change the total (unlike the reference, whose own formula also
	// happens to be symmetric -- but for a different, unreplicable reason).
	if longRecvShortOther != shortRecvLongOther {
		t.Errorf("ops = %d (long receiver), %d (long other); want equal -- both operands are charged, so the total must not depend on which one is long", longRecvShortOther, shortRecvLongOther)
	}
	// path('/a')=2, path(p299)=3; is_relative_to's own share = 1 (call) +
	// ceil(2/256)=1 + ceil(299/256)=2 = 4; total = 2 + 3 + 4 = 9.
	if longRecvShortOther != 9 {
		t.Errorf("ops = %d; want 9 (2 [path('/a')] + 3 [path(p299)] + 4 [is_relative_to: 1 call + 1 + 2])", longRecvShortOther)
	}
}

// TestOperationCount_RelativeToChargesLikeIsRelativeTo pins that relative_to
// adds no further charge for its own constructed path result beyond what
// is_relative_to already carries for the identical operand pair -- confirmed
// by comparing the two directly at the same sizes.
func TestOperationCount_RelativeToChargesLikeIsRelativeTo(t *testing.T) {
	isRel := opsFor(t, "path('/a').is_relative_to(path('/a'))")
	rel := opsFor(t, "path('/a').relative_to(path('/a'))")
	if isRel != rel {
		t.Errorf("ops = %d (is_relative_to), %d (relative_to); want equal", isRel, rel)
	}
}

// TestOperationCount_UnresolvedArgumentChargesRuleOneOnly covers the ruling
// "an unresolved operand charges rule 1 only" for one row of these groups,
// matching the equivalent tests in cost_string_internal_test.go and
// cost_list_internal_test.go. callFunction (call.go) short-circuits before
// callShape/chargeArgs ever run when any argument is unresolved, so this
// holds structurally for every row in these groups; pinned here rather than
// assumed.
func TestOperationCount_UnresolvedArgumentChargesRuleOneOnly(t *testing.T) {
	ec := testCtx()
	if _, err := callFunction(ec, "re_match", []Value{Unresolved(TString), Unresolved(TString)}, false); err != nil {
		t.Fatalf("callFunction(re_match, unresolved): %v", err)
	}
	if ec.m.ops != 1 {
		t.Errorf("ops = %d; want 1 (rule 1 only, no ArgBytes charge for an unresolved operand)", ec.m.ops)
	}
}
