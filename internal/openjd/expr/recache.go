// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "regexp"

// maxCachedPatterns is how many compiled patterns one evaluation's cache may
// hold. Past it the cache stops storing and every further pattern is compiled
// uncached: nothing is evicted, and the map never grows again.
//
// A cap is not optional here, and the reason is the same one that put every
// other bound in this package there. A pattern is an arbitrary string from a
// submitted template — it is not required to be a literal, and
// "[re_match(s, s) for s in Param.Files]" presents one distinct pattern per
// element. An uncapped map would therefore turn a comprehension into a memory
// amplifier: one compiled program retained per element, growing with the input,
// in a package reachable from POST /api/v1/jobs. With the cap the cache retains
// at most maxCachedPatterns compiled programs whatever the input is, so the most
// it can add to an evaluation's peak is a constant multiple of the single
// compile that already happened without it — not a multiple of the list length.
//
// The value is 8, four times the largest count in real templates: across the
// vendored conformance fixtures, sqi's reference presets and the differential
// corpus, no expression contains more than two regex call sites (the only way
// an evaluation reaches a distinct literal pattern). Real templates therefore
// do not approach this, and the miss path past it is a correctness no-op rather
// than a failure.
//
// It is not a semantic bound, which is why it lives here rather than in
// limits.go with the five that are. Every one of those decides whether an
// expression is rejected; this one decides only whether a compile is repeated.
// Raising or lowering it cannot change a single result, an error message or an
// operation count.
const maxCachedPatterns = 8

// reCache is one evaluation's compiled-pattern cache.
//
// Per-evaluation rather than process-wide. It dies with the
// evaluation that created it, so it needs no eviction policy, no expiry and no
// lock, and it cannot carry state — or a compile's cost — between requests,
// templates or tenants. What it captures is the dominant win: a pattern written
// once in the source and evaluated once per element of a comprehension, which
// would otherwise compile once per element.
//
// It is held by pointer on evalCtx for the reason meter's own comment gives:
// evalCtx flows by value, so a map created on a copy would be invisible to the
// caller and the cache would never hit. A nil *reCache is usable and never
// caches — see
// compile — which is what lets unmeteredCtx (meter.go) and the pattern tests
// build a context without one.
//
// It does not participate in metering, and must not. Section 1.3.10's
// charges are applied by callShape (ops.go) around the whole call, before any
// Fn runs, so a cached compile is charged exactly what an uncached one is. The
// differential oracle compares operation counts, and caching must not move any
// of them.
type reCache struct {
	// entries maps a raw spec-dialect pattern — the string the template wrote,
	// not the translated Go one — to what compiling it produced. A failure is
	// cached too, so a bad pattern inside a comprehension is translated and
	// rejected once rather than once per element; the error value is returned
	// unchanged on every hit, so the message a caller sees is byte-for-byte what
	// an uncached compile gives.
	entries map[string]reResult
	// compiles counts the patterns that reached compilePattern. Nothing
	// in production reads it: it exists so the tests can assert that a constant
	// pattern in a comprehension compiles once, which is the only property that
	// distinguishes a working cache from a cache that stores and never hits.
	compiles int
}

// reResult is one cache entry: what compilePattern answered for a pattern.
type reResult struct {
	re  *regexp.Regexp
	err error
}

// compile returns the compiled form of a spec-dialect pattern, reusing this
// evaluation's earlier answer when there is one.
//
// A nil receiver compiles without caching, so a context built without a cache
// still produces identical results.
func (c *reCache) compile(pattern string) (*regexp.Regexp, error) {
	if c == nil {
		return compilePattern(pattern)
	}
	if hit, ok := c.entries[pattern]; ok {
		return hit.re, hit.err
	}
	c.compiles++
	re, err := compilePattern(pattern)
	if len(c.entries) >= maxCachedPatterns {
		// Past the cap: answer, but do not retain. See maxCachedPatterns.
		return re, err
	}
	if c.entries == nil {
		// Built on first use rather than in newEvalCtx: the overwhelming
		// majority of evaluations contain no regex at all, and every one of them
		// would otherwise pay for a map.
		c.entries = make(map[string]reResult, 1)
	}
	c.entries[pattern] = reResult{re: re, err: err}
	return re, err
}
