// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "strings"

// pathFuncs is sub-project C4's group: RFC 0006's path properties and path
// functions. apply_path_mapping is NOT here — it is sub-project D's, because it
// needs the session's mapping rules, which this package has no access to.
var pathFuncs = map[string][]Shape{
	// path() is one of exactly two rows in the whole registry that use FnCtx
	// rather than Fn. Constructing a path is the only operation that has to
	// CHOOSE a flavor; every other path operation reads it off its receiver.
	"path": {
		{Params: []Type{TString}, Ret: TPath, FnCtx: func(ec evalCtx, args []Value) (Value, error) {
			return boundedPath(args[0].AsStr(), ec.pathFormat)
		}},
		// The list row is what makes the specification's stated roundtrip
		// path(p.parts) == p work: parts puts the root first, and joining them
		// with the flavor's separator reproduces the original.
		{Params: []Type{ListOf(TString)}, Ret: TPath, FnCtx: func(ec evalCtx, args []Value) (Value, error) {
			elems := args[0].AsList()
			parts := make([]string, len(elems))
			for i, e := range elems {
				parts[i] = e.AsStr()
			}
			return boundedPath(joinParts(parts, ec.pathFormat), ec.pathFormat)
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

// joinParts reassembles the output of parts() into text.
//
// The first element may be a root ("/", "C:\", "\\srv\share\", "s3://bucket"),
// which already carries its own trailing separator or needs none, so it is
// concatenated rather than joined.
func joinParts(parts []string, f PathFormat) string {
	if len(parts) == 0 {
		return ""
	}
	sep := "/"
	if f.resolve() == PathWindows {
		sep = `\`
	}
	head, tail := parts[0], parts[1:]
	if len(tail) == 0 {
		return head
	}
	if strings.HasSuffix(head, "/") || strings.HasSuffix(head, `\`) {
		return head + strings.Join(tail, sep)
	}
	return head + sep + strings.Join(tail, sep)
}
