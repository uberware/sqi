// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"fmt"
	"strings"
	"testing"
)

// TestOperationCount_StringFunctions pins section 1.3.10's charges for the
// string library. A long string in a case below, written as 'a'*300, is
// spliced into the source as a literal: evaluated by the expression language's
// own "*" operator it would add that operator's charge (('a'*300).upper()
// costs 6 that way, not 3). funcsstrsplit.go names the same trap for join().
//
//   - Case transforms (upper/lower/capitalize/title) charge ArgBytes on the
//     input: 10/300/600 bytes cost 2/3/4 (1 call + ceil(n/256)). Not
//     ResultBytes: ('İ'*100).lower() reads 200 bytes, builds 300, and costs 2.
//   - The classification predicates (isdigit, isalpha, isalnum, isspace,
//     isupper, islower, isascii) scan the whole string (RFC 0006's "all
//     characters are X"), so rule 3's "and similar" covers them; len() of the
//     same 300-byte input stays at 1.
//   - strip/lstrip/rstrip, removeprefix/removesuffix, startswith/endswith,
//     count/find/rfind/index/rindex and replace charge ArgBytes on the main
//     string only. The cutset, affix, needle and replacement never add to it,
//     however long, and neither does the length of replace's result.
//   - split/rsplit charge ArgBytes on the main string, not per produced
//     element: rule 2's enumeration (shape.go's specNamedIteratingFunctions)
//     does not name split.
//   - join charges Cost{ArgElements: {0}, ResultBytes: true}: rule 2 for the
//     list it iterates and rule 3 for the string it builds. ResultBytes
//     because ArgBytes reads one argument's own payload (chargeArgs, ops.go)
//     and args[0] is the list. See
//     TestOperationCount_JoinChargesElementsAndBytesTogether.
//   - ljust/rjust/center and all three zfill rows charge ResultBytes on the
//     padded string: a 10-byte input costs 2/3/4 at widths 20/300/600. A
//     width at or below the input's length, negative included, is a no-op
//     (padWidth, funcsstrpad.go). See
//     TestOperationCount_ZfillDivergesFromReferenceOnWidth.
func TestOperationCount_StringFunctions(t *testing.T) {
	a300 := strings.Repeat("a", 300)
	tests := []struct {
		name string
		src  string
		want int64
		why  string
	}{
		// --- funcsstrcase.go: case transforms, ArgBytes{0} on the input ---
		{"upper", fmt.Sprintf("'%s'.upper()", a300), 3, "rule 3 names upper() by name; ArgBytes on the input, scaling 10/300/600 -> 2/3/4"},
		{"lower", fmt.Sprintf("'%s'.lower()", a300), 3, "rule 3 names lower() by name; ArgBytes on the input, unlike '+' (see the 'İ' case in this file's doc comment)"},
		{"capitalize", fmt.Sprintf("'%s'.capitalize()", a300), 3, "not named by rule 3's own enumeration but does the same length-proportional work as upper/lower; 'and similar' covers it"},
		{"title", fmt.Sprintf("'%s'.title()", a300), 3, "same as capitalize: 'and similar', ArgBytes on the input"},

		// --- funcsstrcase.go: classification predicates ---
		// These SCAN THE WHOLE STRING (RFC 0006's
		// "all characters are X and string is non-empty"), so they are rule-3
		// charges under "and similar", not len()-style exempt lookups. len()
		// itself stays flat at 1 on the identical 300-byte input -- see
		// TestOperationCount_ClassificationFunctionsAreNotLenExempt for the
		// side-by-side control.
		{"isdigit", fmt.Sprintf("'%s'.isdigit()", strings.Repeat("1", 300)), 3, "scans the whole string (RFC 0006's 'all characters are digits'); rule 3's 'and similar', not len()'s exemption"},
		{"isalpha", fmt.Sprintf("'%s'.isalpha()", a300), 3, "same reasoning as isdigit"},
		{"isalnum", fmt.Sprintf("'%s'.isalnum()", a300), 3, "same reasoning as isdigit"},
		{"isspace", fmt.Sprintf("'%s'.isspace()", strings.Repeat(" ", 300)), 3, "same reasoning as isdigit"},
		{"isupper", fmt.Sprintf("'%s'.isupper()", strings.Repeat("A", 300)), 3, "same reasoning as isdigit"},
		{"islower", fmt.Sprintf("'%s'.islower()", a300), 3, "same reasoning as isdigit"},
		{"isascii", fmt.Sprintf("'%s'.isascii()", a300), 3, "same reasoning as isdigit"},

		// --- funcsstrfind.go: trim/affix/search/replace, ArgBytes{0} on the
		// receiver only; the second (and third) string argument's own length
		// never contributes (see the strip-cutset and removeprefix/removesuffix
		// tests below). ---
		{"strip", fmt.Sprintf("'x%sx'.strip('x')", strings.Repeat("a", 298)), 3, "ArgBytes on the 300-byte main string; the cutset argument's length does not matter (see the dedicated cutset-independence test)"},
		{"lstrip", fmt.Sprintf("'x%sx'.lstrip('x')", strings.Repeat("a", 298)), 3, "same as strip"},
		{"rstrip", fmt.Sprintf("'x%sx'.rstrip('x')", strings.Repeat("a", 298)), 3, "same as strip"},
		{"removeprefix", fmt.Sprintf("'%s'.removeprefix('a')", a300), 3, "ArgBytes on the main string; independent of the prefix argument's length (see the dedicated affix-independence test)"},
		{"removesuffix", fmt.Sprintf("'%s'.removesuffix('a')", a300), 3, "same as removeprefix"},
		{"startswith", fmt.Sprintf("'%s'.startswith('a')", a300), 3, "ArgBytes on the receiver, even though a real startswith only needs to look at the prefix -- the reference charges the full receiver length regardless"},
		{"endswith", fmt.Sprintf("'%s'.endswith('a')", a300), 3, "same as startswith"},
		{"count", fmt.Sprintf("'%s'.count('a')", a300), 3, "ArgBytes on the haystack; the needle's own length does not matter at all (see the dedicated needle-independence test)"},
		{"find", fmt.Sprintf("'%sb'.find('b')", a300), 3, "same as count"},
		{"rfind", fmt.Sprintf("'%sb'.rfind('b')", a300), 3, "same as count"},
		{"index", fmt.Sprintf("'%sb'.index('b')", a300), 3, "same as count, index/rindex differ from find/rfind only in what a miss does"},
		{"rindex", fmt.Sprintf("'%sb'.rindex('b')", a300), 3, "same as index"},
		{"replace", fmt.Sprintf("'%s'.replace('a','b')", a300), 3, "ArgBytes on the main string only -- confirmed NOT to charge the produced result even when old/new make it balloon (see the dedicated growth-independence test)"},

		// --- funcsstrsplit.go: split/rsplit, ArgBytes{0}; join, its own row below ---
		{"split", fmt.Sprintf("'%s'.split(',')", strings.Repeat("a,", 149)+"a"), 3, "ArgBytes on the main string; NOT ArgElements/ResultElements despite producing a list (rule 2's specNamedIteratingFunctions does not name split -- see the dedicated word-count-independence test)"},
		{"rsplit", fmt.Sprintf("'%s'.rsplit(',')", strings.Repeat("a,", 149)+"a"), 3, "same as split"},
		{"join", "['a','b'].join(',')", 4, "own dedicated test below (rules 2 AND 3 together, a deliberate divergence) -- 1 call + 2 elements + ceil(len(\"a,b\")=3 /256)=1 result byte"},

		// --- funcsstrpad.go: ljust/rjust/center/zfill, ResultBytes ---
		{"ljust", fmt.Sprintf("'%s'.ljust(600)", strings.Repeat("a", 10)), 4, "ResultBytes (the produced, padded string), not ArgBytes on the 10-byte input -- see the dedicated input-vs-width discrimination test"},
		{"rjust", fmt.Sprintf("'%s'.rjust(600)", strings.Repeat("a", 10)), 4, "same as ljust"},
		{"center", fmt.Sprintf("'%s'.center(600)", strings.Repeat("a", 10)), 4, "same as ljust"},
		{"zfill", "'5'.zfill(600)", 4, "sqi diverges from the reference here (own dedicated test below): ResultBytes on the padded width, matching its three siblings, even though the reference's own zfill measurement never scales with width at all"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := opsFor(t, tt.src); got != tt.want {
				t.Errorf("ops(%s(...)) = %d; want %d (%s)", tt.name, got, tt.want, tt.why)
			}
		})
	}
}

