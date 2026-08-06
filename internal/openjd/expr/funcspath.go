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
	// dot-prefixed. "png" is an error; ".png" and "" are not.
	errInvalidSuffix = errors.New("an extension must be empty or start with a dot")
	// errNotRelative backs relative_to. is_relative_to answers false for the
	// same condition rather than failing — the specification defines the pair
	// that way on purpose.
	errNotRelative = errors.New("the path is not relative to the other path")
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
	// with_name and with_stem both replace the final component and both error
	// the same way on a path that has none — "/" is the usual case — so they
	// share errEmptyName rather than each defining their own.
	"with_name": {
		{Params: []Type{TPath, TString}, Ret: TPath, Fn: func(args []Value) (Value, error) {
			p := pathOf(args[0])
			if p.name() == "" {
				return Value{}, errEmptyName
			}
			p.comps[len(p.comps)-1] = args[1].AsStr()
			return boundedPath(p.String(), p.flavor)
		}},
	},
	"with_stem": {
		{Params: []Type{TPath, TString}, Ret: TPath, Fn: func(args []Value) (Value, error) {
			p := pathOf(args[0])
			if p.name() == "" {
				return Value{}, errEmptyName
			}
			_, suffix := splitStemSuffix(p.name())
			p.comps[len(p.comps)-1] = args[1].AsStr() + suffix
			return boundedPath(p.String(), p.flavor)
		}},
	},
	// with_suffix replaces the suffix; "" removes it, and anything else that
	// does not start with a dot is errInvalidSuffix. It also errors on an
	// empty name for the same reason with_name and with_stem do — there is no
	// final component to carry the new suffix — which the brief's error list
	// does not spell out by name but which follows from reusing name() the
	// same way its siblings do: without the guard, replacing the (absent)
	// last component would index an empty comps slice.
	"with_suffix": {
		{Params: []Type{TPath, TString}, Ret: TPath, Fn: func(args []Value) (Value, error) {
			suffix := args[1].AsStr()
			if suffix != "" && !strings.HasPrefix(suffix, ".") {
				return Value{}, errInvalidSuffix
			}
			p := pathOf(args[0])
			if p.name() == "" {
				return Value{}, errEmptyName
			}
			stem, _ := splitStemSuffix(p.name())
			p.comps[len(p.comps)-1] = stem + suffix
			return boundedPath(p.String(), p.flavor)
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
