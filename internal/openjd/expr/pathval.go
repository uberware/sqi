// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "strings"

// parsedPath is a path split into a root and its components.
//
// THIS TYPE EXISTS BECAUSE GO'S STANDARD LIBRARY HAS NO PURE PATH. path/filepath
// follows the machine it runs on, which is wrong here — the specification makes
// the flavor an evaluator SETTING, and sqi parses templates server-side, so
// deriving it from the host would let one template expand differently depending
// on who submitted it. The "path" package is POSIX-only. Neither implements
// PureWindowsPath, and neither preserves the opacity a URI needs.
//
// root is "" for a relative path, "/" for a POSIX absolute one, and the drive
// or UNC anchor for Windows. comps are the components after the root.
type parsedPath struct {
	root   string
	comps  []string
	flavor PathFormat
	isURI  bool //nolint:unused // set and read starting Task 4 (URI flavor); part of this task's fixed struct shape so later tasks don't reshape it
}

// parsePath splits text into a root and components under the given flavor.
//
// Normalization is Python's, and it is deliberately not uniform: consecutive
// separators collapse, "." segments are dropped, a trailing separator is
// dropped, and ".." is KEPT because resolving it without touching the
// filesystem is wrong in the presence of symlinks. The empty string is ".".
func parsePath(text string, f PathFormat) parsedPath {
	f = f.resolve()
	p := parsedPath{flavor: f}
	rest := text
	// Task 3 inserts the Windows root split here; Task 4 inserts the URI
	// branch ahead of both.
	if strings.HasPrefix(rest, "/") {
		// POSIX.1-2017 §4.13: exactly two leading slashes is
		// implementation-defined and CPython's PurePosixPath keeps it as its
		// own root ("//"); three or more collapses to a single "/", same as
		// one. Collapsing all leading slashes uniformly (as a naive TrimLeft
		// would) is wrong for the exactly-two case.
		trimmed := strings.TrimLeft(rest, "/")
		leading := len(rest) - len(trimmed)
		if leading == 2 {
			p.root = "//"
		} else {
			p.root = "/"
		}
		rest = trimmed
	}
	for c := range strings.SplitSeq(rest, "/") {
		if c == "" || c == "." {
			continue
		}
		p.comps = append(p.comps, c)
	}
	return p
}

// String renders the path back to text.
func (p parsedPath) String() string {
	if p.root == "" && len(p.comps) == 0 {
		return "."
	}
	return p.root + strings.Join(p.comps, "/")
}

// parts is the specification's p.parts: the root, when there is one, followed
// by the components. A relative path with no components yields an empty list,
// matching PurePosixPath(".").parts.
func (p parsedPath) parts() []string {
	if p.root == "" {
		return append([]string{}, p.comps...)
	}
	return append([]string{p.root}, p.comps...)
}
