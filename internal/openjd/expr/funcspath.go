// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"strings"
)

var (
	// errEmptyName backs with_name and with_stem on a path that has no final
	// component — "/" is the usual case. Python raises here too.
	errEmptyName = errors.New("the path has an empty name")
	// errInvalidSuffix backs with_suffix on a suffix that is neither empty nor
	// dot-prefixed, is exactly ".", or contains a separator. "png" and "."
	// are errors; ".png" and "" are not.
	errInvalidSuffix = errors.New("an extension must be empty or a dot followed by one or more non-separator characters")
	// errNotRelative backs relative_to. is_relative_to answers false for the
	// same condition rather than failing — the specification defines the pair
	// that way on purpose.
	errNotRelative = errors.New("the path is not relative to the other path")
	// errInvalidName backs with_name (and, through withName, with_stem and
	// with_suffix) on a replacement that is empty, exactly ".", or contains a
	// separator. Fix round 1: the original implementation accepted any
	// string here, including one containing a separator, fabricating a path
	// component that did not exist in the input — measured against the
	// reference at openjd-model 0.11.1, which rejects all three.
	errInvalidName = errors.New("a replacement name must be non-empty, must not be \".\", and must not contain a separator")
	// errEmptyStemHasSuffix backs with_stem specifically: CPython's own
	// with_stem forbids an empty replacement stem when the receiver's
	// existing suffix is non-empty (there would be nothing left to name the
	// file besides the suffix). A receiver with NO suffix does not hit this
	// at all — with_stem("") on such a receiver is exactly with_name(""),
	// which fails errInvalidName instead, for the ordinary "empty name"
	// reason.
	errEmptyStemHasSuffix = errors.New("the path has a non-empty suffix, so the replacement stem cannot be empty")
)