// TestOperationCount_StringFunctionsCoversEveryFunctionName pins that the
// table above has exactly one row per one of the 31 RFC 0006 string function
// names (11 in funcsstrcase.go + 13 in funcsstrfind.go + 3 in
// funcsstrsplit.go + 4 in funcsstrpad.go), so a future addition or removal in
// those four files is caught here rather than silently under-covered.
func TestOperationCount_StringFunctionsCoversEveryFunctionName(t *testing.T) {
	names := []string{
		"upper", "lower", "capitalize", "title",
		"isdigit", "isalpha", "isalnum", "isspace", "isupper", "islower", "isascii",
		"strip", "lstrip", "rstrip", "removeprefix", "removesuffix",
		"startswith", "endswith", "count", "find", "rfind", "index", "rindex", "replace",
		"split", "rsplit", "join",
		"ljust", "rjust", "center", "zfill",
	}
	if len(names) != 31 {
		t.Fatalf("RFC 0006 has 31 string function names; the coverage list has %d", len(names))
	}
	covered := map[string]bool{
		"upper": true, "lower": true, "capitalize": true, "title": true,
		"isdigit": true, "isalpha": true, "isalnum": true, "isspace": true, "isupper": true, "islower": true, "isascii": true,
		"strip": true, "lstrip": true, "rstrip": true, "removeprefix": true, "removesuffix": true,
		"startswith": true, "endswith": true, "count": true, "find": true, "rfind": true, "index": true, "rindex": true, "replace": true,
		"split": true, "rsplit": true, "join": true,
		"ljust": true, "rjust": true, "center": true, "zfill": true,
	}
	for _, n := range names {
		if !covered[n] {
			t.Errorf("function %q has no row in TestOperationCount_StringFunctions", n)
		}
		if _, ok := functionShapes[n]; !ok {
			t.Errorf("function %q is not registered in functionShapes", n)
		}
	}
}

