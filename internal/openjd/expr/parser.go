// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "sort"

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

// Names returns every dotted value reference the expression makes — for
// example {"Param.Frame", "Task.Param.Chunk"} — sorted and deduplicated.
//
// This is the spec's accessed_symbols set. Callers use it to decide which
// symbols an expression needs before evaluating it; sub-project E uses it to
// scope-check a template's expressions without binding values.
func (e *Expression) Names() []string {
	seen := map[string]bool{}
	var out []string
	walk(e.root, func(n Node) {
		name, ok := n.(*Name)
		if !ok {
			return
		}
		if s := name.String(); !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	})
	sort.Strings(out)
	return out
}

// parser turns a token slice into a tree. One method per grammar production,
// named for it, so the code can be diffed against the BNF in spec section 1.1.
type parser struct {
	src  string
	toks []token
	pos  int
	// depth is how many grammar-descent frames are currently on the stack, so
	// that unbounded nesting is reported instead of overflowing it. See enter.
	depth int
}

// enter accounts for one level of recursive descent and returns the matching
// leave, so a production guards itself with a single deferred line.
//
// Every function that takes part in a recursion CYCLE must call it, not just
// the outermost production. The cycles, enumerated from parser.go's call graph:
//
//   - Every path that re-enters the grammar from inside a construct — a
//     parenthesis, a list element, a subscript, a slice component — goes through
//     parseExpr and so through parseConditional, which is guarded.
//   - parseConditional recurses into ITSELF for the right-associative else
//     branch, without leaving the production.
//   - parseNot and parseUnary each recurse into themselves (a stack of "not"
//     keywords, or of unary minus signs) without passing through parseExpr at
//     all. Both are guarded on the branch that recurses.
//   - parsePower and parseUnary recurse into EACH OTHER: parsePower reads its
//     exponent with parseUnary (which is what makes "**" right-associative),
//     and parseUnary falls through to parsePower whenever the next token is not
//     a sign. Neither of the two guards above closes that loop — parseUnary's
//     sits on the sign branch, which this cycle never takes — so "2**2**2**…"
//     was unbounded until parsePower was guarded too, and killed the process
//     with a stack overflow at a million operators. parsePostfix's guard does
//     not help either: its defer has already run by the time parsePower reads
//     the "**".
//   - parsePostfix is guarded as well, which closes no cycle of its own — its
//     trailer group is a LOOP, and parseSubscript re-enters through parseExpr,
//     already counted — but it is the natural place for the guard if that loop
//     is ever turned into recursion, and one more counted frame per level costs
//     nothing.
//
// The remaining productions (parseOr, parseAnd, parseLogicalLevel, parseCompare,
// parseAdd, parseMul, parseBinaryLevel) form a strictly descending precedence
// chain with no back edge, so they take part in no cycle except through
// parseExpr, and need no guard of their own.
//
// The limit is reported as an ordinary *Error carrying a position, which is the
// whole point: a stack overflow is a runtime.throw that recover() cannot catch,
// so it has to be turned into a value before it happens, not handled after.
func (p *parser) enter(tok token) (func(), error) {
	if p.depth >= maxParseDepth {
		return nil, p.errorAtTok(tok, "this expression is nested too deeply")
	}
	p.depth++
	return func() { p.depth-- }, nil
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

// parseExpr is the entry production: the full <StringInterpExpr>.
func (p *parser) parseExpr() (Node, error) { return p.parseConditional() }

// keyword reports whether tok is the contextual keyword word. Section 1.1.3
// makes keywords contextual: the lexer emits every identifier as tokIdent and
// the parser decides, by position, whether a given one is a keyword. That is
// what keeps "Param.if" a valid attribute reference.
func keyword(tok token, word string) bool {
	return tok.kind == tokIdent && tok.text == word
}

// parseConditional implements
// <ConditionalExpr> ::= <OrExpr> ("if" <OrExpr> "else" <ConditionalExpr>)?.
// The else branch recurses into parseConditional, making the form
// right-associative: "a if p else b if q else c" groups as
// "a if p else (b if q else c)".
func (p *parser) parseConditional() (Node, error) {
	leave, err := p.enter(p.peek())
	if err != nil {
		return nil, err
	}
	defer leave()

	then, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	tok := p.peek()
	if !keyword(tok, "if") {
		return then, nil
	}
	p.advance()

	cond, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if elseTok := p.peek(); !keyword(elseTok, "else") {
		return nil, p.errorAtTok(elseTok,
			`expected "else" to complete the conditional expression, found %s`, elseTok.kind)
	}
	p.advance()

	otherwise, err := p.parseConditional()
	if err != nil {
		return nil, err
	}
	return &Cond{Offset: tok.offset, Then: then, If: cond, Else: otherwise}, nil
}

// parseOr implements <OrExpr> ::= <AndExpr> ("or" <AndExpr>)*.
func (p *parser) parseOr() (Node, error) {
	return p.parseLogicalLevel("or", OpOr, p.parseAnd)
}

// parseAnd implements <AndExpr> ::= <NotExpr> ("and" <NotExpr>)*.
func (p *parser) parseAnd() (Node, error) {
	return p.parseLogicalLevel("and", OpAnd, p.parseNot)
}

// parseLogicalLevel parses one left-associative run of a keyword operator.
// These build Logical rather than Binary nodes: section 2.1.6 makes "and" and
// "or" short-circuiting and value-returning, so they never reach the operand
// dispatch table.
func (p *parser) parseLogicalLevel(word string, op Op, next func() (Node, error)) (Node, error) {
	left, err := next()
	if err != nil {
		return nil, err
	}
	for {
		tok := p.peek()
		if !keyword(tok, word) {
			return left, nil
		}
		p.advance()
		right, err := next()
		if err != nil {
			return nil, err
		}
		left = &Logical{Offset: tok.offset, Op: op, L: left, R: right}
	}
}

// parseNot implements <NotExpr> ::= "not" <NotExpr> | <CompareExpr>.
func (p *parser) parseNot() (Node, error) {
	tok := p.peek()
	if !keyword(tok, "not") {
		return p.parseCompare()
	}
	leave, err := p.enter(tok)
	if err != nil {
		return nil, err
	}
	defer leave()

	p.advance()
	x, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	return &Unary{Offset: tok.offset, Op: OpNot, X: x}, nil
}

var compareOps = map[tokenKind]Op{
	tokLt: OpLt, tokGt: OpGt, tokLe: OpLe, tokGe: OpGe, tokEq: OpEq, tokNe: OpNe,
}

// parseCompare implements <CompareExpr> ::= <AddExpr> ((...) <AddExpr>)*.
//
// A run of comparisons collapses into a single Compare node rather than
// nested Binary nodes: section 1.3.6 says "1 < 2 < 3" means "1 < 2 and 2 < 3"
// with each intermediate value evaluated exactly once, which nesting cannot
// express.
func (p *parser) parseCompare() (Node, error) {
	first, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	cmp := &Compare{Offset: first.Pos(), Operands: []Node{first}}
	for {
		tok := p.peek()
		op, ok := p.compareOperator(tok)
		if !ok {
			if len(cmp.Ops) == 0 {
				return first, nil
			}
			return cmp, nil
		}
		operand, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		cmp.Ops = append(cmp.Ops, op)
		cmp.Operands = append(cmp.Operands, operand)
		cmp.OpOffsets = append(cmp.OpOffsets, tok.offset)
		if len(cmp.Ops) == 1 {
			cmp.Offset = tok.offset
		}
	}
}

// compareOperator consumes a comparison operator if one is next and reports
// which it was. It handles the two-word "not in" form, which is why the caller
// cannot simply look the token up in compareOps.
func (p *parser) compareOperator(tok token) (Op, bool) {
	if op, ok := compareOps[tok.kind]; ok {
		p.advance()
		return op, true
	}
	if keyword(tok, "in") {
		p.advance()
		return OpIn, true
	}
	if keyword(tok, "not") && p.pos+1 < len(p.toks) && keyword(p.toks[p.pos+1], "in") {
		p.advance()
		p.advance()
		return OpNotIn, true
	}
	return 0, false
}

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
	leave, err := p.enter(tok)
	if err != nil {
		return nil, err
	}
	defer leave()

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
//
// That right operand is also a recursion cycle — parseUnary falls straight back
// through to parsePower — so this production takes the depth guard, which is
// what keeps "2**2**2**…" a parse error rather than a stack overflow. See enter.
func (p *parser) parsePower() (Node, error) {
	leave, err := p.enter(p.peek())
	if err != nil {
		return nil, err
	}
	defer leave()

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

// parsePostfix implements <PostfixExpr> ::= <PrimaryExpr> (<Subscript> |
// <Call>)*.
//
// It is a LOOP because the grammar's trailer group repeats: "x[0][1]" is a
// subscript of a subscript. Sub-project B3 attaches <Call> to the same loop.
func (p *parser) parsePostfix() (Node, error) {
	leave, err := p.enter(p.peek())
	if err != nil {
		return nil, err
	}
	defer leave()

	x, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		switch tok := p.peek(); tok.kind {
		case tokLBracket:
			x, err = p.parseSubscript(x)
			if err != nil {
				return nil, err
			}
		case tokLParen:
			return nil, p.errorAtTok(tok, "function and method calls are not supported")
		case tokDot:
			// A Name consumes its own dots, so a dot reaching here follows a
			// literal, a parenthesized expression or a trailer.
			return nil, p.errorAtTok(tok, "property access is not supported")
		default:
			return x, nil
		}
	}
}

// parseSubscript implements <Subscript> ::= "[" <SliceExpr> "]" where
// <SliceExpr> ::= <ConditionalExpr> | <Slice>.
//
// The two are told apart by a ":" at this bracket's own level: with one it is
// a slice, without one an index.
func (p *parser) parseSubscript(x Node) (Node, error) {
	open := p.advance()
	// "[:...]" — a slice whose start is absent.
	if _, ok := p.accept(tokColon); ok {
		return p.parseSliceRest(open.offset, x, nil)
	}
	first, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if _, ok := p.accept(tokColon); ok {
		return p.parseSliceRest(open.offset, x, first)
	}
	if _, err := p.expect(tokRBracket); err != nil {
		return nil, err
	}
	return &Index{Offset: open.offset, X: x, Idx: first}, nil
}

// parseSliceRest reads a slice's remaining components, the first ":" already
// consumed. An omitted component stays nil, which is NOT the same as a zero:
// its default depends on the sign of the step (spec section 1.3.8).
func (p *parser) parseSliceRest(offset int, x, start Node) (Node, error) {
	s := &Slice{Offset: offset, X: x, Start: start}
	if _, ok := p.accept(tokRBracket); ok {
		return s, nil
	}
	if p.peek().kind != tokColon {
		stop, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		s.Stop = stop
		if _, ok := p.accept(tokRBracket); ok {
			return s, nil
		}
	}
	if _, err := p.expect(tokColon); err != nil {
		return nil, err
	}
	if _, ok := p.accept(tokRBracket); ok {
		return s, nil
	}
	step, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	s.Step = step
	if _, err := p.expect(tokRBracket); err != nil {
		return nil, err
	}
	return s, nil
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
		if tok := p.peek(); keyword(tok, "for") {
			return nil, p.errorAtTok(tok, "generator expressions are not supported")
		}
		if _, err := p.expect(tokRParen); err != nil {
			return nil, err
		}
		return inner, nil
	case tokLBracket:
		return p.parseListLiteral()
	}
	return nil, p.errorAtTok(tok, "unexpected %s", tok.kind)
}