// pathFuncs is sub-project C4's group: RFC 0006's path properties and path
// functions. apply_path_mapping is sub-project D's and lives with its engine in
// pathmapping.go's pathMappingFuncs group, not here, because it needs the
// session's mapping rules (threaded through evalCtx by WithPathMapping) that
// this group's functions never touch.
// COST (sub-project E1, Task 8): rule 3 covers "a string or path value", and a
// path IS one, so every row below that PROCESSES its receiver's (or its
// string argument's) text declares an ArgBytes charge — confirmed scaling in
// the reference at 11/299/599-byte path text (2/3/4, matching path()'s own
// ArgBytes-on-input formula reused throughout this file: 1 call +
// ceil(bytes/256)). See cost_misc_internal_test.go's PROBE for the full set
// of transcribed measurements this comment summarizes.
var pathFuncs = map[string][]Shape{
	// path()'s three rows are the only ones in the whole registry that use
	// FnCtx rather than Fn — see Shape.FnCtx's doc comment for why: they are
	// exactly the rows that have to CHOOSE a flavor, because constructing a
	// path is the only operation that ever does. Every other path operation
	// reads the flavor off its receiver.
	//
	// The TString row charges ArgBytes on the INPUT text, not the normalized
	// output — confirmed with a probe built to discriminate the two: a
	// 302-byte input full of redundant separators that normalizes down to a
	// 2-byte output ("/a") still measures 3 (matching the 302-byte input's
	// own ceil(302/256)=2), not 2 (which a result-based charge would give).
	//
	// The ListOf(TString) row instead declares ResultBytes: true, not
	// ArgElements: there is no single input "text" to read (a list's own .s
	// field is empty, the same structural gap join() hit in Task 7), and the
	// reference's own count tracks the JOINED text's length, not the element
	// count — confirmed flat at 2 for 1, 4 and 20-element literal lists whose
	// joined text stays under 256 bytes, then 3 once one element pushes the
	// joined text past it. This mirrors path(list)'s own construction
	// (pathFromParts(...).String()), the same "charge what was BUILT, not
	// what was handed in" idiom join() and the padding functions
	// (funcsstrpad.go, Task 7) already use.
	//
	// The ListOf(TNull) row (the empty-list literal, "path([])") is Cost{}:
	// it always produces the empty path, so a ResultBytes charge would be
	// zero regardless (ceilDiv256 of an empty string is 0) — declared
	// explicitly rather than left to that coincidence, and this row has no
	// reference reading available at all: the reference errors on
	// "path([])" outright ("No matching signature for path(list[nulltype])"),
	// a registration difference from an earlier sub-project, not a Cost
	// question.
	"path": {
		{Params: []Type{TString}, Ret: TPath, Cost: Cost{ArgBytes: []int{0}}, FnCtx: func(ec evalCtx, args []Value) (Value, error) {
			return boundedPath(args[0].AsStr(), ec.pathFormat)
		}},
		// The list row is what makes the specification's stated roundtrip
		// path(p.parts) == p work: parts puts the root first, and
		// pathFromParts reconstructs it into the SAME parsedPath shape
		// parsePath itself produces, so String() — the one formula that
		// already knows how a root joins its components — renders it.
		{Params: []Type{ListOf(TString)}, Ret: TPath, Cost: Cost{ResultBytes: true}, FnCtx: func(ec evalCtx, args []Value) (Value, error) {
			elems := args[0].AsList()
			parts := make([]string, len(elems))
			for i, e := range elems {
				parts[i] = e.AsStr()
			}
			return boundedPath(pathFromParts(parts, ec.pathFormat).String(), ec.pathFormat)
		}},
		{Params: []Type{ListOf(TNull)}, Ret: TPath, FnCtx: func(ec evalCtx, _ []Value) (Value, error) {
			return boundedPath("", ec.pathFormat)
		}},
	},
	"as_posix": {
		{Params: []Type{TPath}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return String(asPosix(pathOf(args[0]))), nil
		}},
	},
	// is_absolute is Cost{}: it looks only at the receiver's ROOT, matching
	// rule 3's own len()-style exemption ("simple lookups ... that do not
	// process the string content"). Confirmed flat at 1 in the reference once
	// path()'s own construction charge is subtracted out (path(11-byte
	// text).is_absolute() measures 3 = 2 [path()] + 1 [is_absolute's own
	// share]; path(299-byte text).is_absolute() measures 4 = 3 + 1 — the
	// SAME 1 both times, unlike as_posix and the six properties below, which
	// grow with the receiver).
	"is_absolute": {
		{Params: []Type{TPath}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			return Bool(pathOf(args[0]).isAbsolute()), nil
		}},
	},
	// The six properties. Section 1.3.3 makes p.x sugar for __property_x__,
	// and resolve.go's evalProperty calls exactly that name with
	// methodStyle = true — so registering these here is what makes
	// section 1.2.4's receiver-coercion restriction apply to them: a STRING
	// receiver does not coerce to TPath, and 'a/b.txt'.stem is an error by
	// design, not an oversight.
	//
	// All six declare Cost{ArgBytes: {0}} on the receiver: RFC 0006's own
	// worked example (path('/a/b/c.exr').stem measures 4 — 1 call + 1 byte
	// unit for path(), 1 call + 1 byte unit for .stem) generalizes to every
	// property here, confirmed scaling identically (11/299 bytes -> the
	// property's own share is 2/3 each time) whether the property returns a
	// string (name/stem/suffix), a path (parent) or a list (suffixes/parts)
	// — the charge tracks what the property READS off the receiver, not what
	// shape it returns.
	"__property_name__": {
		{Params: []Type{TPath}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return String(pathOf(args[0]).name()), nil
		}},
	},
	"__property_stem__": {
		{Params: []Type{TPath}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			stem, _ := splitStemSuffix(pathOf(args[0]).name())
			return String(stem), nil
		}},
	},
	"__property_suffix__": {
		{Params: []Type{TPath}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			_, suffix := splitStemSuffix(pathOf(args[0]).name())
			return String(suffix), nil
		}},
	},
	"__property_suffixes__": {
		{Params: []Type{TPath}, Ret: ListOf(TString), Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return stringList(pathOf(args[0]).suffixes())
		}},
	},
	"__property_parent__": {
		{Params: []Type{TPath}, Ret: TPath, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			p := pathOf(args[0])
			if len(p.comps) > 0 {
				p.comps = p.comps[:len(p.comps)-1]
			}
			return boundedPath(p.String(), p.flavor)
		}},
	},
	"__property_parts__": {
		{Params: []Type{TPath}, Ret: ListOf(TString), Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return stringList(pathOf(args[0]).parts())
		}},
	},
	// with_name validates its argument through withName — see that function's
	// doc comment for the shared rule with_stem and with_suffix also run
	// their replacement text through, rather than each checking separately.
	//
	// ArgBytes on the RECEIVER ONLY (index 0) — confirmed with a probe built
	// to discriminate: a short receiver with a 300-byte replacement name
	// measures the SAME as a short receiver with a 1-byte replacement, but a
	// 299-byte receiver with a 1-byte replacement scales. The replacement's
	// own length never contributes, the same pattern Task 7 confirmed for
	// strip()'s cutset and removeprefix()'s affix arguments.
	"with_name": {
		{Params: []Type{TPath, TString}, Ret: TPath, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return withName(pathOf(args[0]), args[1].AsStr())
		}},
	},
	// with_stem keeps the receiver's existing suffix and swaps only the
	// stem, porting CPython pathlib's own with_stem algorithm exactly: when
	// the receiver's suffix is EMPTY this is just with_name(stem); when it
	// is not, an empty replacement stem is rejected outright
	// (errEmptyStemHasSuffix — there would be nothing left to name the file
	// besides the suffix) rather than silently producing a bare-suffix name;
	// otherwise the replacement is stem+suffix, run through the SAME
	// withName with_name uses, so a value with_name would reject (a
	// separator, ".") is rejected here too rather than independently
	// re-derived.
	//
	// ArgBytes on the receiver only, same as with_name — confirmed identically.
	"with_stem": {
		{Params: []Type{TPath, TString}, Ret: TPath, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			p := pathOf(args[0])
			_, suffix := splitStemSuffix(p.name())
			stem := args[1].AsStr()
			if suffix == "" {
				return withName(p, stem)
			}
			if stem == "" {
				return Value{}, errEmptyStemHasSuffix
			}
			return withName(p, stem+suffix)
		}},
	},
	// with_suffix replaces the suffix; "" removes it. A non-empty suffix
	// must start with a dot, be more than the bare dot alone, and contain no
	// separator, or it is errInvalidSuffix.
	//
	// That FORMAT check runs before anything else — including the
	// receiver's own empty-name check — and the ordering is DELIBERATE, not
	// incidental: path('/').with_suffix('png') is "invalid suffix", not
	// "empty name", per the reference at openjd-model 0.11.1, even though
	// '/' has no final component either. Python 3.14 checks empty-name
	// first; the reference does not, and fix round 1 confirmed this by
	// measurement — do not reorder these to match Python.
	//
	// ArgBytes on the receiver only, same as with_name.
	"with_suffix": {
		{Params: []Type{TPath, TString}, Ret: TPath, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			suffix := args[1].AsStr()
			p := pathOf(args[0])
			if suffix != "" && (!strings.HasPrefix(suffix, ".") || len(suffix) <= 1 || strings.ContainsAny(suffix, pathSeparators(p))) {
				return Value{}, errInvalidSuffix
			}
			stem, _ := splitStemSuffix(p.name())
			return withName(p, stem+suffix)
		}},
	},
	// with_number's path row splits the receiver's name through withNumber —
	// pathnumber.go's own file, since the frame-number grammar it implements
	// is independent of everything else here — and then rebuilds the path
	// through the SAME withName the other with_* rows use, so a receiver
	// with no final component (p.name() == "") fails with the ordinary
	// errEmptyName rather than a second empty-name check grown here; the
	// string row calls withNumber directly with no path involved at all.
	//
	// Both rows declare ArgBytes: {0} — on the PATH receiver for the first
	// row, on the plain STRING argument for the second (there is no path
	// involved on that row at all) — each confirmed scaling with that row's
	// own index-0 argument.
	"with_number": {
		{Params: []Type{TPath, TInt}, Ret: TPath, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			p := pathOf(args[0])
			newName, err := withNumber(p.name(), args[1].AsInt())
			if err != nil {
				return Value{}, err
			}
			return withName(p, newName)
		}},
		{Params: []Type{TString, TInt}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			result, err := withNumber(args[0].AsStr(), args[1].AsInt())
			if err != nil {
				return Value{}, err
			}
			return String(result), nil
		}},
	},
	// is_relative_to and relative_to both PROCESS TWO path values —
	// relativeParts (below) walks both p.parts() and other.parts() to find
	// the common prefix — so both declare Cost{ArgBytes: {0, 1}}, charging
	// EACH operand's own bytes.
	//
	// This is a DELIBERATE, DOCUMENTED OVERCOUNT relative to the reference,
	// not a match: the reference's own formula is neither "receiver only"
	// nor "sum of both", but max(ceil(len(p)/256), ceil(len(other)/256)) —
	// isolated with three probes that a simpler formula cannot all explain
	// at once (a receiver=2B/other=2B pair measuring 2, a
	// receiver=299B/other=2B pair AND its reverse (2B/299B) both measuring
	// 3, and a receiver=10B/other=249B pair — both individually under the
	// 256-byte ceiling alone — measuring 2, not the 3 a combined-then-ceiled
	// sum would give). The Cost mechanism has no "max of two ceils"
	// primitive — chargeArgs (ops.go) ceils and sums each declared index
	// independently — so declaring both indices instead OVER-counts by
	// (at most) the smaller operand's own ceil, typically 1 op. Given the
	// mechanism cannot express the reference's exact formula, the
	// conservative direction is the safe one: both is_relative_to and
	// relative_to genuinely read every byte relativeParts touches on BOTH
	// operands, so undercounting by charging only one (which a
	// receiver-only or other-only reading would do whenever the UNCHARGED
	// side is the long one) would leave real, template-author-controlled
	// work unmetered — the exact gap section 1.3.10 exists to close. A small
	// constant overcount does not.
	//
	// relative_to's own constructed result gets NO additional ResultBytes
	// charge: confirmed identical to is_relative_to's (bool-returning, so
	// definitionally cannot carry one) at every probed size, so relative_to's
	// own path construction contributes nothing beyond the shared ArgBytes
	// charge above.
	"is_relative_to": {
		{Params: []Type{TPath, TPath}, Ret: TBool, Cost: Cost{ArgBytes: []int{0, 1}}, Fn: func(args []Value) (Value, error) {
			_, ok := relativeParts(pathOf(args[0]), pathOf(args[1]), byteEqual)
			return Bool(ok), nil
		}},
	},
	"relative_to": {
		{Params: []Type{TPath, TPath}, Ret: TPath, Cost: Cost{ArgBytes: []int{0, 1}}, Fn: func(args []Value) (Value, error) {
			p := pathOf(args[0])
			remaining, ok := relativeParts(p, pathOf(args[1]), byteEqual)
			if !ok {
				return Value{}, errNotRelative
			}
			return boundedPath(pathFromParts(remaining, p.flavor).String(), p.flavor)
		}},
	},
}

