// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
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

func (l *lexer) lexNumber() (token, error) {
	return token{}, errorAt(l.src, l.pos, "numeric literals are not implemented yet")
}

func (l *lexer) lexString(raw bool) (token, error) {
	_ = raw
	return token{}, errorAt(l.src, l.pos, "string literals are not implemented yet")
}
