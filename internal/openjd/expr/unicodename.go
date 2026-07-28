// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/text/unicode/runenames"
)

// cjkIdeographPrefix is the algorithmic name form Unicode assigns to CJK
// unified ideographs, which runenames does not spell out.
const cjkIdeographPrefix = "CJK UNIFIED IDEOGRAPH-"

var (
	runeNamesOnce sync.Once
	runeNames     map[string]rune
)

// unicodeByName maps a Unicode character name to its rune, for the \N{name}
// string escape (spec section 1.1.5). Lookup is case-insensitive and ignores
// surrounding whitespace.
//
// golang.org/x/text/unicode/runenames maps runes to names, so the reverse map
// is built by inverting it: about 1.1 million lookups producing 34,823
// entries and roughly 2 MB. That is done lazily, once per process, and only
// by an expression that actually uses the escape — which no conformance
// fixture does. The alternative, a generated table, would be large and is not
// warranted for an escape this rare.
//
// Runes with algorithmic rather than listed names come back from runenames as
// placeholders such as "<CJK Ideograph>" and "<Hangul Syllable>", so they are
// skipped when inverting. CJK ideographs are then resolved directly, since
// their name is just the code point in hexadecimal. Hangul syllable names are
// composed from jamo and are deliberately NOT supported: the escape reports an
// unknown name rather than resolving to something wrong.
func unicodeByName(name string) (rune, bool) {
	name = strings.ToUpper(strings.TrimSpace(name))
	if name == "" {
		return 0, false
	}
	if r, ok := cjkIdeographByName(name); ok {
		return r, true
	}
	runeNamesOnce.Do(buildRuneNames)
	r, ok := runeNames[name]
	return r, ok
}

func buildRuneNames() {
	runeNames = make(map[string]rune, 1<<16)
	for r := rune(0); r <= utf8.MaxRune; r++ {
		name := runenames.Name(r)
		if name == "" || strings.HasPrefix(name, "<") {
			continue
		}
		runeNames[name] = r
	}
}

// cjkIdeographByName resolves the "CJK UNIFIED IDEOGRAPH-<hex>" name form. The
// resulting code point is confirmed against runenames so a well-formed name
// for a code point that is not actually an ideograph is still rejected.
func cjkIdeographByName(name string) (rune, bool) {
	hex, ok := strings.CutPrefix(name, cjkIdeographPrefix)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseUint(hex, 16, 32)
	if err != nil || v > utf8.MaxRune {
		return 0, false
	}
	r := rune(v)
	if !strings.HasPrefix(runenames.Name(r), "<CJK") {
		return 0, false
	}
	return r, true
}