// asPosix renders p with its separators written as "/".
//
// IT IS FLAVOR-AWARE, and that is the whole content of the function: it
// replaces THE FLAVOR'S OWN canonical separator, reusing pathval.go's single
// per-flavor definition rather than hard-coding a third render-side encoding of
// it beside String()'s. Under POSIX that separator already IS "/", so this is
// the identity; under Windows it is "\", so this is the "\" -> "/" rewrite the
// RFC's table row describes; on a URI it is the identity under every flavor,
// because a URI's body is "/"-separated already and everything else in it is
// opaque object-key text.
//
// The original implementation rewrote EVERY backslash unconditionally, and the
// argument that settles it is this package's own internal contradiction rather
// than a preference between two readings. A backslash is an ordinary POSIX
// filename character and parsePath treats it as one, so path('/a/\b/c') has
// parts ["/", "a", `\b`, "c"] and name "c" — while as_posix() answered
// "/a/b/c", which has different parts and a different name. One value cannot be
// both. The same rewrite turned the S3 key "a\b" into the different key "a/b",
// contradicting the "a URI NORMALIZES NOTHING" rule stated in splitURI and in
// doc.go.
//
// CPython settles the direction: PurePath.as_posix() is
// str(self).replace(self.parser.sep, '/'), the flavor's separator, so
// PurePosixPath('/a\\b/c').as_posix() is "/a\b/c" (measured). RFC 0006 line 857
// names as_posix among the functions that "match Python's pathlib API" — the
// same clause test/oracle/baseline.txt already cites to rule against the
// reference for stem/suffix — and the RFC table's "return string with forward
// slashes" summarizes the Windows case, the only one in which a separator has
// to change at all. The reference implementation agrees with the OLD behavior
// (measured at openjd-model 0.11.1), so this is a baselined divergence; the
// reference's own parts for that path keep the backslash as an ordinary
// component, so the contradiction above is present in the reference too.
func asPosix(p parsedPath) string {
	if p.isURI {
		return p.String()
	}
	return strings.ReplaceAll(p.String(), string(pathCanonicalSeparator(p.flavor)), "/")
}

