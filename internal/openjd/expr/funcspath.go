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
	errInvalidName = errors.New("invalid replacement name")
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
// functions. apply_path_mapping is NOT here — it is sub-project D's, because it
// needs the session's mapping rules, which this package has no access to.
var pathFuncs = map[string][]Shape{
	// path()'s three rows are the only ones in the whole registry that use
	// FnCtx rather than Fn — see Shape.FnCtx's doc comment for why: they are
	// exactly the rows that have to CHOOSE a flavor, because constructing a
	// path is the only operation that ever does. Every other path operation
	// reads the flavor off its receiver.
	"path": {
		{Params: []Type{TString}, Ret: TPath, FnCtx: func(ec evalCtx, args []Value) (Value, error) {
			return boundedPath(args[0].AsStr(), ec.pathFormat)
		}},
		// The list row is what makes the specification's stated roundtrip
		// path(p.parts) == p work: parts puts the root first, and
		// pathFromParts reconstructs it into the SAME parsedPath shape
		// parsePath itself produces, so String() — the one formula that
		// already knows how a root joins its components — renders it.
		{Params: []Type{ListOf(TString)}, Ret: TPath, FnCtx: func(ec evalCtx, args []Value) (Value, error) {
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
		{Params: []Type{TPath}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return String(strings.ReplaceAll(pathText(args[0]), `\`, "/")), nil
		}},
	},
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
	"__property_name__": {
		{Params: []Type{TPath}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return String(pathOf(args[0]).name()), nil
		}},
	},
	"__property_stem__": {
		{Params: []Type{TPath}, Ret: TString, Fn: func(args []Value) (Value, error) {
			stem, _ := splitStemSuffix(pathOf(args[0]).name())
			return String(stem), nil
		}},
	},
	"__property_suffix__": {
		{Params: []Type{TPath}, Ret: TString, Fn: func(args []Value) (Value, error) {
			_, suffix := splitStemSuffix(pathOf(args[0]).name())
			return String(suffix), nil
		}},
	},
	"__property_suffixes__": {
		{Params: []Type{TPath}, Ret: ListOf(TString), Fn: func(args []Value) (Value, error) {
			return stringList(pathOf(args[0]).suffixes())
		}},
	},
	"__property_parent__": {
		{Params: []Type{TPath}, Ret: TPath, Fn: func(args []Value) (Value, error) {
			p := pathOf(args[0])
			if len(p.comps) > 0 {
				p.comps = p.comps[:len(p.comps)-1]
			}
			return boundedPath(p.String(), p.flavor)
		}},
	},
	"__property_parts__": {
		{Params: []Type{TPath}, Ret: ListOf(TString), Fn: func(args []Value) (Value, error) {
			return stringList(pathOf(args[0]).parts())
		}},
	},
	// with_name validates its argument through withName — see that function's
	// doc comment for the shared rule with_stem and with_suffix also run
	// their replacement text through, rather than each checking separately.
	"with_name": {
		{Params: []Type{TPath, TString}, Ret: TPath, Fn: func(args []Value) (Value, error) {
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
	"with_stem": {
		{Params: []Type{TPath, TString}, Ret: TPath, Fn: func(args []Value) (Value, error) {
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
	"with_suffix": {
		{Params: []Type{TPath, TString}, Ret: TPath, Fn: func(args []Value) (Value, error) {
			suffix := args[1].AsStr()
			p := pathOf(args[0])
			if suffix != "" && (!strings.HasPrefix(suffix, ".") || len(suffix) <= 1 || strings.ContainsAny(suffix, pathSeparators(p))) {
				return Value{}, errInvalidSuffix
			}
			stem, _ := splitStemSuffix(p.name())
			return withName(p, stem+suffix)
		}},
	},
	"is_relative_to": {
		{Params: []Type{TPath, TPath}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			_, ok := relativeParts(pathOf(args[0]), pathOf(args[1]))
			return Bool(ok), nil
		}},
	},
	"relative_to": {
		{Params: []Type{TPath, TPath}, Ret: TPath, Fn: func(args []Value) (Value, error) {
			p := pathOf(args[0])
			remaining, ok := relativeParts(p, pathOf(args[1]))
			if !ok {
				return Value{}, errNotRelative
			}
			return boundedPath(pathFromParts(remaining, p.flavor).String(), p.flavor)
		}},
	},
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

// windowsSeparatorChars are the characters parseWindows treats as
// interchangeable separators — it normalizes "/" to "\" via
// strings.ReplaceAll before splitting on "\", so both count. Extracted here
// as the one definition, so argument validation can check the SAME set
// parseWindows already encodes rather than hand-writing a second one that
// could drift out of sync with it.
const windowsSeparatorChars = `/\`

// pathSeparators reports which characters separate components for p's
// shape: "/" for POSIX and for a URI — POSIX parsing and splitURI's own
// component split both use only "/" — or windowsSeparatorChars for a
// non-URI Windows-flavor path, matching parseWindows's own normalization.
// Measured against the reference (fix round 1): it is POSIX-only for these
// functions and accepts a backslash outright, including on a
// drive-looking receiver like "C:/a/b" — POSIX parsing never treats ':'
// specially, so the drive does not switch the flavor. A URI stays
// "/"-only even when the evaluator's chosen flavor is Windows, because
// parsePath's isURI branch never reaches parseWindows in the first place;
// validation must not either.
func pathSeparators(p parsedPath) string {
	if !p.isURI && p.flavor == PathWindows {
		return windowsSeparatorChars
	}
	return "/"
}

// relativeParts backs relative_to and is_relative_to. Both compare parts()
// element-wise rather than reasoning about root/comps separately, which is
// what makes a URI fall out for free: its scheme+authority is just the first
// element of parts(), so a prefix match over parts is already a prefix match
// over the full URI without any URI-specific code here.
//
// ok is true when other's parts are a full prefix of p's; remaining is
// whatever is left of p's parts after that prefix, valid only when ok.
func relativeParts(p, other parsedPath) (remaining []string, ok bool) {
	pp, op := p.parts(), other.parts()
	if len(op) > len(pp) {
		return nil, false
	}
	for i, part := range op {
		if pp[i] != part {
			return nil, false
		}
	}
	return pp[len(op):], true
}