// TestOperationCount_ClassificationFunctionsAreNotLenExempt is the explicit
// side-by-side: the 11 classification/case functions scan
// the whole string and are rule-3 charges, unlike len(), which stays flat at
// 1 on the identical input no matter its size.
func TestOperationCount_ClassificationFunctionsAreNotLenExempt(t *testing.T) {
	big := strings.Repeat("a", 300)
	if got := opsFor(t, fmt.Sprintf("len('%s')", big)); got != 1 {
		t.Errorf("ops(len(<300 bytes>)) = %d; want 1 -- len() is section 1.3.10's own named exemption", got)
	}
	if got := opsFor(t, fmt.Sprintf("'%s'.isalpha()", big)); got != 3 {
		t.Errorf("ops(isalpha(<300 bytes>)) = %d; want 3 -- isalpha scans the whole string, it is not exempt like len()", got)
	}
}

// TestOperationCount_StripCutsetDoesNotAffectCost pins that strip/lstrip/
// rstrip's second (cutset) argument never contributes to the charge, no
// matter how large: only the receiver's byte length (ArgBytes{0}) is
// declared, and chargeArgs only reads the indices a Cost actually names.
func TestOperationCount_StripCutsetDoesNotAffectCost(t *testing.T) {
	small := opsFor(t, "'xaaaaaaaaaax'.strip('xyxyxyxyxy')")
	big := opsFor(t, fmt.Sprintf("'xaaaaaaaaaax'.strip('%s')", strings.Repeat("xy", 150)))
	if small != 2 || big != 2 {
		t.Errorf("ops = %d, %d; want 2, 2 -- growing the cutset from 10 to 300 bytes must not move the count", small, big)
	}
}

// TestOperationCount_AffixArgumentDoesNotAffectRemoveprefixCost pins the same
// independence for removeprefix/removesuffix: the charge tracks the main
// string's total length, not which side (the matched affix or the remaining
// text) is long. If the reference charged the result instead, these two would
// differ (results are 10 and 300 bytes respectively); they do not.
func TestOperationCount_AffixArgumentDoesNotAffectRemoveprefixCost(t *testing.T) {
	longAffixShortRemainder := opsFor(t, fmt.Sprintf("'%s%s'.removeprefix('%s')",
		strings.Repeat("a", 300), strings.Repeat("b", 10), strings.Repeat("a", 300)))
	shortAffixLongRemainder := opsFor(t, fmt.Sprintf("'%s%s'.removeprefix('%s')",
		strings.Repeat("a", 10), strings.Repeat("b", 300), strings.Repeat("a", 10)))
	if longAffixShortRemainder != 3 || shortAffixLongRemainder != 3 {
		t.Errorf("ops = %d, %d; want 3, 3 -- both total 310 bytes of MAIN string, regardless of which side is long", longAffixShortRemainder, shortAffixLongRemainder)
	}
}