// boundedPath builds a path value with its text bounded before it is stored.
func boundedPath(text string, f PathFormat) (Value, error) {
	if err := checkStringBytes(len(text)); err != nil {
		return Value{}, err
	}
	return Path(text, f), nil
}

// pathFromParts reconstructs a parsedPath from the specification's p.parts()
// shape — a root, when there is one, followed by ordinary components — using
// parsePath itself to decide what counts as a root, rather than a second
// formula beside it.
//
// A FIX-ROUND NOTE, because the first version of this function got this
// wrong: it re-derived "how does a root join its components" as its own
// string formula (concatenate if the root already ends in a separator,
// otherwise insert one) instead of reusing parsedPath.String(), which already
// answers that question correctly for every root shape — including a bare
// Windows drive with NO separator ("C:"), which String() renders correctly
// for free by never inserting a separator between root and components at
// all, only between components. The reimplementation missed exactly that
// case: joining root "C:" with component "a" produced "C:\a" (drive-rooted,
// WRONG) instead of "C:a" (drive-relative, matching the reference and RFC
// 0006's own path(p.parts) == p guarantee). Reconstructing a parsedPath and
// deferring to String() closes that gap by construction — there is no longer
// a second place a root-joining rule can drift out of sync with the first.
//
// The list is not guaranteed to have come from parts(): "path(['a', 'b'])"
// has no root at all, so the first element must be CLASSIFIED, not assumed.
// It is classified by parsing it ALONE under the same flavor: a token that
// parses to a non-empty root with NO leftover components — parsePath
// consumed the whole token as an anchor and nothing else — is a root.
// Anything else, including a token that parses to component(s) of its own
// (a plain relative element like "a", or an empty token), is treated as an
// ordinary first component instead. Only the head is ever probed this way;
// every later element is always an ordinary component, because a real path
// has at most one root and parts() would never place one anywhere else.
func pathFromParts(parts []string, f PathFormat) parsedPath {
	f = f.resolve()
	if len(parts) == 0 {
		return parsedPath{flavor: f}
	}
	head, tail := parts[0], parts[1:]
	if probe := parsePath(head, f); probe.root != "" && len(probe.comps) == 0 {
		return parsedPath{root: probe.root, comps: append([]string{}, tail...), flavor: f, isURI: probe.isURI}
	}
	return parsedPath{comps: append([]string{}, parts...), flavor: f}
}

