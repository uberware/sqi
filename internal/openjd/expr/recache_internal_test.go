// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strconv"
	"strings"
	"testing"
)

// evalCtxFor parses src and evaluates it against syms through the package's own
// entry points rather than through Eval, so a test can read the evalCtx the
// evaluation used -- the regex cache lives there, and Eval discards it.
func evalCtxFor(t *testing.T, src string, syms Symbols) (Value, evalCtx) {
	t.Helper()
	e, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q) failed: %v", src, err)
	}
	ec := newEvalCtx(e.src, syms, nil)
	v, err := evalNode(e.root, ec, TAny, 0)
	if err != nil {
		t.Fatalf("evaluating %q failed: %v", src, err)
	}
	return v, ec
}

// stringListSymbols binds Param.Files to a list of the given strings.
func stringListSymbols(items ...string) MapSymbols {
	vals := make([]Value, len(items))
	for i, s := range items {
		vals[i] = String(s)
	}
	return MapSymbols{"Param.Files": List(TString, vals)}
}

// TestRECache_ConstantPatternInAComprehensionCompilesOnce is the whole point of
// the cache: a pattern written once in the source is compiled once, however
// many elements the comprehension around it iterates.
func TestRECache_ConstantPatternInAComprehensionCompilesOnce(t *testing.T) {
	const n = 200
	items := make([]string, n)
	for i := range items {
		items[i] = "shot" + strconv.Itoa(i) + ".exr"
	}
	src := `[re_match(s, r'shot\d+\.exr') for s in Param.Files]`
	v, ec := evalCtxFor(t, src, stringListSymbols(items...))
	if got := len(v.l); got != n {
		t.Fatalf("evaluating %q produced %d elements, want %d", src, got, n)
	}
	if got := ec.re.compiles; got != 1 {
		t.Errorf("evaluating %q over %d elements compiled the pattern %d times, want 1", src, n, got)
	}
	if got := len(ec.re.entries); got != 1 {
		t.Errorf("the cache holds %d entries, want 1", got)
	}
}

// TestRECache_AnInvalidPatternIsCompiledOnceToo covers the other half of the
// same win: a pattern that FAILS must not be re-translated and re-failed once
// per element either, and the error a later element sees must be the error the
// first one saw.
func TestRECache_AnInvalidPatternIsCompiledOnceToo(t *testing.T) {
	src := `[re_match(s, r'(?=x)') for s in Param.Files]`
	e, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q) failed: %v", src, err)
	}
	ec := newEvalCtx(e.src, stringListSymbols("a", "b", "c"), nil)
	if _, err := evalNode(e.root, ec, TAny, 0); err == nil {
		t.Fatalf("evaluating %q succeeded; want the lookahead rejection", src)
	}
	// The comprehension stops at the first failure, so one compile is all this
	// can observe directly -- what it pins is that the failure was CACHED, so a
	// second call with the same pattern does not translate it again.
	if got := ec.re.compiles; got != 1 {
		t.Fatalf("evaluating %q compiled %d times, want 1", src, got)
	}
	if _, err := ec.re.compile(`(?=x)`); err == nil {
		t.Fatal("a second compile of the same invalid pattern succeeded")
	}
	if got := ec.re.compiles; got != 1 {
		t.Errorf("a second compile of the same invalid pattern recompiled it: compiles = %d, want 1", got)
	}
}

// TestRECache_StopsAtTheCap is the hazard the cap exists for. A pattern is an
// attacker-supplied string and need not be constant within an evaluation, so an
// unbounded map would let one comprehension retain one compiled program per
// element. Past the cap the cache simply stops storing -- it does not evict and
// it does not grow -- and every element still gets the right answer.
func TestRECache_StopsAtTheCap(t *testing.T) {
	const n = maxCachedPatterns * 3
	items := make([]string, n)
	want := make([]string, n)
	for i := range items {
		items[i] = "x" + strconv.Itoa(i)
		want[i] = `["` + items[i] + `"]`
	}
	// Each element is its own pattern, and each matches itself literally.
	src := `[re_match(s, s) for s in Param.Files]`
	v, ec := evalCtxFor(t, src, stringListSymbols(items...))
	if got, wantStr := v.String(), "["+strings.Join(want, ", ")+"]"; got != wantStr {
		t.Errorf("evaluating %q = %s, want %s", src, got, wantStr)
	}
	if got := len(ec.re.entries); got != maxCachedPatterns {
		t.Errorf("the cache holds %d entries, want it capped at %d", got, maxCachedPatterns)
	}
	if got := ec.re.compiles; got != n {
		t.Errorf("compiled %d times over %d distinct patterns, want %d", got, n, n)
	}
}

