// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// errGroupReference is RFC 0006's rule that re_sub's replacement is literal
// text: "group references (\1, \g<1>, $1, ${1}) are not supported and are
// errors".
var errGroupReference = errors.New("group references in a replacement string are not supported")

// reFuncs is sub-project C3's regular-expression group.
//
// Every pattern goes through translatePattern (repattern.go) before it reaches
// Go's engine. That is not a convenience: Go's regexp is neither Python's re
// nor Rust's regex, and RFC 0006 defines the accepted dialect as the
// INTERSECTION of those two. See translatePattern's comment for the three
// directions in which they differ.
var reFuncs = map[string][]Shape{
	// re_match and re_search differ only in anchoring. Both return the full
	// match at index 0 followed by the capture groups, or null.
	"re_match": {
		{Params: []Type{TString, TString}, Ret: UnionOf(ListOf(TString), TNull), Fn: func(args []Value) (Value, error) {
			return reFind(args[0].AsStr(), args[1].AsStr(), true)
		}},
	},
	"re_search": {
		{Params: []Type{TString, TString}, Ret: UnionOf(ListOf(TString), TNull), Fn: func(args []Value) (Value, error) {
			return reFind(args[0].AsStr(), args[1].AsStr(), false)
		}},
	},
	// re_findall's RESULT SHAPE depends on the pattern's group count, which is
	// a runtime value, so its declared return is a union. RFC 0006: full
	// matches with no group, the single group's captures with one, a list per
	// match with two or more.
	"re_findall": {
		{Params: []Type{TString, TString}, Ret: UnionOf(ListOf(TString), ListOf(ListOf(TString))), Fn: func(args []Value) (Value, error) {
			return reFindAll(args[0].AsStr(), args[1].AsStr())
		}},
	},
	"re_sub": {
		{Params: []Type{TString, TString, TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return reSub(args[0].AsStr(), args[1].AsStr(), args[2].AsStr())
		}},
	},
	"re_escape": {
		{Params: []Type{TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			quoted := regexp.QuoteMeta(args[0].AsStr())
			if err := checkStringBytes(len(quoted)); err != nil {
				return Value{}, err
			}
			return String(quoted), nil
		}},
	},
	"re_split": {
		{Params: []Type{TString, TString}, Ret: ListOf(TString), Fn: func(args []Value) (Value, error) {
			return reSplit(args[0].AsStr(), args[1].AsStr(), -1)
		}},
		{Params: []Type{TString, TString, TInt}, Ret: ListOf(TString), Fn: func(args []Value) (Value, error) {
			return reSplit(args[0].AsStr(), args[1].AsStr(), args[2].AsInt())
		}},
	},
}

// compilePattern translates and compiles, so no caller ever hands Go a raw
// spec-dialect pattern.
func compilePattern(pattern string) (*regexp.Regexp, error) {
	translated, err := translatePattern(pattern)
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile(translated)
	if err != nil {
		return nil, fmt.Errorf("invalid regular expression %q: %w", pattern, err)
	}
	return re, nil
}

// reFind backs re_match and re_search. anchored restricts the match to the
// START of the string, which is re_match's whole difference from re_search.
func reFind(s, pattern string, anchored bool) (Value, error) {
	re, err := compilePattern(pattern)
	if err != nil {
		return Value{}, err
	}
	// Go's search is LEFTMOST, so if any match starts at offset 0 the one it
	// finds starts at 0. That makes "the leftmost match begins at 0" an exact
	// test for Python's re.match anchoring — no second, anchored search is
	// needed.
	loc := re.FindStringSubmatchIndex(s)
	if len(loc) == 0 || (anchored && loc[0] != 0) {
		return Value{Type: TNull}, nil
	}
	// loc holds one (start, end) BYTE-OFFSET pair per group, in the same
	// order FindStringSubmatch would return the strings themselves — a
	// non-participating group's pair is (-1, -1). Deriving the strings from
	// these offsets directly avoids a second regex pass over s.
	n := len(loc) / 2
	if err := checkElementCount(n); err != nil {
		return Value{}, err
	}
	vals := make([]Value, n)
	for i := range n {
		start, end := loc[2*i], loc[2*i+1]
		if start < 0 {
			vals[i] = String("")
		} else {
			vals[i] = String(s[start:end])
		}
	}
	return List(TString, vals), nil
}