// withName implements with_name's replacement after both of its checks: the
// receiver must have a final component (errEmptyName), and name must be a
// legal replacement (errInvalidName, via isValidReplacementName). with_stem
// and with_suffix funnel their own computed replacement text through this
// SAME function rather than re-checking validity themselves, so the three
// with_* functions can never validate a replacement two different ways —
// exactly the "second formula beside an existing one" shape this wave's
// three earlier Criticals all had.
func withName(p parsedPath, name string) (Value, error) {
	if p.name() == "" {
		return Value{}, errEmptyName
	}
	if !isValidReplacementName(name, p) {
		return Value{}, errInvalidName
	}
	p.comps[len(p.comps)-1] = name
	return boundedPath(p.String(), p.flavor)
}

// isValidReplacementName reports whether name is legal as with_name's
// argument. This is CPython pathlib's own rule, reproduced exactly by the
// openjd-model reference (measured, fix round 1): empty, exactly ".", or
// containing a separator are invalid. ".." is explicitly NOT invalid — only
// exact equality to "." is checked, not "starts with a dot run" — and
// neither is a drive-looking string like "C:" under POSIX, where ':' is not
// a separator at all.
func isValidReplacementName(name string, p parsedPath) bool {
	return name != "" && name != "." && !strings.ContainsAny(name, pathSeparators(p))
}

