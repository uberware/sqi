// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"slices"
	"strings"
	"unicode/utf8"
)

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

// pathSeparatorChars is the set of characters that separate path components
// under flavor f: "/" alone for POSIX, or "/" and "\" together for Windows.
// This is the ONE place "what is a separator" is answered for a flavor —
// parsePath's POSIX branch and parseWindows below both consult it (via
// pathCanonicalSeparator and normalizeSeparators) to find component
// boundaries, and funcspath.go's isValidReplacementName consults it
// directly to reject a with_name/with_stem/with_suffix replacement that
// contains one. Parser and validator sharing this definition matters
// because they must never independently drift the way an earlier fix
// round's joinParts (a second copy of String()'s join rule) and
// splitStemSuffix (a second copy of suffixes()'s leading-dot handling) did.
//
// A URI's body is always "/"-only regardless of flavor — splitURI never
// calls this function at all, it hard-codes "/" for its own component
// split below — so a caller that must handle both shapes
// (isValidReplacementName) checks p.isURI itself before consulting this.
func pathSeparatorChars(f PathFormat) string {
	if f.resolve() == PathWindows {
		return `/\`
	}
	return "/"
}

// pathCanonicalSeparator is the ONE character parseWindows normalizes every
// OTHER character in pathSeparatorChars(f) to, before splitting on it: "\"
// for Windows. For POSIX it is "/", the only separator POSIX has, so
// normalizing to it is a no-op — parsePath's POSIX branch still splits on
// this value rather than a second hard-coded "/", so both flavors' splits
// read the same source.
func pathCanonicalSeparator(f PathFormat) byte {
	if f.resolve() == PathWindows {
		return '\\'
	}
	return '/'
}

// normalizeSeparators rewrites every character in pathSeparatorChars(f) to
// f's canonical separator, so a downstream split on that ONE character finds
// every component boundary regardless of which separator character produced
// it. parseWindows is the caller that actually needs this — it accepts "/"
// on input but represents an anchor, and renders output, using "\" alone;
// for POSIX it is a no-op, since POSIX has only one separator to begin with.
func normalizeSeparators(text string, f PathFormat) string {
	seps := pathSeparatorChars(f)
	canon := rune(pathCanonicalSeparator(f))
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(seps, r) {
			return canon
		}
		return r
	}, text)
}

// parsePath splits text into a root and components under the given flavor.
//
// Normalization is Python's, and it is deliberately not uniform: consecutive
// separators collapse, "." segments are dropped, a trailing separator is
// dropped, and ".." is KEPT because resolving it without touching the
// filesystem is wrong in the presence of symlinks. The empty string is ".".
func parsePath(text string, f PathFormat) parsedPath {
	f = f.resolve()
	if root, rest, ok := splitURI(text); ok {
		p := parsedPath{root: root, isURI: true, flavor: f}
		if rest != "" {
			p.comps = strings.Split(rest, "/")
		}
		return p
	}
	p := parsedPath{flavor: f}
	rest := text
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
	for c := range strings.SplitSeq(rest, string(pathCanonicalSeparator(f))) {
		if c == "" || c == "." {
			continue
		}
		p.comps = append(p.comps, c)
	}
	return p
}

// String renders the path back to text, porting pathlib's own
// _format_parsed_parts exactly (drv/root here are already combined into
// p.root, so "drv or root" below reads as "p.root != \"\""):
//
//	if drv or root:
//	    return drv + root + sep.join(tail)
//	elif tail and splitdrive(tail[0])[0]:
//	    return '.' + sep + sep.join(tail)
//	else:
//	    return sep.join(tail) or '.'
//
// The middle branch is the one worth calling out: a relative path (no root
// at all) whose FIRST component itself looks like a drive specifier — e.g.
// comps == ["a:b"], which parsePath produces from the text ".\a:b" once the
// leading "." is normalized away — gets a "." PREPENDED on render, so the
// text round-trips as ".\a:b" rather than the bare "a:b" a naive join would
// produce. That distinction is not decorative: re-parsing "a:b" itself
// yields drive "a:" plus component "b" — a DIFFERENT path — so omitting the
// "." silently corrupts the one case where doing so changes meaning. Only
// tail[0] (not any later component) is tested, matching the reference: a
// LATER colon-bearing component never needs disambiguating, since it can't
// be mistaken for a leading drive once it isn't in the leading position.
func (p parsedPath) String() string {
	if p.isURI {
		if len(p.comps) == 0 {
			return p.root
		}
		return p.root + "/" + strings.Join(p.comps, "/")
	}
	sep := "/"
	if p.flavor == PathWindows {
		sep = `\`
	}
	switch {
	case p.root != "":
		return p.root + strings.Join(p.comps, sep)
	case p.flavor == PathWindows && len(p.comps) > 0 && driveColonLen(p.comps[0]) > 0:
		return "." + sep + strings.Join(p.comps, sep)
	default:
		if len(p.comps) == 0 {
			return "."
		}
		return strings.Join(p.comps, sep)
	}
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

// name is the final component, or "" for a path that has only a root.
func (p parsedPath) name() string {
	if len(p.comps) == 0 {
		return ""
	}
	return p.comps[len(p.comps)-1]
}

// splitLeadingDots is the ONE place stem, suffix and suffixes agree on where
// a name's leading run of dots ends, so they cannot independently drift out
// of sync the way splitStemSuffix and suffixes() did before fix-round 1 (see
// splitStemSuffix's doc comment for what that cost).
//
// The general pathlib rule, stated precisely because a narrower reading of
// it is exactly what shipped broken: strip the ENTIRE leading run of dots
// first — not just a single leading dot — and only then split what remains
// on "."; a name whose only dots are that leading run has NO further pieces
// and therefore no suffix at all, however long the run is. leading is
// returned separately so a caller can re-prepend it to the stem; pieces is
// nil when nothing follows the leading run (including when name is entirely
// dots, or empty).
func splitLeadingDots(name string) (leading string, pieces []string) {
	trimmed := strings.TrimLeft(name, ".")
	leading = name[:len(name)-len(trimmed)]
	if trimmed == "" {
		return leading, nil
	}
	return leading, strings.Split(trimmed, ".")
}

// splitStemSuffix divides a final component at its LAST dot, following
// pathlib.
//
// Two rules make this less obvious than it looks, and both were measured
// against Python:
//   - a LEADING dot is not a suffix separator, so ".hidden" is all stem —
//     and this generalizes to any RUN of leading dots, however long
//     (splitLeadingDots), not just a single one: "..a" is all stem too,
//     which is what fix-round 1 found broken here (LastIndex over the
//     unstripped name, guarded only by i <= 0, catches a single leading dot
//     at index 0 but not a run of two or more, where the last dot sits at
//     index >= 1 and slips past the guard);
//   - a name that is entirely dots ("..") has no suffix at all.
//
// A trailing dot IS a suffix: "a." has stem "a" and suffix ".". The reference
// implementation disagrees and reports stem "a." with no suffix; RFC 0006 says
// these properties match pathlib, so this follows Python.
func splitStemSuffix(name string) (stem, suffix string) {
	leading, pieces := splitLeadingDots(name)
	if len(pieces) <= 1 {
		return name, ""
	}
	stem = leading + strings.Join(pieces[:len(pieces)-1], ".")
	suffix = "." + pieces[len(pieces)-1]
	return stem, suffix
}

// suffixes is every extension on the final component, in order.
func (p parsedPath) suffixes() []string {
	_, pieces := splitLeadingDots(p.name())
	if len(pieces) <= 1 {
		return nil
	}
	out := make([]string, 0, len(pieces)-1)
	for _, s := range pieces[1:] {
		out = append(out, "."+s)
	}
	return out
}

// anchorParts splits p's root into the drive and the root separator that
// CPython's own join reasons about separately. It is the ONE distinction
// parsedPath.root deliberately erases — it stores the two already combined,
// because every other operation in this file needs the anchor only as a
// whole — so this is the only place that has to take them apart again.
//
// Only the Windows flavor can produce a drive, and the split there is
// delegated to splitRootWindows, the existing CPython port, rather than
// re-derived beside it.
//
// A URI's scheme+authority is reported as a ROOT and never as a drive, which
// is not a detail: pathJoin lets a rootless child INHERIT its parent's drive,
// and a URI authority must never be inherited that way.
func (p parsedPath) anchorParts() (drive, root string) {
	if p.isURI || p.flavor != PathWindows {
		return "", p.root
	}
	d, r, _ := splitRootWindows(p.root)
	return d, r
}

// pathJoin combines a parent with a child: RFC 0006 section 2.1.5's "/",
// applied at the structural level. The result is BUILT out of the two parsed
// paths and rendered by String(), which is the single formula that already
// knows how each root shape joins its components — a bare Windows drive takes
// no separator after it, a URI always uses "/" whatever the flavor says.
// Nothing here concatenates text or picks a separator; doing either is how
// this wave's earlier joinParts broke bare drives.
//
// The rule is CPython's own ntpath.join/posixpath.join, restated over the
// parsed shape rather than over strings:
//
//   - a child carrying a root of its own anchors the result, inheriting the
//     parent's drive when it has none — so "C:/a" / "/x" is "C:\x". For POSIX
//     and URI, which have no drives at all, this IS section 2.1.5's "if the
//     right operand is an absolute path, it replaces the left operand
//     entirely";
//   - a child with a DIFFERENT drive and no root discards the parent outright
//     ("C:/a" / "D:b" is "D:b"). The SAME drive spelled in another case keeps
//     the child's spelling and then appends, which is ntpath.join's own
//     behavior ("C:/a" / "c:b" is "c:\a\b");
//   - anything else is an ordinary relative child: its components are
//     appended.
//
// isAbsolute() is deliberately NOT the test, even though section 2.1.5 words
// the rule that way. Under Windows a child can anchor the result without
// being absolute — "/x" and "D:b" are both is_absolute() == false — so a rule
// keyed on that predicate silently DROPS their anchor and answers "C:\a\x"
// and "C:\a\b". The reference implementation's path family is POSIX-only and
// cannot adjudicate any of this; the Windows expectations are CPython's, and
// are pinned by TestPathOperators_Windows.
func pathJoin(parent, child parsedPath) parsedPath {
	// A URI child replaces the parent whole. It is handled before the anchor
	// split so that its authority can never be treated as a root the parent's
	// drive gets prepended to.
	if child.isURI {
		return child
	}
	pDrive, pRoot := parent.anchorParts()
	cDrive, cRoot := child.anchorParts()
	switch {
	case cRoot != "":
		if cDrive == "" {
			cDrive = pDrive
		}
		return parsedPath{root: cDrive + cRoot, comps: child.comps, flavor: parent.flavor}
	case cDrive != "" && !strings.EqualFold(cDrive, pDrive):
		return child
	case cDrive != "":
		pDrive = cDrive
	}
	return parsedPath{
		root:   pDrive + pRoot,
		comps:  slices.Concat(trimTrailingEmptyComps(parent.comps), child.comps),
		flavor: parent.flavor,
		isURI:  parent.isURI,
	}
}

// trimTrailingEmptyComps drops the empty components a trailing separator
// leaves behind, which is section 2.1.5's "a trailing slash on the left
// operand is consumed by the join".
//
// Only a URI ever has one to drop: both filesystem flavors discard a trailing
// separator while parsing, so their component lists never end in "". The
// whole trailing RUN goes rather than a single component, matching the
// reference implementation, which answers "s3://b/d///" / "f" with
// "s3://b/d/f" while keeping an INTERIOR run intact ("s3://b/d//x" is
// unchanged by parsing and by joining).
func trimTrailingEmptyComps(comps []string) []string {
	end := len(comps)
	for end > 0 && comps[end-1] == "" {
		end--
	}
	return comps[:end]
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
//
// Extended-length ("\\?\") and device paths are DELIBERATELY not handled,
// and this paragraph says plainly what that means rather than gesturing at
// it. pathlib gives the specific literal prefix "\\?\UNC\" its own
// start-at-offset-8 parsing (splitting a FURTHER server+share out of what
// follows), so "\\?\UNC\srv\share" is ONE opaque root
// ("\\?\UNC\srv\share\") to Python. We do not implement offset-8 parsing at
// all, so that exact input instead runs through the ordinary offset-2 UNC
// algorithm below and comes out server="?", share="UNC", with "srv" and
// "share" demoted to ordinary path COMPONENTS — a different split, not
// merely a different rendering. This is the one extended-length/device shape
// known to diverge; it is not implemented on purpose, and doing so would
// require porting the offset-8/six-segment branches CPython carries
// alongside the offset-2 ones this file already ports.
//
// A plain "\\?\" or "\\.\" WITHOUT the literal "UNC\" that follows never hits
// Python's offset-8 branch either — Python's own splitroot only special-cases
// the exact "\\?\UNC\" prefix, so "\\?\a" and "\\.\a" already run through the
// SAME offset-2 algorithm on the reference side that this file ports, and the
// two sides agree on them without any device-path-specific code here (see
// synthesizeUNCRoot's non-empty/non-"?"/non-"."/non-"?." exclusion, which is
// the reference's OWN generic rule, not something this file added for
// devices specifically). There is no doc.go entry for the "\\?\UNC\" omission
// yet — that lands in this wave's documentation task — so this paragraph is
// its only record for now.
func parseWindows(text string) parsedPath {
	p := parsedPath{flavor: PathWindows}
	s := normalizeSeparators(text, PathWindows)
	drive, root, rest := splitRootWindows(s)
	p.root = drive + root
	for c := range strings.SplitSeq(rest, string(pathCanonicalSeparator(PathWindows))) {
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
	case driveColonLen(p) > 0:
		n := driveColonLen(p)
		if len(p) > n && p[n] == '\\' {
			return p[:n], p[n : n+1], p[n+1:]
		}
		return p[:n], "", p[n:]
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
// segs[2] is excluded when it is "?" or "." (the "\\?\" extended-length and
// "\\.\" device prefixes — both deliberately unhandled, see parseWindows's
// doc comment) or "" (an EMPTY server, which arises from extra collapsed
// leading backslashes like "\\\a" — not a real share pair either). CPython's
// own exclusion is `drv_parts[2] not in '?.'`, a SUBSTRING test against the
// two-character string "?.", not per-character membership — and a substring
// test admits FOUR values, not three: "", "?", "." AND "?." itself (every
// substring "?." has, including itself). Three explicit inequalities
// (!= "?" && != "." && != "") is a plausible-looking but WRONG translation:
// it lets segs[2] == "?." itself through, which is exactly backwards — the
// two-character server name "?." (e.g. "\\?.\share") must stay excluded,
// same as "?" and "." alone. strings.Contains reproduces the substring test
// directly instead of re-deriving its member list by hand.
func synthesizeUNCRoot(p string) (drive, root, rest string) {
	if !strings.HasSuffix(p, `\`) {
		segs := strings.Split(p, `\`)
		if len(segs) == 4 && !strings.Contains("?.", segs[2]) { //nolint:gocritic // args are intentionally reversed: "?." is CPython's own literal exclusion set, segs[2] is what's tested against it — see the doc comment above
			return p, `\`, ""
		}
	}
	return p, "", ""
}

// driveColonLen reports the BYTE length of a Windows drive specifier prefix
// at the start of s — ANY single Unicode code point followed by ':' — or 0
// if s does not begin with one. Python's ntpath.splitdrive (which
// PureWindowsPath uses under the hood) is not restricted to ASCII letters —
// PureWindowsPath("1:\\a"), PureWindowsPath("::\\a") and even
// PureWindowsPath(" :\\a") are all drive-rooted, so a letters-only check
// would silently misclassify those as ordinary relative paths.
//
// The return value is a BYTE count, not a fixed 2, because the leading code
// point can be multi-byte in UTF-8: "é:\a" is drive-rooted to Python
// (PureWindowsPath("é:\\a").is_absolute() is True), but 'é' alone is 2 bytes,
// so indexing s[1] (the second BYTE) lands on the ':' 's ONE byte too early —
// on the continuation byte of 'é' — and never finds the colon at all. There
// is no risk of a false match in the other direction: ':' (0x3A) is outside
// the 0x80-0xBF continuation-byte range, so it can never be mistaken for
// part of a multi-byte rune; the hazard is purely the false NEGATIVE from
// checking the wrong byte position, not a false positive from checking the
// right one.
//
// This predicate is Windows-drive-specific ONLY. It must NOT be reused to
// detect a URI scheme (Task 4): the specification's own grammar requires a
// URI scheme to start with a LETTER, which is a stricter and unrelated rule.
// As of this task the two checks share no code — keep it that way rather
// than widening a future scheme check by accident.
func driveColonLen(s string) int {
	if s == "" {
		return 0
	}
	r, n := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && n <= 1 {
		return 0
	}
	if len(s) <= n || s[n] != ':' {
		return 0
	}
	return n + 1
}

// splitURI recognizes the specification's URI form and splits off the opaque
// scheme+authority prefix.
//
// The pattern is the spec's own: ^[a-zA-Z][a-zA-Z0-9+.-]*://
//
// Everything after the authority is kept VERBATIM. Consecutive slashes, "."
// and ".." segments and a trailing slash all survive, because a URI path
// component is an opaque identifier — "a//b" and "a/b" may name different
// objects in a store. This is the exact opposite of the filesystem flavors,
// and getting it wrong is silent, which is why the URI tests pin each case
// that a filesystem path would normalize away.
//
// This deliberately does NOT reuse driveColonLen: that predicate accepts ANY
// code point before ':' (matching ntpath.splitdrive), but a URI scheme must
// start with an ASCII LETTER per the spec's own grammar — a stricter and
// unrelated rule that gets its own predicate below.
func splitURI(text string) (root, rest string, ok bool) {
	if text == "" || !isSchemeStart(text[0]) {
		return "", "", false
	}
	i := 1
	for i < len(text) && isSchemeByte(text[i]) {
		i++
	}
	if !strings.HasPrefix(text[i:], "://") {
		return "", "", false
	}
	authStart := i + 3
	rel := strings.Index(text[authStart:], "/")
	if rel < 0 {
		return text, "", true
	}
	return text[:authStart+rel], text[authStart+rel+1:], true
}

// isSchemeStart reports whether b may begin a URI scheme: an ASCII letter,
// per ^[a-zA-Z] in the specification's own regex. Unlike driveColonLen, this
// is intentionally NOT Unicode-aware and NOT digit/symbol-permissive — a
// scheme's first character is strictly narrower than the rest of it.
func isSchemeStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// isSchemeByte reports whether b may appear after the first character of a
// URI scheme: [a-zA-Z0-9+.-], matching the specification's regex tail.
func isSchemeByte(b byte) bool {
	return isSchemeStart(b) || (b >= '0' && b <= '9') || b == '+' || b == '.' || b == '-'
}

// isAbsolute reports the specification's p.is_absolute().
//
// A URI is ALWAYS absolute, which the spec states outright. For POSIX, a raw
// leading "/" is absolute, matching pathlib's own POSIX fast path (it tests
// the raw text directly rather than going through isabs, as an optimization
// pathlib documents in its own source — the result is identical either way).
//
// For Windows the rule is PurePath.is_absolute() -> self.parser.isabs(self),
// where "self" is coerced to its RENDERED string (str(self), i.e. what our
// String() produces) before ntpath.isabs ever runs. This is NOT a test of
// the parsed root/drive's SHAPE — an earlier version of this function tried
// to derive the rule from root shape (UNC prefix, contains ":", etc.) and
// that derivation was wrong, because the reference rule was never about
// shape. It is a literal prefix match on the first three CHARACTERS (Unicode
// code points, not bytes — same multi-byte hazard as driveColonLen) of the
// rendered string:
//
//	s = s[:3].replace('/', '\\')
//	return s.startswith(':\\', 1) or s.startswith('\\\\')
//
// This textual test does not care WHY those three characters are what they
// are. parsedPath{root: `\`, comps: [":", "a"]}.String() renders "\:\a",
// whose first three characters are '\', ':', '\' — which SATISFIES the
// colon-backslash-at-index-1 branch even though "\:\a" has no drive and no
// UNC anchor at all; Python agrees (PureWindowsPath("\\:\\a").is_absolute()
// is True). Any shape-based rule inevitably disagrees with cases like this
// one, by construction.
func (p parsedPath) isAbsolute() bool {
	switch {
	case p.isURI:
		return true
	case p.flavor == PathWindows:
		return windowsIsAbsPrefix(p.String())
	default:
		return p.root == "/"
	}
}

// windowsIsAbsPrefix is the direct port of ntpath.isabs's prefix test; see
// isAbsolute's doc comment for what it tests and why.
func windowsIsAbsPrefix(s string) bool {
	r := []rune(s)
	if len(r) > 3 {
		r = r[:3]
	}
	for i, c := range r {
		if c == '/' {
			r[i] = '\\'
		}
	}
	if len(r) >= 3 && r[1] == ':' && r[2] == '\\' {
		return true
	}
	return len(r) >= 2 && r[0] == '\\' && r[1] == '\\'
}
