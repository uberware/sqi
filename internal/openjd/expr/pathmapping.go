// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"sort"
	"strings"
)

// PathMapSourceFormat is the format of a path-mapping rule's SourcePath, the
// OpenJD pathmapping-1.0 "source_path_format" enum (URI is the EXPR extension's
// addition).
type PathMapSourceFormat int

const (
	// PathMapPOSIX matches SourcePath using POSIX path semantics.
	PathMapPOSIX PathMapSourceFormat = iota
	// PathMapWindows matches SourcePath using Windows path semantics
	// (case-insensitive, separator-insensitive — see applyFileRule).
	PathMapWindows
	// PathMapURI matches SourcePath by string prefix on a "/" path-segment
	// boundary, with no path normalization — see applyURIRule.
	PathMapURI
)

// PathMapRule is one source→destination path-mapping rule. It mirrors the
// OpenJD pathmapping-1.0 schema but is independent of internal/worker/protocol,
// which this leaf package cannot import; sub-project E translates a
// protocol.PathMapRule into this type at the injection boundary.
type PathMapRule struct {
	SourceFormat    PathMapSourceFormat
	SourcePath      string
	DestinationPath string
}

// mapPath applies rules to s and returns the mapped path text, or s unchanged
// when no rule matches (passthrough). dst is the flavor destinations and the
// mapped result are expressed in — the evaluation's pathFormat, which is the
// worker's native flavor in the host context apply_path_mapping runs in.
//
// This is the OpenJD path-mapping algorithm (wiki How-Jobs-Are-Run §Path
// Mapping): rules are tried in order of DECREASING SourcePath length, and the
// FIRST match wins and stops the scan. It is deliberately NOT the substring
// semantics of internal/worker/pathmap.Apply, which serves a different job
// (swapping paths into command strings) and which this leaf cannot import.
func mapPath(s string, rules []PathMapRule, dst PathFormat) string {
	ordered := make([]PathMapRule, 0, len(rules))
	for _, r := range rules {
		if r.SourcePath != "" {
			ordered = append(ordered, r)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return len(ordered[i].SourcePath) > len(ordered[j].SourcePath)
	})
	for _, r := range ordered {
		if mapped, ok := applyRule(s, r, dst); ok {
			return mapped
		}
	}
	return s
}

func applyRule(s string, r PathMapRule, dst PathFormat) (string, bool) {
	if r.SourceFormat == PathMapURI {
		return applyURIRule(s, r)
	}
	return applyFileRule(s, r, dst)
}

// applyFileRule matches a POSIX or WINDOWS source against s on component
// boundaries and, on a match, joins s's remainder components onto the
// destination in the dst flavor.
//
// The matching itself is relativeParts (funcspath.go), the SAME function
// is_relative_to/relative_to use to answer the identical question ("are
// other's parts a full prefix of p's, and what remains?"), rather than a
// second hand-written prefix loop beside it — precisely the "two formulas
// that happen to agree today" duplication this package's own doc comments
// (pathval.go's String(), pathJoin) name as its signature defect class. The
// only thing this caller varies is the comparator: relativeParts takes an eq
// function, and this passes strings.EqualFold for a WINDOWS source (C4's
// Windows parser already accepts both separators, so separator-insensitivity
// falls out of parsePath itself) and byteEqual otherwise. This is
// deliberately distinct from C4's byte-exact, case-SENSITIVE path equality
// (==) that relativeParts' OTHER two callers always use: source MATCHING and
// path EQUALITY are different operations, and eq is the one place that
// difference is expressed — relativeParts' other rules (the anchorless-other
// guard, the length check, the boundary trimming) are shared unchanged.
func applyFileRule(s string, r PathMapRule, dst PathFormat) (string, bool) {
	srcFlavor := PathPOSIX
	eq := byteEqual
	if r.SourceFormat == PathMapWindows {
		srcFlavor = PathWindows
		eq = strings.EqualFold
	}
	remainder, ok := relativeParts(parsePath(s, srcFlavor), parsePath(r.SourcePath, srcFlavor), eq)
	if !ok {
		return "", false
	}
	// parsedPath.parts() already returns a freshly built slice (it appends
	// into a []string literal rather than slicing p.comps directly), so its
	// backing array is not shared with the parsedPath it came from. But
	// relying on that invariant here — appending remainder straight onto the
	// value parts() hands back — would tie this call's safety to an
	// implementation detail of a function it does not control, which is the
	// exact shape of aliasing defect C4 had to close repeatedly in this
	// package (pathJoin's doc comment above catalogs three of them). Building
	// a fresh slice explicitly keeps this call correct regardless of how
	// parts() is implemented.
	dstParts := parsePath(r.DestinationPath, dst).parts()
	resultParts := make([]string, 0, len(dstParts)+len(remainder))
	resultParts = append(resultParts, dstParts...)
	resultParts = append(resultParts, remainder...)
	return pathFromParts(resultParts, dst).String(), true
}

// applyURIRule matches a URI source by string prefix on path boundaries (segment
// boundary at "/"), preserving the exact remaining URI text with no
// normalization (wiki §Path Mapping, "@extension EXPR — URI source paths").
func applyURIRule(s string, r PathMapRule) (string, bool) {
	src := strings.TrimRight(r.SourcePath, "/")
	if s == src {
		return r.DestinationPath, true
	}
	if strings.HasPrefix(s, src+"/") {
		remainder := s[len(src):] // begins with "/"
		return strings.TrimRight(r.DestinationPath, "/") + remainder, true
	}
	return "", false
}

// pathMappingFuncs is sub-project D's group: the single host-context function
// apply_path_mapping, co-located with the engine it wraps. It is a SEPARATE
// group from C4's pathFuncs because the codebase convention is that a wave adds
// its own table and never edits another's (see funcs.go's mergeFuncs).
//
// There is NO leaf-level host-context gate: the function is always resolvable
// and, with no rules, passes through. Host-context availability (the function is
// valid only in @fmtstring[host] scopes) is enforced by sub-project E, which is
// the only layer with a scope model; E uses Expression.CalledFunctions to spot
// the call. A leaf gate would also break the deliberate conformance regression
// this registration causes — see the design doc §5 and §6.
var pathMappingFuncs = map[string][]Shape{
	"apply_path_mapping": {
		{Params: []Type{TString}, Ret: TPath, FnCtx: func(ec evalCtx, args []Value) (Value, error) {
			return boundedPath(mapPath(args[0].AsStr(), ec.pathMapping, ec.pathFormat), ec.pathFormat)
		}},
	},
}
