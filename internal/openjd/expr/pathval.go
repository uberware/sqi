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
// Extended-length ("\\?\") and device paths are deliberately not handled: an
// input beginning with "\\?\" or "\\.\" is parsed as an ordinary UNC root
// (server "?" or "."), not specially recognized — except that splitRootWindows
// happens to inherit pathlib's own refusal to synthesize a root for one,
// since that refusal is baked into the same four-segment check used for a
// genuine UNC share (see splitRootWindows). There is no doc.go entry for the
// extended-length/device-path omission yet — that lands in this wave's
// documentation task — so this comment is the omission's only record for now.
func parseWindows(text string) parsedPath {
	p := parsedPath{flavor: PathWindows}
	s := strings.ReplaceAll(text, "/", `\`)
	drive, root, rest := splitRootWindows(s)
	p.root = drive + root
	for c := range strings.SplitSeq(rest, `\`) {
		if c == "" || c == "." {
			continue
		}
		p.comps = append(p.comps, c)
	}
	return p
}

// splitRootWindows is a direct port of two pieces of CPython's OWN parsing,
// read from the running interpreter's installed pathlib source
// (ntpath.splitroot, and the root-synthesis heuristic in
// PurePath._parse_path) rather than reverse-engineered from output alone —
// hand-tracing the fix-round corpus surfaced shapes (a share-less UNC
// wrapped in extra backslashes, the "\\.\" device prefix) where guessing had
// already gone wrong once. See task-3-report.md's fix-round section for the
// exact source excerpts this was read from.
//
// It returns drive+root split apart, matching splitroot's own three-way
// return; parseWindows recombines them into the single anchor parsedPath.root
// stores. rest is everything after the root, still to be split into comps.
func splitRootWindows(p string) (drive, root, rest string) {
	switch {
	case strings.HasPrefix(p, `\\`):
		// UNC or device drive, e.g. "\\server\share" or "\\.\device". Find
		// the separator ending the server, then the one ending the share.
		idx := strings.IndexByte(p[2:], '\\')
		if idx == -1 {
			return synthesizeUNCRoot(p)
		}
		idx += 2
		idx2 := strings.IndexByte(p[idx+1:], '\\')
		if idx2 == -1 {
			return synthesizeUNCRoot(p)
		}
		idx2 += idx + 1
		return p[:idx2], p[idx2 : idx2+1], p[idx2+1:]
	case strings.HasPrefix(p, `\`):
		// Relative path with root, e.g. "\Windows" — no drive, no UNC.
		return "", p[:1], p[1:]
	case hasDriveColon(p):
		if len(p) > 2 && p[2] == '\\' {
			return p[:2], p[2:3], p[3:]
		}
		return p[:2], "", p[2:]
	default:
		return "", "", p
	}
}

// synthesizeUNCRoot handles the two "\\..." shapes where the input has no
// separator that cleanly closes off a share: no separator at all after the
// server ("\\srv"), or one trailing separator and nothing past it ("\\srv\").
// In BOTH cases the whole input p already IS the correct drive verbatim, with
// no root — UNLESS p represents a genuine "\\server\share" pair with no
// separator typed after the share (e.g. "\\srv\share"), in which case pathlib
// still credits it with a root ("\\srv\share\") despite the missing
// separator, precisely because a real share was present.
//
// A drive already ending in "\" (e.g. "\\srv\", the bare-trailing-separator
// case) is excluded up front — that separator already accounts for
// everything pathlib would synthesize, and synthesizing a second one is
// exactly fix-round IMPORTANT 2's fabricated-separator bug.
//
// segs[2] excludes "?" and "." (the "\\?\" extended-length and "\\.\" device
// prefixes — both deliberately unhandled, see parseWindows's doc comment) and
// "" (an EMPTY server, which arises from extra collapsed leading backslashes
// like "\\\a" — not a real share pair either). Only a genuine, non-reserved,
// non-empty server name earns the synthesized root.
func synthesizeUNCRoot(p string) (drive, root, rest string) {
	if !strings.HasSuffix(p, `\`) {
		segs := strings.Split(p, `\`)
		if len(segs) == 4 && segs[2] != "?" && segs[2] != "." && segs[2] != "" {
			return p, `\`, ""
		}
	}
	return p, "", ""
}

// hasDriveColon reports whether s begins with a Windows drive specifier: ANY
// single byte followed by ':'. Python's ntpath.splitdrive (which
// PureWindowsPath uses under the hood) is not restricted to ASCII letters —
// PureWindowsPath("1:\\a"), PureWindowsPath("::\\a") and even
// PureWindowsPath(" :\\a") are all drive-rooted, so a letters-only check
// would silently misclassify those as ordinary relative paths.
//
// This predicate is Windows-drive-specific ONLY. It must NOT be reused to
// detect a URI scheme (Task 4): the specification's own grammar requires a
// URI scheme to start with a LETTER, which is a stricter and unrelated rule.
// As of this task the two checks share no code — keep it that way rather
// than widening a future scheme check by accident.
func hasDriveColon(s string) bool {
	return len(s) >= 2 && s[1] == ':'
}

// isAbsolute reports the specification's p.is_absolute().
//
// A URI is ALWAYS absolute, which the spec states outright. On Windows the
// rule is NOT uniform across anchor shapes:
//   - A UNC root ("\\server...") is absolute unconditionally — Python treats
//     any "\\server" root as absolute whether or not a share or trailing
//     separator follows (PureWindowsPath("\\srv").is_absolute() is True).
//   - A drive root is absolute only WITH a trailing separator ("C:\", not
//     "C:") — a bare drive is relative to that drive's current directory.
//   - The rooted-relative anchor ("\" alone, no drive and no UNC) is never
//     absolute.
//
// The UNC and drive cases are told apart by the root's own shape (a UNC root
// always starts with "\\"; a drive root always contains ":"), so no extra
// field is needed to remember which branch of parseWindows produced it.
func (p parsedPath) isAbsolute() bool {
	switch {
	case p.isURI:
		return true
	case p.flavor == PathWindows:
		switch {
		case strings.HasPrefix(p.root, `\\`):
			return true
		case strings.Contains(p.root, ":"):
			return strings.HasSuffix(p.root, `\`)
		default:
			return false
		}
	default:
		return p.root == "/"
	}
}