// parseListLiteral implements <ListExpr> ::= "[" (<ConditionalExpr> (","
// <ConditionalExpr>)* ","?)? "]" (spec section 1.1.2).
//
// <ListComp> shares the opening bracket and is detected here — on the first
// element only, since "[a, b for x in y]" is not a comprehension — rather than
// left to fall out as a syntax error at "for", because a bare "unexpected
// name" would send a reader hunting for a typo instead of pointing at the
// actual problem.
func (p *parser) parseListLiteral() (Node, error) {
	open := p.advance()
	lit := &ListLit{Offset: open.offset}
	if _, ok := p.accept(tokRBracket); ok {
		return lit, nil
	}
	for {
		elem, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		lit.Elems = append(lit.Elems, elem)
		if tok := p.peek(); keyword(tok, "for") {
			if len(lit.Elems) != 1 {
				return nil, p.errorAtTok(tok, "a list comprehension takes a single element expression")
			}
			return p.parseComprehensionRest(open.offset, elem)
		}
		if _, ok := p.accept(tokComma); !ok {
			break
		}
		// A trailing comma ends the list rather than starting an element.
		if _, ok := p.accept(tokRBracket); ok {
			return lit, nil
		}
	}
	if _, err := p.expect(tokRBracket); err != nil {
		return nil, err
	}
	return lit, nil
}

