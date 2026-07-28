// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "fmt"

// Symbols resolves the dotted names an expression references — Param.Frame,
// Task.Param.Chunk, Session.WorkingDirectory and the like.
//
// The package deliberately hardcodes no namespaces: the caller decides what is
// in scope, exactly as fmtstring.Scope does for base-spec format strings.
type Symbols interface {
	// Lookup returns the value bound to a fully qualified dotted name, and
	// whether it is bound at all.
	Lookup(name string) (Value, bool)
}

// MapSymbols is a Symbols backed by a map keyed on fully qualified dotted
// names, for example {"Param.Frame": Int(7)}. A nil MapSymbols is a valid
// empty table.
type MapSymbols map[string]Value

// Lookup implements Symbols.
func (m MapSymbols) Lookup(name string) (Value, bool) {
	v, ok := m[name]
	return v, ok
}

// Eval evaluates the parsed expression against syms, which may be nil for an
// expression that references no symbols.
//
// Sub-project A evaluates operands of the SAME TYPE only: "1 + 2.5" is an
// error here and becomes valid in sub-project B, which adds implicit
// conversion. See the package documentation.
func (e *Expression) Eval(syms Symbols) (Value, error) {
	if syms == nil {
		syms = MapSymbols(nil)
	}
	return evalNode(e.root, e.src, syms)
}

// Eval parses and evaluates src in one step. Prefer Parse plus
// Expression.Eval when the same expression is evaluated more than once.
func Eval(src string, syms Symbols) (Value, error) {
	e, err := Parse(src)
	if err != nil {
		return Value{}, err
	}
	return e.Eval(syms)
}

// evalNode dispatches on node type. Each arm is a small named function so the
// switch stays within the complexity budget as sub-projects B and C add kinds.
func evalNode(n Node, src string, syms Symbols) (Value, error) {
	switch v := n.(type) {
	case *IntLit:
		return Int(v.Val), nil
	case *FloatLit:
		return Float(v.Val), nil
	case *StringLit:
		return String(v.Val), nil
	case *BoolLit:
		return Bool(v.Val), nil
	case *NullLit:
		return Null(), nil
	case *Name:
		return evalName(v, src, syms)
	case *Unary:
		return evalUnary(v, src, syms)
	case *Binary:
		return evalBinary(v, src, syms)
	case *Compare:
		return evalCompare(v, src, syms)
	case *Logical:
		return evalLogical(v, src, syms)
	case *Cond:
		return evalCond(v, src, syms)
	}
	return Value{}, errorAt(src, n.Pos(), "internal error: cannot evaluate %T", n)
}

func evalName(n *Name, src string, syms Symbols) (Value, error) {
	name := n.String()
	v, ok := syms.Lookup(name)
	if !ok {
		return Value{}, errorAt(src, n.Offset, "unknown symbol %q", name)
	}
	return v, nil
}

func evalUnary(n *Unary, src string, syms Symbols) (Value, error) {
	x, err := evalNode(n.X, src, syms)
	if err != nil {
		return Value{}, err
	}
	out, err := applyUnary(n.Op, x)
	if err != nil {
		return Value{}, wrapAt(src, n.Offset, err)
	}
	return out, nil
}

func evalBinary(n *Binary, src string, syms Symbols) (Value, error) {
	l, err := evalNode(n.L, src, syms)
	if err != nil {
		return Value{}, err
	}
	r, err := evalNode(n.R, src, syms)
	if err != nil {
		return Value{}, err
	}
	out, err := applyBinary(n.Op, l, r)
	if err != nil {
		// n.Offset is the operator's own position, so the error blames the "+"
		// rather than the start of the expression.
		return Value{}, wrapAt(src, n.Offset, err)
	}
	return out, nil
}

// evalCompare evaluates a comparison chain (spec section 1.3.6). "1 < 2 < 3"
// means "1 < 2 and 2 < 3" with each intermediate value evaluated EXACTLY ONCE,
// so the loop carries the previous operand's value forward rather than
// re-evaluating its node. A false link stops the chain, leaving the remaining
// operands unevaluated.
func evalCompare(n *Compare, src string, syms Symbols) (Value, error) {
	left, err := evalNode(n.Operands[0], src, syms)
	if err != nil {
		return Value{}, err
	}
	for i, op := range n.Ops {
		right, err := evalNode(n.Operands[i+1], src, syms)
		if err != nil {
			return Value{}, err
		}
		out, err := applyBinary(op, left, right)
		if err != nil {
			// n.OpOffsets[i] is THIS link's own operator, not the chain's
			// first one (n.Offset) — a chain of three or more operators must
			// blame whichever link actually failed.
			return Value{}, wrapAt(src, n.OpOffsets[i], err)
		}
		// Every operator that can appear in a comparison chain returns Bool
		// today, but AsBool panics on any other kind (via mustBe). Guard it
		// rather than call it blind: sub-project B's unresolved[bool] (a
		// chain link whose result is not yet known) must fail with a
		// positioned error here, not panic.
		if out.Type.Code != CodeBool {
			return Value{}, wrapAt(src, n.OpOffsets[i],
				fmt.Errorf("comparison operator %s did not produce a bool: %s", op, out.Type))
		}
		if !out.AsBool() {
			return Bool(false), nil
		}
		left = right
	}
	return Bool(true), nil
}

// evalLogical implements "and" and "or" (spec section 2.1.6). They are
// short-circuiting and VALUE-RETURNING: they return one of their operands, not
// necessarily a bool. That is why they are not rows in the operator table.
func evalLogical(n *Logical, src string, syms Symbols) (Value, error) {
	left, err := evalNode(n.L, src, syms)
	if err != nil {
		return Value{}, err
	}
	switch {
	case n.Op == OpAnd && !truthy(left):
		return left, nil
	case n.Op == OpOr && truthy(left):
		return left, nil
	}
	return evalNode(n.R, src, syms)
}

// truthy implements section 2.1.6's falsiness rule: ONLY null and false are
// falsy. Unlike Python, 0, 0.0 and "" are all truthy, which is what makes
// "Param.X or 'fallback'" a null-coalescing operator rather than an
// empty-string test.
func truthy(v Value) bool {
	switch v.Type.Code {
	case CodeNull:
		return false
	case CodeBool:
		return v.AsBool()
	}
	return true
}

// evalCond implements "<then> if <cond> else <else>" (spec section 1.3.5). The
// condition is evaluated first and must be a bool — there is no truthiness
// here, in deliberate contrast to and/or — and only the chosen branch is
// evaluated.
func evalCond(n *Cond, src string, syms Symbols) (Value, error) {
	cond, err := evalNode(n.If, src, syms)
	if err != nil {
		return Value{}, err
	}
	if cond.Type.Code != CodeBool {
		return Value{}, errorAt(src, n.If.Pos(),
			"the condition of a conditional expression must be a bool, found %s", cond.Type)
	}
	if cond.AsBool() {
		return evalNode(n.Then, src, syms)
	}
	return evalNode(n.Else, src, syms)
}
