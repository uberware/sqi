// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

// tokenKind identifies one lexical token class.
//
// The set is deliberately narrow: it covers exactly the characters EXPR's
// grammar uses (spec section 1.1) and nothing else. Every other character is
// an "unexpected character" error, which is how syntax the evaluator cannot
// handle — bitwise operators, dict and set literals, the walrus operator,
// matmul, assignment — is rejected rather than silently accepted.
type tokenKind int

const (
	tokEOF tokenKind = iota
	tokInt
	tokFloat
	tokString
	tokIdent
	tokPlus
	tokMinus
	tokStar
	tokDoubleStar
	tokSlash
	tokDoubleSlash
	tokPercent
	tokLParen
	tokRParen
	tokLBracket
	tokRBracket
	tokComma
	tokDot
	tokColon
	tokLt
	tokGt
	tokLe
	tokGe
	tokEq
	tokNe
)

// tokenNames renders a kind for error messages.
var tokenNames = map[tokenKind]string{
	tokEOF:         "end of expression",
	tokInt:         "integer literal",
	tokFloat:       "float literal",
	tokString:      "string literal",
	tokIdent:       "name",
	tokPlus:        `"+"`,
	tokMinus:       `"-"`,
	tokStar:        `"*"`,
	tokDoubleStar:  `"**"`,
	tokSlash:       `"/"`,
	tokDoubleSlash: `"//"`,
	tokPercent:     `"%"`,
	tokLParen:      `"("`,
	tokRParen:      `")"`,
	tokLBracket:    `"["`,
	tokRBracket:    `"]"`,
	tokComma:       `","`,
	tokDot:         `"."`,
	tokColon:       `":"`,
	tokLt:          `"<"`,
	tokGt:          `">"`,
	tokLe:          `"<="`,
	tokGe:          `">="`,
	tokEq:          `"=="`,
	tokNe:          `"!="`,
}

func (k tokenKind) String() string {
	if name, ok := tokenNames[k]; ok {
		return name
	}
	return "unknown token"
}

// token is one lexical token. The literal payload fields are valid only for
// their corresponding kind: i for tokInt, f for tokFloat, s for tokString.
type token struct {
	kind   tokenKind
	text   string
	offset int
	i      int64
	f      float64
	s      string
}

// operators is the punctuation table, ordered longest-first so that maximal
// munch falls out of a linear scan: "**" is matched before "*", "//" before
// "/", and "<=" before "<".
var operators = []struct {
	text string
	kind tokenKind
}{
	{"**", tokDoubleStar},
	{"//", tokDoubleSlash},
	{"<=", tokLe},
	{">=", tokGe},
	{"==", tokEq},
	{"!=", tokNe},
	{"+", tokPlus},
	{"-", tokMinus},
	{"*", tokStar},
	{"/", tokSlash},
	{"%", tokPercent},
	{"(", tokLParen},
	{")", tokRParen},
	{"[", tokLBracket},
	{"]", tokRBracket},
	{",", tokComma},
	{".", tokDot},
	{":", tokColon},
	{"<", tokLt},
	{">", tokGt},
}
