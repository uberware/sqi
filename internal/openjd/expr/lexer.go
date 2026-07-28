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

// lexIdent reads an identifier. Keywords are not recognized here: spec section
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

// lexString reads a string literal (spec sections 1.1.1 and 1.1.5). raw
// reports whether an r or R prefix is present, in which case escape sequences
// are left as written.
func (l *lexer) lexString(raw bool) (token, error) {
	start := l.pos
	if raw {
		l.pos++
	}

	quote := string(l.src[l.pos])
	delim := quote
	if strings.HasPrefix(l.src[l.pos:], strings.Repeat(quote, 3)) {
		delim = strings.Repeat(quote, 3)
	}
	l.pos += len(delim)

	bodyOffset := l.pos
	body, err := l.scanStringBody(start, delim)
	if err != nil {
		return token{}, err
	}

	s := body
	if !raw {
		if s, err = decodeEscapes(l.src, bodyOffset, body); err != nil {
			return token{}, err
		}
	}
	return token{kind: tokString, text: l.src[start:l.pos], offset: start, s: s}, nil
}

// scanStringBody advances past the literal's contents and its closing
// delimiter, returning the raw contents. start is the offset of the whole
// literal, used for the position of an unterminated-literal error.
func (l *lexer) scanStringBody(start int, delim string) (string, error) {
	long := len(delim) == 3
	bodyStart := l.pos
	for l.pos < len(l.src) {
		if strings.HasPrefix(l.src[l.pos:], delim) {
			body := l.src[bodyStart:l.pos]
			l.pos += len(delim)
			return body, nil
		}
		switch c := l.src[l.pos]; {
		case c == '\\':
			// A backslash consumes the next character so an escaped quote
			// does not terminate the literal. This holds for raw strings too:
			// r'\'' is the two-character string \', matching Python. Only the
			// decoding step below distinguishes raw from non-raw.
			if l.pos+1 >= len(l.src) {
				return "", errorAt(l.src, start, "unterminated string literal")
			}
			l.pos += 2
		case c == '\n' && !long:
			return "", errorAt(l.src, start,
				"unterminated string literal: a newline may not appear in a singly-quoted string")
		default:
			l.pos++
		}
	}
	return "", errorAt(l.src, start, "unterminated string literal")
}

// decodeEscapes expands the escape sequences in a non-raw string body.
// bodyOffset is the body's offset within src, so an error can point at the
// offending escape rather than at the literal.
func decodeEscapes(src string, bodyOffset int, body string) (string, error) {
	var b strings.Builder
	b.Grow(len(body))
	for i := 0; i < len(body); {
		if body[i] != '\\' {
			b.WriteByte(body[i])
			i++
			continue
		}
		n, err := writeEscape(&b, src, bodyOffset, body, i)
		if err != nil {
			return "", err
		}
		i += n
	}
	return b.String(), nil
}

// writeEscape decodes the escape sequence starting at body[i] and reports how
// many bytes of body it consumed.
func writeEscape(b *strings.Builder, src string, bodyOffset int, body string, i int) (int, error) {
	at := bodyOffset + i
	if i+1 >= len(body) {
		return 0, errorAt(src, at, "string ends with a lone backslash")
	}
	switch c := body[i+1]; c {
	case '\\', '\'', '"':
		b.WriteByte(c)
		return 2, nil
	case 'n':
		b.WriteByte('\n')
		return 2, nil
	case 'r':
		b.WriteByte('\r')
		return 2, nil
	case 't':
		b.WriteByte('\t')
		return 2, nil
	case 'x':
		return writeHexEscape(b, src, at, body, i, 2)
	case 'u':
		return writeHexEscape(b, src, at, body, i, 4)
	case 'U':
		return writeHexEscape(b, src, at, body, i, 8)
	case 'N':
		return writeNamedEscape(b, src, at, body, i)
	default:
		// Section 1.1.5 lists the escapes handled above. Python keeps any
		// other escape verbatim, backslash included, and so does this:
		// rejecting them would break Windows paths and regular expressions
		// written without an r prefix. \a, \b, \f, \v and octal escapes are
		// therefore NOT decoded — they are absent from the spec's table.
		b.WriteByte('\\')
		b.WriteByte(c)
		return 2, nil
	}
}

// writeHexEscape decodes \xhh, \uhhhh and \Uhhhhhhhh, which take exactly 2, 4
// and 8 hexadecimal digits respectively.
func writeHexEscape(b *strings.Builder, src string, at int, body string, i, digits int) (int, error) {
	marker := body[i+1]
	start := i + 2
	if start+digits > len(body) {
		return 0, errorAt(src, at, `\%c escape needs %d hexadecimal digits`, marker, digits)
	}
	text := body[start : start+digits]
	v, err := strconv.ParseUint(text, 16, 32)
	if err != nil {
		return 0, errorAt(src, at, `\%c escape needs %d hexadecimal digits, got %q`, marker, digits, text)
	}

	// \xhh names a code point, not a byte, so U+0000 to U+00FF are always
	// valid. The wider escapes can name something that is not a code point.
	// The range check runs on v, before the conversion to rune, so a
	// \Uhhhhhhhh value near the uint32 ceiling cannot wrap around int32 and
	// slip past the check.
	if marker != 'x' && (v > utf8.MaxRune || (v >= 0xD800 && v <= 0xDFFF)) {
		return 0, errorAt(src, at, `\%c escape %q is not a valid unicode code point`, marker, text)
	}
	b.WriteRune(rune(v)) //nolint:gosec // G115: the check above bounds v to utf8.MaxRune, so it fits in an int32 rune
	return 2 + digits, nil
}

// writeNamedEscape decodes \N{NAME}. Task 5 replaces this body.
func writeNamedEscape(b *strings.Builder, src string, at int, body string, i int) (int, error) {
	_, _, _ = b, body, i
	return 0, errorAt(src, at, `\N escapes are not implemented yet`)
}
