// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

// Expression is a parsed expression, ready to evaluate.
type Expression struct {
	src  string
	root Node
}

// Parse reads an expression and returns its tree, or an *Error naming the
// position at which the source stopped making sense.
//
// The grammar implemented here is EXPR's, not Python's. Anything outside it —
// list literals, comprehensions, subscripts, slices, calls, property access,
// bitwise operators, assignment — is rejected. That direction is deliberate:
// a reader borrowed from a full Python parser would silently ACCEPT syntax
// this package cannot evaluate, whereas anything unimplemented here fails to
// parse.
func Parse(src string) (*Expression, error) {
	toks, err := tokenize(src)
	if err != nil {
		return nil, err
	}
	p := &parser{src: src, toks: toks}
	if p.peek().kind == tokEOF {
		return nil, errorAt(src, 0, "empty expression")
	}
	root, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if tok := p.peek(); tok.kind != tokEOF {
		return nil, p.errorAtTok(tok, "unexpected %s after expression", tok.kind)
	}
	return &Expression{src: src, root: root}, nil
}

// Source returns the text this expression was parsed from.
func (e *Expression) Source() string { return e.src }

// Root returns the tree's root node.
func (e *Expression) Root() Node { return e.root }

// parser turns a token slice into a tree. One method per grammar production,
// named for it, so the code can be diffed against the BNF in spec section 1.1.
type parser struct {
	src  string
	toks []token
	pos  int
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) advance() token {
	tok := p.toks[p.pos]
	if tok.kind != tokEOF {
		p.pos++
	}
	return tok
}

func (p *parser) accept(kind tokenKind) (token, bool) {
	if tok := p.peek(); tok.kind == kind {
		return p.advance(), true
	}
	return token{}, false
}

func (p *parser) expect(kind tokenKind) (token, error) {
	tok := p.peek()
	if tok.kind != kind {
		if tok.kind == tokEOF {
			// Running out of input reads better as "unexpected end of
			// expression" than "expected X, found end of expression" — the
			// same wording parsePrimary already uses for a bare EOF.
			return token{}, p.errorAtTok(tok, "unexpected %s", tok.kind)
		}
		return token{}, p.errorAtTok(tok, "expected %s, found %s", kind, tok.kind)
	}
	return p.advance(), nil
}

func (p *parser) errorAtTok(tok token, format string, args ...any) *Error {
	return errorAt(p.src, tok.offset, format, args...)
}

// primaryKeywords may not start a primary expression. True/False/None and
// their aliases are handled before this check, since they are literals.
var primaryKeywords = map[string]bool{
	"if": true, "else": true, "and": true, "or": true,
	"not": true, "for": true, "in": true,
}

// parseExpr is the entry production. Task 7 of the implementation plan
// replaces the body with p.parseConditional() once the outer precedence
// levels exist.
func (p *parser) parseExpr() (Node, error) { return p.parseAdd() }

var (
	addOps = map[tokenKind]Op{tokPlus: OpAdd, tokMinus: OpSub}
	mulOps = map[tokenKind]Op{
		tokStar: OpMul, tokSlash: OpDiv, tokDoubleSlash: OpFloorDiv, tokPercent: OpMod,
	}
)

// parseAdd implements <AddExpr> ::= <MulExpr> (("+" | "-") <MulExpr>)*.
func (p *parser) parseAdd() (Node, error) { return p.parseBinaryLevel(addOps, p.parseMul) }

// parseMul implements <MulExpr> ::= <UnaryExpr> (("*" | "/" | "//" | "%") <UnaryExpr>)*.
func (p *parser) parseMul() (Node, error) { return p.parseBinaryLevel(mulOps, p.parseUnary) }

// parseBinaryLevel parses one left-associative precedence level: next, then
// any number of (operator, next) pairs drawn from ops.
func (p *parser) parseBinaryLevel(ops map[tokenKind]Op, next func() (Node, error)) (Node, error) {
	left, err := next()
	if err != nil {
		return nil, err
	}
	for {
		tok := p.peek()
		op, ok := ops[tok.kind]
		if !ok {
			return left, nil
		}
		p.advance()
		right, err := next()
		if err != nil {
			return nil, err
		}
		left = &Binary{Offset: tok.offset, Op: op, L: left, R: right}
	}
}

