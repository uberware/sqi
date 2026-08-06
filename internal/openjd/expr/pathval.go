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
	isURI  bool // read by isAbsolute since Task 3; set starting Task 4 (URI flavor)
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
	// Task 4 inserts the URI branch ahead of both.
	if f == PathWindows {
		return parseWindows(text)
	}
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
	sep := "/"
	if p.flavor == PathWindows {
		sep = `\`
	}
	return p.root + strings.Join(p.comps, sep)
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

// parseWindows splits a Windows path into its anchor and components.
//
// Three anchor shapes exist and they are NOT interchangeable:
//   - "C:\" — a drive with a root. ABSOLUTE.
//   - "C:"  — a drive WITHOUT a root, i.e. relative to that drive's current
//     directory. NOT absolute, and the anchor carries no separator. Python
//     reports PureWindowsPath("C:a").parts as ("C:", "a").
//   - "\\srv\share\" — a UNC root, which the specification says is preserved
//     as-is. The whole server+share is ONE component.
//
// Both separators are accepted on input and "\" is emitted, matching pathlib.
// Extended-length ("\\?\") and device paths are deliberately not handled; see
// doc.go's omissions.
func parseWindows(text string) parsedPath {
	p := parsedPath{flavor: PathWindows}
	s := strings.ReplaceAll(text, "/", `\`)
	switch {
	case strings.HasPrefix(s, `\\`):
		// UNC: consume "\\server\share" plus one trailing separator.
		rest := s[2:]
		srv, after, ok := strings.Cut(rest, `\`)
		if !ok {
			p.root = `\\` + rest
			return p
		}
		share, tail, hadTail := strings.Cut(after, `\`)
		p.root = `\\` + srv + `\` + share + `\`
		if hadTail {
			s = tail
		} else {
			s = ""
		}
	case len(s) >= 2 && s[1] == ':' && isDriveLetter(s[0]):
		if len(s) > 2 && s[2] == '\\' {
			p.root = s[:2] + `\`
			s = s[3:]
		} else {
			p.root = s[:2]
			s = s[2:]
		}
	case strings.HasPrefix(s, `\`):
		p.root = `\`
		s = s[1:]
	}
	for c := range strings.SplitSeq(s, `\`) {
		if c == "" || c == "." {
			continue
		}
		p.comps = append(p.comps, c)
	}
	return p
}

func isDriveLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// isAbsolute reports the specification's p.is_absolute().
//
// A URI is ALWAYS absolute, which the spec states outright. On Windows a drive
// alone ("C:") is NOT absolute — only a drive WITH a root is, which is why the
// test is on the trailing separator and not merely on the presence of a drive.
func (p parsedPath) isAbsolute() bool {
	switch {
	case p.isURI:
		return true
	case p.flavor == PathWindows:
		return strings.HasSuffix(p.root, `\`) && p.root != `\`
	default:
		return p.root == "/"
	}
}