// reFindAll backs re_findall, whose result shape depends on the group count.
func reFindAll(s, pattern string) (Value, error) {
	re, err := compilePattern(pattern)
	if err != nil {
		return Value{}, err
	}
	// Bounded at the point of allocation: a zero-width pattern can match at
	// every byte position, so an unbounded -1 here would build up to
	// maxStringBytes+1 submatch slices before checkElementCount below ever
	// runs. maxElements+1 lets the engine itself stop at the bound.
	matches := re.FindAllStringSubmatch(s, maxElements+1)
	if err := checkElementCount(len(matches)); err != nil {
		return Value{}, err
	}
	groups := re.NumSubexp()
	vals := make([]Value, 0, len(matches))
	switch groups {
	case 0:
		for _, m := range matches {
			vals = append(vals, String(m[0]))
		}
		return List(TString, vals), nil
	case 1:
		for _, m := range matches {
			vals = append(vals, String(m[1]))
		}
		return List(TString, vals), nil
	default:
		// This branch builds ONE inner list PER MATCH, so the quantity that
		// matters is the PRODUCT len(matches)*groups, not len(matches) alone
		// — a pattern with a large capturing-group count can hold
		// len(matches) small while the product is still enormous.
		// checkRepeat guards that product itself, before any inner list is
		// allocated; see its own doc for why forming the product first and
		// checking the result is not a check at all.
		if _, err := checkRepeat(groups, int64(len(matches)), maxElements); err != nil {
			return Value{}, err
		}
		for _, m := range matches {
			inner := make([]Value, groups)
			for g := 1; g <= groups; g++ {
				inner[g-1] = String(m[g])
			}
			vals = append(vals, List(TString, inner))
		}
		return List(ListOf(TString), vals), nil
	}
}

// reSub backs re_sub. The replacement is LITERAL: RFC 0006 states that group
// references are errors, so this rejects all four spellings and then uses
// Go's literal replacement, which does not expand "$1" either.
//
// The output size is bounded ARITHMETICALLY, before any replacement is ever
// built. re.FindAllStringIndex allocates only byte-offset pairs, so counting
// matches and summing their matched-byte lengths is cheap regardless of what
// repl is. re.ReplaceAllLiteralString, by contrast, materializes the whole
// result first — and a pattern that matches zero-width at every position, on
// a string already at maxStringBytes, times a multi-KB repl, would allocate
// gigabytes before a check on the RESULT could ever run. That is the same
// hazard replaceAll (funcsstrfind.go) already guards against for plain
// string replacement.
func reSub(s, pattern, repl string) (Value, error) {
	if err := rejectGroupReferences(repl); err != nil {
		return Value{}, err
	}
	re, err := compilePattern(pattern)
	if err != nil {
		return Value{}, err
	}
	// Bounded even while only counting: an unbounded -1 here would let a
	// zero-width pattern enumerate maxStringBytes+1 matches before anything
	// downstream could object.
	locs := re.FindAllStringIndex(s, maxElements+1)
	if err := checkElementCount(len(locs)); err != nil {
		return Value{}, err
	}
	var matchedBytes int
	for _, loc := range locs {
		matchedBytes += loc[1] - loc[0]
	}
	// checkRepeat guards the multiplication itself, not just its result — see
	// its own doc for why forming len(repl)*len(locs) directly and checking
	// afterward is not a check at all.
	replTotal, err := checkRepeat(len(repl), int64(len(locs)), maxStringBytes)
	if err != nil {
		return Value{}, err
	}
	outLen := int64(len(s)-matchedBytes) + replTotal
	if err := checkStringBytes(int(outLen)); err != nil {
		return Value{}, err
	}
	out := re.ReplaceAllLiteralString(s, repl)
	return String(out), nil
}

// rejectGroupReferences refuses the four spellings RFC 0006 names.
func rejectGroupReferences(repl string) error {
	for i := 0; i < len(repl); i++ {
		switch repl[i] {
		case '\\':
			if i+1 < len(repl) && (isDigitByte(repl[i+1]) || strings.HasPrefix(repl[i+1:], "g<")) {
				return fmt.Errorf("%w: %q", errGroupReference, repl)
			}
			i++
		case '$':
			if i+1 < len(repl) && (isDigitByte(repl[i+1]) || repl[i+1] == '{') {
				return fmt.Errorf("%w: %q", errGroupReference, repl)
			}
		}
	}
	return nil
}

func isDigitByte(b byte) bool { return b >= '0' && b <= '9' }

// reSplit backs re_split. A negative maxsplit means unlimited, matching Go's
// own Split convention and C2's split().
func reSplit(s, pattern string, maxsplit int64) (Value, error) {
	re, err := compilePattern(pattern)
	if err != nil {
		return Value{}, err
	}
	// Bounded even in the "unlimited" case: Go's own -1 sentinel would let a
	// zero-width pattern split into maxStringBytes+1 parts before the size is
	// ever checked. maxElements+1 lets the engine itself stop at the bound,
	// so the existing checkElementCount call below reports the overflow
	// rather than merely observing it after an unbounded split already ran.
	n := maxElements + 1
	if maxsplit >= 0 {
		// Go counts RESULT PARTS where RFC 0006 counts SPLITS, so the limit is
		// one higher. maxsplit is clamped to the string length first: it
		// arrives as an arbitrary int64 and could otherwise overflow int.
		if maxsplit > int64(len(s)) {
			maxsplit = int64(len(s))
		}
		n = int(maxsplit) + 1
	}
	parts := re.Split(s, n)
	if err := checkElementCount(len(parts)); err != nil {
		return Value{}, err
	}
	vals := make([]Value, len(parts))
	for i, p := range parts {
		vals[i] = String(p)
	}
	return List(TString, vals), nil
}
