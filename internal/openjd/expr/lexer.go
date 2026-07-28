// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// lexer turns expression source text into tokens.
type lexer struct {
	src string
	pos int
}

// tokenize reads src to the end and returns every token. The returned slice
// always ends with a tokEOF token, so the parser never has to bounds-check.
func tokenize(src string) ([]token, error) {
	l := &lexer{src: src}
	var toks []token
	for {
		tok, err := l.next()
		if err != nil {
			return nil, err
		}
		toks = append(toks, tok)
		if tok.kind == tokEOF {
			return toks, nil
		}
	}
}

func (l *lexer) next() (token, error) {
	l.skipSpace()
	if l.pos >= len(l.src) {
		return token{kind: tokEOF, offset: l.pos}, nil
	}

	c := l.src[l.pos]
	switch {
	case isDigit(c):
		return l.lexNumber()
	case c == '.' && l.pos+1 < len(l.src) && isDigit(l.src[l.pos+1]):
		// A leading-dot float such as ".5". Checked before the operator table
		// so "." does not steal the literal's first character.
		return l.lexNumber()
	case isQuote(c):
		return l.lexString(false)
	case (c == 'r' || c == 'R') && l.pos+1 < len(l.src) && isQuote(l.src[l.pos+1]):
		// A raw-string prefix. No whitespace is permitted between the prefix
		// and the quote, so this only fires when they are adjacent.
		return l.lexString(true)
	case isIdentStart(c):
		return l.lexIdent(), nil
	}

	for _, op := range operators {
		if strings.HasPrefix(l.src[l.pos:], op.text) {
			tok := token{kind: op.kind, text: op.text, offset: l.pos}
			l.pos += len(op.text)
			return tok, nil
		}
	}

	r, _ := utf8.DecodeRuneInString(l.src[l.pos:])
	return token{}, errorAt(l.src, l.pos, "unexpected character %q", r)
}

// skipSpace consumes whitespace. Newlines are ordinary whitespace: spec
// section 1.1.7 lets an expression span lines with no continuation syntax.
func (l *lexer) skipSpace() {
	for l.pos < len(l.src) {
		switch l.src[l.pos] {
		case ' ', '\t', '\r', '\n', '\f', '\v':
			l.pos++
		default:
			return
		}
	}
}

// lexIdent reads an identifier. Keywords are not recognised here: spec section
// 1.1.3 makes them contextual — "if", "and" and "True" are keywords only in
// their syntactic positions, and are ordinary attribute names after a ".". The
// parser makes that call, so the lexer emits tokIdent for all of them.
func (l *lexer) lexIdent() token {
	start := l.pos
	for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
		l.pos++
	}
	return token{kind: tokIdent, text: l.src[start:l.pos], offset: start}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isQuote(c byte) bool { return c == '\'' || c == '"' }

// isIdentStart and isIdentPart implement OpenJD's <Identifier> production,
// [A-Za-z_][A-Za-z0-9_]*, which is ASCII-only. Using unicode.IsLetter here
// would accept names the rest of OpenJD rejects.
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }

// lexNumber reads a numeric literal (spec sections 1.1.1 and 1.1.6).
//
// Scanning and conversion are separated: scanNumber decides how far the
// literal extends, then strconv does the arithmetic. strconv.ParseInt with
// base 0 already implements every base prefix and EXPR's underscore rules, so
// the only extra work is rejecting leading zeros, which base 0 accepts as
// Go's legacy octal but section 1.1.6 makes a syntax error.
func (l *lexer) lexNumber() (token, error) {
	start := l.pos
	isFloat, based := l.scanNumber()
	text := l.src[start:l.pos]

	if isFloat {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				return token{}, errorAt(l.src, start, "float literal %s is out of range", text)
			}
			return token{}, errorAt(l.src, start, "invalid float literal %s", text)
		}
		return token{kind: tokFloat, text: text, offset: start, f: f}, nil
	}

	// Only decimal literals carry the leading-zero rule. A based literal such
	// as "0x" must fall through to ParseInt, which names it properly as an
	// invalid literal rather than blaming a leading zero it does not have.
	if !based {
		if err := checkNoLeadingZeros(text); err != nil {
			return token{}, errorAt(l.src, start, "%s", err.Error())
		}
	}

	i, err := strconv.ParseInt(text, 0, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return token{}, errorAt(l.src, start, "integer literal %s is out of range for int64", text)
		}
		return token{}, errorAt(l.src, start, "invalid integer literal %s", text)
	}
	return token{kind: tokInt, text: text, offset: start, i: i}, nil
}

// scanNumber advances l.pos past a numeric literal and reports whether it is a
// float and whether it carries a base prefix. It is deliberately permissive
// about the literal's internal shape: strconv rejects malformed underscore
// placement and bad digits, and does it with better messages than a
// hand-rolled validator would.
func (l *lexer) scanNumber() (isFloat, based bool) {
	if l.src[l.pos] == '0' && l.pos+1 < len(l.src) && isBasePrefix(l.src[l.pos+1]) {
		// A based literal: 0x, 0o or 0b. No decimal point and no exponent, so
		// "0x1e+2" reads as 0x1e, "+", 2 — matching Python.
		l.pos += 2
		l.scanDigits(isBaseDigit)
		return false, true
	}

	l.scanDigits(isDigit)
	if l.pos < len(l.src) && l.src[l.pos] == '.' {
		isFloat = true
		l.pos++
		l.scanDigits(isDigit)
	}
	if l.scanExponent() {
		isFloat = true
	}
	return isFloat, false
}

// scanExponent consumes an "e"/"E" exponent when one is genuinely present and
// reports whether it did. An "e" not followed by a digit — optionally signed —
// begins an identifier instead, which the parser rejects; consuming it here
// would turn a clear "unexpected name" into a confusing literal error.
func (l *lexer) scanExponent() bool {
	if l.pos >= len(l.src) || (l.src[l.pos] != 'e' && l.src[l.pos] != 'E') {
		return false
	}
	next := l.pos + 1
	if next < len(l.src) && (l.src[next] == '+' || l.src[next] == '-') {
		next++
	}
	if next >= len(l.src) || !isDigit(l.src[next]) {
		return false
	}
	l.pos = next
	l.scanDigits(isDigit)
	return true
}

// scanDigits advances past a run of characters accepted by ok, plus the
// underscore separators section 1.1.6 permits between them.
func (l *lexer) scanDigits(ok func(byte) bool) {
	for l.pos < len(l.src) && (ok(l.src[l.pos]) || l.src[l.pos] == '_') {
		l.pos++
	}
}

func isBasePrefix(c byte) bool {
	switch c {
	case 'x', 'X', 'o', 'O', 'b', 'B':
		return true
	}
	return false
}

// isBaseDigit accepts every digit any of the three bases can use. Rejecting a
// digit that is wrong for the actual base ("0b2") is left to strconv, which
// names the offending literal in its message.
func isBaseDigit(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// checkNoLeadingZeros implements section 1.1.6: a decimal integer may not
// carry leading zeros ("007" and "0123" are syntax errors), but "0" and "00"
// are valid because they unambiguously represent zero.
func checkNoLeadingZeros(text string) error {
	digits := strings.ReplaceAll(text, "_", "")
	if len(digits) > 1 && digits[0] == '0' && strings.Trim(digits, "0") != "" {
		return fmt.Errorf(
			"leading zeros are not permitted in decimal integer %s; use the 0o prefix for octal", text,
		)
	}
	return nil
}

func (l *lexer) lexString(raw bool) (token, error) {
	_ = raw
	return token{}, errorAt(l.src, l.pos, "string literals are not implemented yet")
}