// pathSeparators reports which characters separate components for p's
// shape, deferring to pathval.go's pathSeparatorChars — the SAME definition
// parsePath's POSIX branch and parseWindows themselves split on — rather
// than a second hand-written set here. Fix round 2: an earlier version of
// this function carried its own windowsSeparatorChars constant that only
// this validator read, which was still the "two formulas that happen to
// agree today" shape the whole wave has been fixing, just moved rather than
// closed.
//
// A URI's body is always "/"-only regardless of flavor — splitURI hard-codes
// "/" for its own component split and never calls pathSeparatorChars at
// all, so this checks p.isURI itself before consulting it. Measured against
// the reference (fix round 1): it is POSIX-only for these functions and
// accepts a backslash outright, including on a drive-looking receiver like
// "C:/a/b" — POSIX parsing never treats ':' specially, so the drive does not
// switch the flavor.
func pathSeparators(p parsedPath) string {
	if p.isURI {
		return "/"
	}
	return pathSeparatorChars(p.flavor)
}

// byteEqual is plain string equality, given a name so relativeParts' two
// in-package callers (is_relative_to/relative_to here, and
// pathmapping.go's applyFileRule for a POSIX or non-Windows-flavored rule)
// can pass a comparator by value without each writing out its own
// single-use closure.
func byteEqual(a, b string) bool { return a == b }

// relativeParts backs relative_to and is_relative_to, and — via an injected
// eq — pathmapping.go's applyFileRule, which asks the identical question
// ("are other's parts a full prefix of p's, and what remains?") for a
// WINDOWS-flavored path-mapping rule, where component comparison must be
// case-insensitive rather than the byte-exact equality relative_to/
// is_relative_to use. eq is the ONE place that distinction is made; every
// other rule below (the anchorless-other guard, the length check, the
// separator-run trimming) is unaffected by it and applies identically to
// both callers. is_relative_to and relative_to pass byte-exact equality, so
// their own behavior is unchanged by this parameter's existence.
//
// Both compare parts() element-wise rather than reasoning about root/comps
// separately, which is what makes a URI fall out for free: its
// scheme+authority is just the first element of parts(), so a prefix match
// over parts is already a prefix match over the full URI without any
// URI-specific code here.
//
// ok is true when other's parts are a full prefix of p's; remaining is
// whatever is left of p's parts after that prefix, valid only when ok.
func relativeParts(p, other parsedPath, eq func(a, b string) bool) (remaining []string, ok bool) {
	// The separator run at the boundary between the base and what follows it
	// belongs to the boundary and to neither side, which is section 2.1.5's own
	// rule for "/" ("a trailing slash on the left operand is consumed by the
	// join") applied to the operand these two functions call a base. Both
	// halves are the SAME rule and both reuse pathval.go's pair rather than
	// re-deriving anything here: trimTrailingEmptyComps on the base is what
	// makes the operator-written prefix "s3://renders/" match the objects under
	// it, as RFC 0006's "For URIs, checks prefix match on the full URI"
	// requires; trimLeadingEmptyComps on the remainder is what keeps the result
	// RELATIVE (see that function's doc comment for the absolute-result defect
	// it closes).
	//
	// Only a URI ever carries an empty component to trim — both filesystem
	// parsers drop them — so for POSIX and Windows both calls are no-ops.
	pp, op := p.parts(), trimTrailingEmptyComps(other.parts())
	// An ANCHORLESS other is the one case a prefix test over parts() gets
	// wrong on its own. path(".") has no parts at all, so its (empty) part
	// list is trivially a prefix of every path's — including an absolute
	// one's, which would make path("/a/b").is_relative_to(path(".")) true and
	// path("/a/b").relative_to(path(".")) answer "/a/b", a "relative" result
	// that is absolute. CPython says False here, because is_relative_to is
	// "other == self or other in self.parents" and an absolute path's parents
	// run down to "/" and never reach "."; the reference implementation agrees,
	// and the specification names PurePath.is_relative_to() as the rule. When
	// other DOES have parts, its first one is its anchor and the prefix test
	// already compares the anchors, so this guard is needed only for the empty
	// case — and it must not reject it outright, since two anchorless paths
	// really are comparable ("a/b" IS relative to ".").
	if other.root == "" && p.root != "" {
		return nil, false
	}
	if len(op) > len(pp) {
		return nil, false
	}
	for i, part := range op {
		if !eq(pp[i], part) {
			return nil, false
		}
	}
	return trimLeadingEmptyComps(pp[len(op):]), true
}