// TestOperationCount_NeedleLengthDoesNotAffectFindCost pins count/find's
// independence from the needle's own length: a 300-byte, entirely absent
// needle against a 2-byte haystack still charges only the haystack.
func TestOperationCount_NeedleLengthDoesNotAffectFindCost(t *testing.T) {
	longNeedle := strings.Repeat("c", 300)
	if got := opsFor(t, fmt.Sprintf("'ab'.count('%s')", longNeedle)); got != 2 {
		t.Errorf("ops(count) = %d; want 2 -- a 2-byte haystack charges 1 call + ceil(2/256)=1, regardless of the 300-byte (unmatched) needle", got)
	}
	if got := opsFor(t, fmt.Sprintf("'ab'.find('%s')", longNeedle)); got != 2 {
		t.Errorf("ops(find) = %d; want 2 -- same reasoning as count", got)
	}
}

// TestOperationCount_ReplaceDoesNotChargeTheProducedResult pins a trap
// directly: replace() can balloon a small input into a huge
// result (old shorter than new, many occurrences), and the reference's own
// count does not move when that happens -- it tracks the main string's own
// length, never the result actually built. sqi follows the reference here
// (unlike join/zfill below): rule 3 groups replace() in the same sentence as
// upper()/lower()/strip(), which are all input-driven (see the 'İ' case at
// the top of this file), and the worst-case growth
// this exposes is independently bounded by maxStringBytes (checkRepeat in
// replaceAll, funcsstrfind.go) regardless of what the operation count says --
// a separate limit (section 1.3.9) from this one (section 1.3.10).
func TestOperationCount_ReplaceDoesNotChargeTheProducedResult(t *testing.T) {
	// 100 'a's, each replaced by a 300-byte string: the result is ~30000
	// bytes, but the charge tracks only the 100-byte input.
	got := opsFor(t, fmt.Sprintf("'%s'.replace('a','%s')", strings.Repeat("a", 100), strings.Repeat("b", 300)))
	if got != 2 {
		t.Errorf("ops(replace, ballooning result) = %d; want 2 (1 call + ceil(100/256)=1) -- the ~30000-byte result must not move the count", got)
	}
}

// TestOperationCount_SplitDoesNotChargeByWordCount pins that split/rsplit are
// ArgBytes-only, not ArgElements/ResultElements: a 5-word and a 150-word
// whitespace split measure what their byte lengths predict, not their word
// counts (which would give 6 and 151 respectively under an ArgElements/
// ResultElements reading).
func TestOperationCount_SplitDoesNotChargeByWordCount(t *testing.T) {
	five := opsFor(t, fmt.Sprintf("'%s'.split()", strings.Repeat("a ", 5)))
	oneFifty := opsFor(t, fmt.Sprintf("'%s'.split()", strings.Repeat("a ", 150)))
	if five != 2 {
		t.Errorf("ops(5-word split) = %d; want 2 (1 call + ceil(10/256)=1), not 6 (1 call + 5 words)", five)
	}
	if oneFifty != 3 {
		t.Errorf("ops(150-word split) = %d; want 3 (1 call + ceil(300/256)=2), not 151 (1 call + 150 words)", oneFifty)
	}
}

// TestOperationCount_JoinChargesElementsAndBytesTogether is join's own
// dedicated test: it is named by both rule 2 (iterates a list)
// and rule 3 (processes strings), and is deliberately charged
// Cost{ArgElements: {0}, ResultBytes: true}. Discriminated by holding the
// element count fixed and growing each element's byte length: an
// ArgElements-only reading would predict the same total for both rows below;
// it does not, once sqi's own ResultBytes charge is added.
func TestOperationCount_JoinChargesElementsAndBytesTogether(t *testing.T) {
	small := opsFor(t, "join(['a','a','a','a','a'], ',')")
	// 5 elements, each 300 bytes: produced string is 5*300 + 4*1(seps) =
	// 1504 bytes, ceil(1504/256) = 6. ArgElements alone would give 6 for both
	// rows; sqi's ResultBytes addition is
	// what makes them differ.
	elem := strings.Repeat("a", 300)
	src := fmt.Sprintf("join(['%s','%s','%s','%s','%s'], ',')", elem, elem, elem, elem, elem)
	big := opsFor(t, src)
	if small != 7 {
		t.Errorf("ops(join, 5x 1-byte elements) = %d; want 7 (1 call + 5 elements + ceil(len(\"a,a,a,a,a\")=9 /256)=1 result byte)", small)
	}
	if big != 12 {
		t.Errorf("ops(join, 5x 300-byte elements) = %d; want 12 (1 call + 5 elements + ceil(1504/256)=6 result bytes)", big)
	}
	if small == big {
		t.Errorf("join's charge did not scale with string content at all (both %d) -- sqi's ResultBytes divergence from the reference did not take effect", small)
	}
}