// parseUnary implements <UnaryExpr> ::= ("-" | "+") <UnaryExpr> | <PowerExpr>.
func (p *parser) parseUnary() (Node, error) {
	tok := p.peek()
	var op Op
	switch tok.kind {
	case tokMinus:
		op = OpNeg
	case tokPlus:
		op = OpPos
	default:
		return p.parsePower()
	}
	p.advance()
	x, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	return &Unary{Offset: tok.offset, Op: op, X: x}, nil
}

// parsePower implements <PowerExpr> ::= <PostfixExpr> ("**" <UnaryExpr>)?.
// The right operand is a UnaryExpr, which makes ** right-associative and lets
// "2 ** -1" parse while leaving "-2 ** 2" as -(2 ** 2).
func (p *parser) parsePower() (Node, error) {
	base, err := p.parsePostfix()
	if err != nil {
		return nil, err
	}
	tok, ok := p.accept(tokDoubleStar)
	if !ok {
		return base, nil
	}
	exp, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	return &Binary{Offset: tok.offset, Op: OpPow, L: base, R: exp}, nil
}

// parsePostfix implements <PostfixExpr> ::= <PrimaryExpr> (<Subscript> | <Call>)*.
//
// Sub-project A has no postfix forms, so each one is rejected by name rather
// than by falling through to a generic "unexpected token". Sub-projects B
// (subscript, slice) and C (calls, properties) replace these arms.
func (p *parser) parsePostfix() (Node, error) {
	x, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	switch tok := p.peek(); tok.kind {
	case tokLBracket:
		return nil, p.errorAtTok(tok, "subscript and slice expressions are not supported")
	case tokLParen:
		return nil, p.errorAtTok(tok, "function and method calls are not supported")
	case tokDot:
		// A Name consumes its own dots, so a dot reaching here follows a
		// literal or a parenthesised expression.
		return nil, p.errorAtTok(tok, "property access is not supported")
	}
	return x, nil
}

// parsePrimary implements <PrimaryExpr> ::= <ValueReference> | <Literal>
// | <ListExpr> | <ListComp> | "(" <ConditionalExpr> ")".
func (p *parser) parsePrimary() (Node, error) {
	tok := p.peek()
	switch tok.kind {
	case tokInt:
		p.advance()
		return &IntLit{Offset: tok.offset, Val: tok.i}, nil
	case tokFloat:
		p.advance()
		return &FloatLit{Offset: tok.offset, Val: tok.f}, nil
	case tokString:
		p.advance()
		return &StringLit{Offset: tok.offset, Val: tok.s}, nil
	case tokIdent:
		return p.parseIdentPrimary()
	case tokLParen:
		p.advance()
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRParen); err != nil {
			return nil, err
		}
		return inner, nil
	case tokLBracket:
		return nil, p.errorAtTok(tok, "list expressions are not supported")
	}
	return nil, p.errorAtTok(tok, "unexpected %s", tok.kind)
}

// parseIdentPrimary reads a literal keyword or a dotted <Name>.
func (p *parser) parseIdentPrimary() (Node, error) {
	tok := p.advance()
	switch tok.text {
	case "True", "true":
		return &BoolLit{Offset: tok.offset, Val: true}, nil
	case "False", "false":
		return &BoolLit{Offset: tok.offset, Val: false}, nil
	case "None", "null":
		return &NullLit{Offset: tok.offset}, nil
	}
	if primaryKeywords[tok.text] {
		return nil, p.errorAtTok(tok, "unexpected keyword %q", tok.text)
	}

	name := &Name{Offset: tok.offset, Parts: []string{tok.text}}
	for {
		if _, ok := p.accept(tokDot); !ok {
			return name, nil
		}
		attr, err := p.expect(tokIdent)
		if err != nil {
			return nil, err
		}
		// No keyword check here, deliberately: section 1.1.3 makes keywords
		// contextual, so Param.if and Param.True are ordinary attributes.
		name.Parts = append(name.Parts, attr.text)
	}
}
