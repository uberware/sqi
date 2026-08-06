// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "strings"

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