// parseComprehensionRest reads a comprehension after its element expression and
// the "for" keyword have been seen, through the closing bracket. Implements
// <ListComp> (spec section 1.1.2) with the semantics of section 1.3.7.
//
// The ITERABLE and the FILTER are parsed with parseOr, not parseExpr, and that
// is load-bearing rather than a shortcut. Section 1.1.2 writes both as
// <ConditionalExpr>, which is ambiguous against the optional ("if"
// <ConditionalExpr>) filter: parseConditional consumes an "if" and then demands
// an "else", so "[x for x in y if c]" would fail at the "]" looking for one.
// Python resolves the same ambiguity the same way — comp_for and comp_if both
// take an or_test — so a conditional in either position needs parentheses.
func (p *parser) parseComprehensionRest(offset int, elem Node) (Node, error) {
	p.advance() // the "for"
	comp := &ListComp{Offset: offset, Elem: elem}

	name, err := p.expect(tokIdent)
	if err != nil {
		return nil, err
	}
	if !isUserIdentifier(name.text) {
		return nil, p.errorAtTok(name,
			"a comprehension loop variable must start with a lowercase letter or underscore, found %q", name.text)
	}
	comp.Var, comp.VarOffset = name.text, name.offset

	if tok := p.peek(); !keyword(tok, "in") {
		return nil, p.errorAtTok(tok, `expected "in", found %s`, tok.kind)
	}
	p.advance()

	iter, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	comp.Iter = iter

	if tok := p.peek(); keyword(tok, "if") {
		p.advance()
		cond, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		comp.Cond = cond
		if tok := p.peek(); keyword(tok, "if") {
			return nil, p.errorAtTok(tok, `a list comprehension takes only one "if" filter`)
		}
	}
	if tok := p.peek(); keyword(tok, "for") {
		return nil, p.errorAtTok(tok,
			`a list comprehension takes only one "for" clause; Python's multi-generator form is not supported`)
	}
	if _, err := p.expect(tokRBracket); err != nil {
		return nil, err
	}
	return comp, nil
}

// isUserIdentifier reports whether s is a <UserIdentifier> (spec section
// 1.3.7): a lowercase letter or underscore, then letters, digits or
// underscores. The casing rule is what makes it impossible for a loop variable
// to shadow a spec-defined symbol like Param or Task.
func isUserIdentifier(s string) bool {
	if s == "" {
		return false
	}
	if c := s[0]; (c < 'a' || c > 'z') && c != '_' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		default:
			return false
		}
	}
	return true
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