// TestOperationCount_JoinListNullRowChargesNothing pins the list[nulltype]
// row's Cost{}: an empty list is empty by TYPE, matching flatten's and any/
// all's list[nulltype] rows (funcslist.go) -- there is nothing to
// iterate or produce regardless of Cost, so it is declared as an explicit
// zero rather than a copy of the list[string] row's Cost.
func TestOperationCount_JoinListNullRowChargesNothing(t *testing.T) {
	if got := opsFor(t, "join([], ',')"); got != 1 {
		t.Errorf("ops(join([], ',')) = %d; want 1 (rule 1 only)", got)
	}
}

// TestOperationCount_PaddingFunctionsChargeProducedBytes pins ljust/rjust/
// center's ResultBytes over ArgBytes, discriminated by holding the 10-byte
// input fixed and growing only the requested width -- an ArgBytes(input)
// reading would stay flat at 2 for every row; it does not.
func TestOperationCount_PaddingFunctionsChargeProducedBytes(t *testing.T) {
	small := strings.Repeat("a", 10)
	for _, fn := range []string{"ljust", "rjust", "center"} {
		t.Run(fn, func(t *testing.T) {
			at20 := opsFor(t, fmt.Sprintf("'%s'.%s(20)", small, fn))
			at300 := opsFor(t, fmt.Sprintf("'%s'.%s(300)", small, fn))
			at600 := opsFor(t, fmt.Sprintf("'%s'.%s(600)", small, fn))
			if at20 != 2 {
				t.Errorf("ops(%s(width=20)) = %d; want 2", fn, at20)
			}
			if at300 != 3 {
				t.Errorf("ops(%s(width=300)) = %d; want 3 -- ArgBytes(input=10) would stay at 2", fn, at300)
			}
			if at600 != 4 {
				t.Errorf("ops(%s(width=600)) = %d; want 4", fn, at600)
			}
		})
	}
}

// TestOperationCount_ZfillDivergesFromReferenceOnWidth is zfill's own
// dedicated divergence test. The reference charges padding as two length
// terms, the argument's and the added padding's; sqi charges
// Cost{ResultBytes: true} on all three zfill rows, matching its three
// siblings (ljust/rjust/center, which share the same padWidth mechanism,
// funcsstrpad.go).
func TestOperationCount_ZfillDivergesFromReferenceOnWidth(t *testing.T) {
	small := opsFor(t, "'5'.zfill(10)")
	big := opsFor(t, "'5'.zfill(600)")
	if small != 2 {
		t.Errorf("ops(zfill(width=10)) = %d; want 2", small)
	}
	if big != 4 {
		t.Errorf("ops(zfill(width=600)) = %d; want 4 (1 call + ceil(600/256)=3) -- the reference measures 2 here (flat, no width scaling)", big)
	}
	// int and float first-argument rows charge identically -- same
	// zfillString call, same Cost declared on all three Shape rows.
	if got := opsFor(t, "(5).zfill(600)"); got != big {
		t.Errorf("ops((5).zfill(600)) = %d; want the same as the string row's %d", got, big)
	}
	if got := opsFor(t, "(5.0).zfill(600)"); got != big {
		t.Errorf("ops((5.0).zfill(600)) = %d; want the same as the string row's %d", got, big)
	}
}

// TestOperationCount_UnresolvedStringArgumentChargesRuleOneOnly covers the
// ruling "an unresolved operand charges rule 1 only -- no element or byte
// charges, since no values were processed." callFunction (call.go)
// short-circuits before callShape/chargeArgs ever runs when any argument is
// unresolved, so this holds structurally for every row in this file; pinned
// here for one of the string-function ArgBytes rows rather than assumed.
func TestOperationCount_UnresolvedStringArgumentChargesRuleOneOnly(t *testing.T) {
	ec := testCtx()
	if _, err := callFunction(ec, "upper", []Value{Unresolved(TString)}, false); err != nil {
		t.Fatalf("callFunction(upper, unresolved): %v", err)
	}
	if ec.m.ops != 1 {
		t.Errorf("ops = %d; want 1 (rule 1 only, no ArgBytes charge for an unresolved operand)", ec.m.ops)
	}
}