// TestRECache_MatchesTheUncachedResultAtEveryCallSite is the correctness
// guarantee: a cache hit must return exactly what a fresh compile returns, for
// every function that compiles a pattern and for a valid and an invalid pattern
// alike -- the same matches, and the same error text.
//
// It compares the SECOND call on a cached context (the hit) against a context
// whose cache is nil (compilePattern every time), which is also the mutation
// point: making compile bypass its map makes the two identical by construction
// and this test vacuous, so read it alongside the two counting tests above.
func TestRECache_MatchesTheUncachedResultAtEveryCallSite(t *testing.T) {
	const (
		subject = "shot010_shot020"
		valid   = `shot(\d+)`
		invalid = `(?=x)`
	)
	calls := map[string]func(ec evalCtx, pattern string) (Value, error){
		"re_match": func(ec evalCtx, pattern string) (Value, error) {
			return reFind(ec, subject, pattern, true)
		},
		"re_search": func(ec evalCtx, pattern string) (Value, error) {
			return reFind(ec, subject, pattern, false)
		},
		"re_findall": func(ec evalCtx, pattern string) (Value, error) {
			return reFindAll(ec, subject, pattern)
		},
		"re_sub": func(ec evalCtx, pattern string) (Value, error) {
			return reSub(ec, subject, pattern, "X")
		},
		"re_split": func(ec evalCtx, pattern string) (Value, error) {
			return reSplit(ec, subject, pattern, reSplitUnlimited)
		},
	}
	for name, call := range calls {
		for _, pattern := range []string{valid, invalid} {
			t.Run(name+" "+pattern, func(t *testing.T) {
				uncached := newEvalCtx("", nil, nil)
				uncached.re = nil
				wantVal, wantErr := call(uncached, pattern)

				// Two warm-up calls, so the third below is unambiguously a
				// cache hit rather than the call that populated it.
				cached := newEvalCtx("", nil, nil)
				for range 2 {
					if _, err := call(cached, pattern); (err == nil) != (wantErr == nil) {
						t.Fatalf("%s warm-up error = %v, uncached = %v", name, err, wantErr)
					}
				}
				gotVal, gotErr := call(cached, pattern)
				if cached.re.compiles != 1 {
					t.Errorf("%s compiled %d times over three identical calls, want 1", name, cached.re.compiles)
				}
				if (gotErr == nil) != (wantErr == nil) {
					t.Fatalf("%s cached error = %v, uncached = %v", name, gotErr, wantErr)
				}
				if gotErr != nil {
					if gotErr.Error() != wantErr.Error() {
						t.Fatalf("%s cached error = %q, uncached = %q", name, gotErr, wantErr)
					}
					return
				}
				if got, want := gotVal.String(), wantVal.String(); got != want {
					t.Errorf("%s cached = %s, uncached = %s", name, got, want)
				}
				if got, want := gotVal.Type.String(), wantVal.Type.String(); got != want {
					t.Errorf("%s cached typed %s, uncached %s", name, got, want)
				}
			})
		}
	}
}

// TestRECache_IsNotSharedBetweenEvaluations pins what makes the bound above a
// bound at all: the cache dies with the evaluation that created it, so nothing
// accumulates across requests and no eviction policy or lock is needed.
func TestRECache_IsNotSharedBetweenEvaluations(t *testing.T) {
	const src = `re_match('abc', 'a(b)c')`
	_, first := evalCtxFor(t, src, MapSymbols(nil))
	_, second := evalCtxFor(t, src, MapSymbols(nil))
	if first.re == second.re {
		t.Fatal("two evaluations share one cache")
	}
	if first.re.compiles != 1 || second.re.compiles != 1 {
		t.Errorf("compiles = %d and %d, want 1 each: the second evaluation reused the first's cache",
			first.re.compiles, second.re.compiles)
	}
}

// TestRECache_NilCacheStillCompiles covers the contexts that have no cache at
// all -- unmeteredCtx (meter.go) builds one by hand -- so a nil cache must
// compile rather than panic.
func TestRECache_NilCacheStillCompiles(t *testing.T) {
	var c *reCache
	re, err := c.compile(`a(b)c`)
	if err != nil {
		t.Fatalf("a nil cache failed to compile: %v", err)
	}
	if !re.MatchString("abc") {
		t.Error("the pattern a nil cache compiled does not match")
	}
	if _, err := c.compile(`(?=x)`); err == nil {
		t.Error("a nil cache accepted an unsupported pattern")
	}
}
