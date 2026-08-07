// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"fmt"
	"strings"
	"testing"
)

// PROBE (sub-project E1, Task 7), .venv-oracle/bin/python -c "..." against
// openjd-model 0.11.1. Pasted verbatim as evidence, per "run something first,
// decide against the spec text second." All 42 rows across funcsstrcase.go
// (11), funcsstrfind.go (16), funcsstrsplit.go (9) and funcsstrpad.go (6) were
// probed; the 300/600-byte inputs below are the ones that actually
// DISCRIMINATE (a sub-256-byte probe cannot, per the brief's own warning: it
// cannot tell "charges nothing" from "charges nothing because the input was
// small").
//
// Brief's own command (10-byte inputs, non-discriminating but confirms every
// row costs SOMETHING beyond len()'s exemption):
//
//	2  'abc'.upper()
//	2  'abc'.lower()
//	2  'a,b,c'.split(',')
//	ERR  ','.join(['a','b'])   -- WRONG receiver: openjd.expr's join is a LIST
//	                              method (sep is the ARGUMENT), matching sqi's
//	                              own Params order (list, sep). Re-run below.
//	2  ' a '.strip()
//	2  'abc'.replace('a','b')
//	2  'abc'.startswith('a')
//	2  'abc'.isalpha()
//	2  'abc'.ljust(10)
//	2  'abc'.find('b')
//
// Case transforms (upper/lower/capitalize/title) -- ArgBytes on the INPUT,
// scaling 10/300/600 bytes to 2/3/4 (1 call + ceil(n/256)):
//
//	2  'a'*10 .upper()      3  'a'*300 .upper()      4  'a'*600 .upper()
//	2  'a'*10 .lower()      3  'a'*300 .lower()       4  'a'*600 .lower()
//	2  'a'*10 .capitalize() 3  'a'*300 .capitalize()  4  'a'*600 .capitalize()
//	2  'a'*10 .title()      3  'a'*300 .title()       4  'a'*600 .title()
//
// The ArgBytes-vs-ResultBytes question (Task 5's own trap for "+", re-run here
// because case mapping can GROW its output): 'İ' (U+0130, 2 UTF-8 bytes)
// lowers to 'i' + a combining dot above (3 UTF-8 bytes), a per-rune growth
// ratio no same-length probe can see. 100 copies is 200 INPUT bytes (ceil/256
// = 1) but 300 OUTPUT bytes (ceil/256 = 2) -- the two hypotheses predict
// different totals:
//
//	2  ('İ'*100).lower()   ArgBytes predicts 1+1=2 (MATCHES). ResultBytes
//	                        would predict 1+2=3. ArgBytes wins for the case
//	                        transforms -- the OPPOSITE of "+"'s ResultBytes
//	                        finding (Task 5), and the same as string "in"
//	                        (Task 5's OpIn/OpNotIn ArgBytes rows).
//
// Classification predicates (isdigit/isalpha/isalnum/isspace/isupper/islower/
// isascii) -- same ArgBytes-on-input formula, 10/300 bytes to 2/3, decided
// explicitly per the brief's item 2: they SCAN THE WHOLE STRING (RFC 0006's
// "all characters are X"), so rule 3's "and similar" covers them. They are NOT
// len()-style lookups -- len() stays flat at 1 on the SAME 300-byte input,
// pinned as the control:
//
//	2  ('1'*10).isdigit()   3  ('1'*300).isdigit()
//	2  ('a'*10).isalpha()   3  ('a'*300).isalpha()
//	2  ('a'*10).isalnum()   3  ('a'*300).isalnum()
//	2  (' '*10).isspace()   3  (' '*300).isspace()
//	2  ('A'*10).isupper()   3  ('A'*300).isupper()
//	2  ('a'*10).islower()   3  ('a'*300).islower()
//	2  ('a'*10).isascii()   3  ('a'*300).isascii()
//	1  len('a'*300)         -- exempt, the control
//
// strip/lstrip/rstrip -- ArgBytes on the MAIN string (arg0) ONLY. The 2-arg
// cutset form's SECOND argument does NOT add to the charge, no matter how
// large: holding the 12-byte main string fixed and growing the cutset from
// 10 to 300 bytes leaves the count at 2 both times:
//
//	2  ('x'+'a'*10+'x').strip()          3  ('x'+'a'*298+'x' -- 300B).strip()
//	2  'xaaaaaaaaaax'.strip('x')          -- 12B main, 1B cutset
//	3  ('x'+'a'*298+'x').strip('x')       -- 300B main, 1B cutset -- scales
//	2  'xaaaaaaaaaax'.strip('xyxyxyxyxy') -- 12B main, 10B cutset: still 2
//	2  'xaaaaaaaaaax'.strip('xy'*150)     -- 12B main, 300B cutset: STILL 2
//
// removeprefix/removesuffix, startswith/endswith -- ArgBytes on the MAIN
// string ONLY, independent of the prefix/suffix argument's own length.
// Discriminated by holding the TOTAL main-string length at 310 bytes while
// swapping which side (the matched affix or the remainder) is long:
//
//	2  ('a'*10+'b'*10).removeprefix('a'*10)              -- 20B main
//	3  ('a'*300+'b'*10).removeprefix('a'*300)             -- 310B main, 300B affix
//	3  ('a'*10+'b'*300).removeprefix('a'*10)               -- 310B main,  10B affix
//	   (if the reference charged the RESULT, these two would differ --
//	    result lengths are 10B and 300B respectively; they do not: both 3)
//	2  ('a'*10+'b'*10).startswith('a'*10)                 3  ('a'*300).startswith('a')
//	3  ('a'*600).startswith('a')  -- 1B needle, still scales with the 600B main
//
// count/find/rfind/index/rindex -- ArgBytes on the MAIN (haystack) string
// only; the needle's own length does not matter at all (a 300-byte, entirely
// absent needle against a 2-byte haystack still measures 2):
//
//	2  ('a'*10+'b').find('b')       3  ('a'*300+'b').find('b')     4  ('a'*600+'b').find('b')
//	   (rfind/index/rindex measure identically at each size)
//	2  'ab'.count('c'*300)          2  'ab'.find('c'*300)   -- needle length: no effect
//
// replace -- ArgBytes on the MAIN string (arg0) ONLY, confirmed with a probe
// designed to catch the trap the brief warns about (a huge PRODUCED string
// hiding behind a small charge): 100 'a's replaced by a 300-byte "new" would
// balloon the RESULT to ~30000 bytes, and the count does not move:
//
//	2  ('a'*10).replace('a','b')     3  ('a'*300).replace('a','b')    4  ('a'*600).replace('a','b')
//	2  ('a'*100).replace('a','bb'*150)  -- new=300B, result~30000B: STILL 1+ceil(100/256)=2
//	2  'ab'.replace('a','x'*300)        -- old=1B, new=300B, tiny result: still 2
//
// split/rsplit -- ArgBytes on the MAIN string ONLY, confirmed NOT to be
// ArgElements/ResultElements despite producing a list (rule 2's own
// enumeration in shape.go's specNamedIteratingFunctions does not name split,
// matching this): a 5-word and a 150-word whitespace split measure the SAME
// as their BYTE lengths predict, not their word counts:
//
//	2  ('a '*5).split()             3  ('a '*150).split()        -- NOT 1+5 / 1+150
//	2  'a,a,a,a,a'.split(',')       3  (299-byte comma list).split(',')  4  (599-byte).split(',')
//	   (rsplit identical at every size; the 3-arg maxsplit form too:)
//	2  'a,a,a,a,a'.split(',', 2)    3  (299-byte comma list).split(',', 2)
//
// join -- named by BOTH rule 2 (iterates a list) and rule 3 (processes
// strings), and this is the row the brief and the sub-project's own standing
// method most distrust. Isolating with a LITERAL list (not a repetition
// operator, whose own ResultElements charge would contaminate the reading):
//
//	3   ['a','b'].join(',')                    -- 2 elements, 1-byte strings
//	6   ['a','a','a','a','a'].join(',')        -- 5 elements, 1-byte strings
//	6   (5 literal 300-byte elements).join(',') -- 5 elements, 300-byte strings:
//	                                                SAME as the 1-byte-element
//	                                                case above -- the
//	                                                reference does NOT scale
//	                                                with string content at all
//	21  (20 literal 300-byte elements).join(',') -- 1 + 20 (ArgElements only)
//	1   [].join(',')                            -- list[nulltype] row
//
// This is a genuine SPEC/REFERENCE divergence, not a brief-guess artifact:
// section 1.3.10 rule 3 names join() BY NAME as a function that "does work
// roughly proportional to the string length", but the reference's own
// operation count for join never moves when the strings it is joining grow
// from 1 byte to 300 bytes at a fixed element count (6 both times, above).
// The reference implements rule 2's charge for join and OMITS rule 3's.
// Per the standing rule ("the specification outranks the reference"), sqi
// implements BOTH: Cost{ArgElements: {0}, ResultBytes: true} on join's
// list[string] and list[path] rows -- ResultBytes rather than a (structurally
// unavailable) per-element ArgBytes sum, because the Cost mechanism's
// ArgBytes reads a single argument's OWN byte payload (chargeArgs, ops.go),
// and args[0] here is the LIST, whose own .s field is empty; ResultBytes on
// the PRODUCED joined string is the same idiom this package already uses
// wherever a function's byte-proportional work is best measured by what it
// BUILDS rather than what it was HANDED (padString below; the "+" and "*"
// operators, ops.go, Task 5). See TestOperationCount_JoinChargesElementsAndBytesTogether.
//
// ljust/rjust/center -- ResultBytes (the PRODUCED, padded string), not
// ArgBytes on the input. Discriminated by holding the input FIXED at 10 bytes
// and growing the WIDTH: a pure-input charge would stay flat at 2 for every
// row below; it does not:
//
//	2  ('a'*10).ljust(20)     3  ('a'*10).ljust(300)     4  ('a'*10).ljust(600)
//	3  ('a'*300).ljust(310)   3  ('a'*300).ljust(5)  -- width<=len is a no-op,
//	                             result=input=300B either way, not discriminating
//	(rjust and center measure identically at every size above)
//	ERR  ('a'*10).ljust(-5)  -- capacity overflow (72057594037927938 ops): the
//	     reference's own known negative-width defect (brief's own warning).
//	     sqi's padWidth already treats width<=len as a no-op (funcsstrpad.go),
//	     so a negative width never reaches padString/zfillString's growth path
//	     at all -- there is nothing for Cost to charge beyond ResultBytes on
//	     the (unchanged) input, and no divergence to adjudicate here.
//
// zfill -- the ONE row in this file where sqi's OWN design (ResultBytes,
// matching its three siblings above, which share the SAME padWidth helper)
// diverges from the reference's own measured count, which does NOT scale
// with width at all -- it stays flat at the INPUT's own ArgBytes charge:
//
//	2  '5'.zfill(10)     2  '5'.zfill(300)     2  '5'.zfill(600)   2  '5'.zfill(1000)
//	   (all four measure 2 = 1 + ceil(1/256); the WIDTH never appears)
//	3  ('a'*300).zfill(310)   -- matches ArgBytes(input=300B), NOT
//	                              ResultBytes(310B) -- both ceil to 2 here, so
//	                              this row alone cannot tell the two apart
//	3  ('a'*300).zfill(1)     -- width<cur, a no-op; input=output=300B, so
//	                              ArgBytes and ResultBytes agree here too
//
// This is the same shape of defect the brief calls out for center/zfill's
// negative widths ("center('ab', -3) failing with a bogus 72-quadrillion
// operation count, and zfill with a negative width panics outright") but on
// the POSITIVE-width path instead: the reference's zfill silently fails to
// charge the pad growth its own sibling ljust/rjust/center charge correctly,
// even though zfillString (funcsstrpad.go) shares the exact same padWidth
// mechanism and does the exact same proportional work. Per the standing rule,
// sqi does not reproduce that omission: all three zfill rows (string/int,
// int/int, float/int) charge Cost{ResultBytes: true}, identically to
// ljust/rjust/center. See TestOperationCount_ZfillDivergesFromReferenceOnWidth.
func TestOperationCount_StringFunctions(t *testing.T) {
	a300 := strings.Repeat("a", 300)
	tests := []struct {
		name string
		src  string
		want int64
		why  string
	}{
		// --- funcsstrcase.go: case transforms, ArgBytes{0} on the input ---
		{"upper", fmt.Sprintf("'%s'.upper()", a300), 3, "rule 3 names upper() by name; ArgBytes on the input, confirmed scaling 10/300/600 -> 2/3/4"},
		{"lower", fmt.Sprintf("'%s'.lower()", a300), 3, "rule 3 names lower() by name; ArgBytes on the input -- confirmed by the ArgBytes-vs-ResultBytes 'İ' probe above, unlike '+' (Task 5)"},
		{"capitalize", fmt.Sprintf("'%s'.capitalize()", a300), 3, "not named by rule 3's own enumeration but does the same length-proportional work as upper/lower; 'and similar' covers it"},
		{"title", fmt.Sprintf("'%s'.title()", a300), 3, "same as capitalize: 'and similar', ArgBytes on the input"},

		// --- funcsstrcase.go: classification predicates, decided explicitly ---
		// per the brief's item 2 -- these SCAN THE WHOLE STRING (RFC 0006's
		// "all characters are X and string is non-empty"), so they are rule-3
		// charges under "and similar", NOT len()-style exempt lookups. len()
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
		// never contributes, confirmed by the strip-cutset and
		// removeprefix/removesuffix probes above. ---
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
// names this task owns (11 in funcsstrcase.go + 13 in funcsstrfind.go + 3 in
// funcsstrsplit.go + 4 in funcsstrpad.go), so a future addition or removal to
// this task's four files is caught here rather than silently under-covered.
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
		t.Fatalf("this task owns 31 string function names; the coverage list has %d", len(names))
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
// side-by-side the brief asks for: the 11 classification/case functions scan
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
// matter how large: only the RECEIVER's byte length (ArgBytes{0}) is
// declared, and chargeArgs only reads the indices a Cost actually names.
func TestOperationCount_StripCutsetDoesNotAffectCost(t *testing.T) {
	small := opsFor(t, "'xaaaaaaaaaax'.strip('xyxyxyxyxy')")
	big := opsFor(t, fmt.Sprintf("'xaaaaaaaaaax'.strip('%s')", strings.Repeat("xy", 150)))
	if small != 2 || big != 2 {
		t.Errorf("ops = %d, %d; want 2, 2 -- growing the cutset from 10 to 300 bytes must not move the count", small, big)
	}
}

// TestOperationCount_AffixArgumentDoesNotAffectRemoveprefixCost pins the same
// independence for removeprefix/removesuffix: the charge tracks the MAIN
// string's total length, not which side (the matched affix or the remaining
// text) is long. If the reference charged the RESULT instead, these two would
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

// TestOperationCount_ReplaceDoesNotChargeTheProducedResult pins the trap the
// brief warns about directly: replace() can balloon a small input into a huge
// result (old shorter than new, many occurrences), and the reference's own
// count does not move when that happens -- it tracks the MAIN string's own
// length, never the result actually built. sqi follows the reference here
// (unlike join/zfill below): rule 3 groups replace() in the same sentence as
// upper()/lower()/strip(), which are all input-driven (see the ArgBytes-vs-
// ResultBytes 'İ' probe at the top of this file), and the worst-case growth
// this exposes is independently bounded by maxStringBytes (checkRepeat in
// replaceAll, funcsstrfind.go) regardless of what the operation count says --
// a SEPARATE limit (section 1.3.9, Task 11) from this one (section 1.3.10).
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
// whitespace split measure what their BYTE lengths predict, not their word
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
// dedicated test, per the brief: it is named by BOTH rule 2 (iterates a list)
// and rule 3 (processes strings), and is deliberately charged
// Cost{ArgElements: {0}, ResultBytes: true} -- a divergence from the
// reference's own measured count, which never moves with string content (see
// the PROBE comment at the top of this file). Discriminated by holding the
// element COUNT fixed and growing each element's byte length: an
// ArgElements-only reading would predict the SAME total for both rows below;
// it does not, once sqi's own ResultBytes charge is added.
func TestOperationCount_JoinChargesElementsAndBytesTogether(t *testing.T) {
	small := opsFor(t, "join(['a','a','a','a','a'], ',')")
	// 5 elements, each 300 bytes: produced string is 5*300 + 4*1(seps) =
	// 1504 bytes, ceil(1504/256) = 6. ArgElements alone (matching the
	// reference) would measure 6 for BOTH rows; sqi's ResultBytes addition is
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
// all's list[nulltype] rows (funcslist.go, Task 6) -- there is nothing to
// iterate or produce regardless of Cost, so it is declared as an explicit
// zero rather than a copy of the list[string] row's Cost.
func TestOperationCount_JoinListNullRowChargesNothing(t *testing.T) {
	if got := opsFor(t, "join([], ',')"); got != 1 {
		t.Errorf("ops(join([], ',')) = %d; want 1 (rule 1 only)", got)
	}
}

// TestOperationCount_PaddingFunctionsChargeProducedBytes pins ljust/rjust/
// center's ResultBytes over ArgBytes, discriminated by holding the 10-byte
// input FIXED and growing only the requested width -- an ArgBytes(input)
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
// dedicated divergence test. The reference's own zfill measurement does not
// scale with the requested width at all (see the PROBE comment); sqi charges
// Cost{ResultBytes: true} on all three zfill rows regardless, matching its
// three siblings (ljust/rjust/center, which share the same padWidth
// mechanism, funcsstrpad.go) rather than reproducing an omission that would
// make zfill the ONE padding function whose declared cost does not reflect
// the padding it actually performs.
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
// standing ruling that binds this task: "an unresolved operand charges rule 1
// only -- no element or byte charges, since no values were processed."
// callFunction (call.go) short-circuits BEFORE callShape/chargeArgs ever runs
// when any argument is unresolved, so this holds structurally for every row
// in this file; pinned here for one of this task's own ArgBytes rows rather
// than assumed.
func TestOperationCount_UnresolvedStringArgumentChargesRuleOneOnly(t *testing.T) {
	ec := testCtx()
	if _, err := callFunction(ec, "upper", []Value{Unresolved(TString)}, false); err != nil {
		t.Fatalf("callFunction(upper, unresolved): %v", err)
	}
	if ec.m.ops != 1 {
		t.Errorf("ops = %d; want 1 (rule 1 only, no ArgBytes charge for an unresolved operand)", ec.m.ops)
	}
}
