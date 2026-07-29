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

// Eval evaluates the parsed expression against syms, coercing the result to
// target. syms may be nil for an expression that references no symbols; pass
// TAny as the target to accept the expression's natural result type.
//
// The target guides implicit coercion (section 1.3.1). It applies HERE, at the
// boundary, and inside operator dispatch when a shape is matched — it does NOT
// propagate into sub-expressions. Section 1.3.1 leaves that open; propagating it
// would make "Param.Count + 1" against a string target concatenate its operands
// into "11" rather than adding them. Section 1.3.2's own example of
// "Count: {{ len(myList) }}" yielding "Count: 5" describes evaluating naturally
// and converting afterward. List literals (section 1.2.6) and function
// arguments do take a target inward, and both belong to later sub-projects.
func (e *Expression) Eval(syms Symbols, target Type) (Value, error) {
	if syms == nil {
		syms = MapSymbols(nil)
	}
	// The target is threaded inward for the node kinds that forward it (see
	// evalNode); the boundary coercion below still applies to the result of the
	// whole expression, whether or not any node consumed the target.
	v, err := evalNode(e.root, e.src, syms, target)
	if err != nil {
		return Value{}, err
	}
	out, err := coerce(v, target)
	if err != nil {
		// The whole expression failed to meet its context's type, so blame the
		// expression's start rather than any operator inside it.
		return Value{}, wrapAt(e.src, e.root.Pos(), err)
	}
	return out, nil
}

// Eval parses and evaluates src in one step. Prefer Parse plus Expression.Eval
// when the same expression is evaluated more than once.
func Eval(src string, syms Symbols, target Type) (Value, error) {
	e, err := Parse(src)
	if err != nil {
		return Value{}, err
	}
	return e.Eval(syms, target)
}

// evalNode dispatches on node type.
//
// target is the type the surrounding context expects (section 1.3.1). It is
// forwarded ONLY by the node kinds whose result IS a sub-expression's value —
// see the forwarding table in evalCond, evalLogical and evalListLit. Nodes that
// COMPUTE a value pass TAny to their operands: propagating a string target into
// "Param.Count + 1" would concatenate its operands into "11" rather than adding
// them.
func evalNode(n Node, src string, syms Symbols, target Type) (Value, error) {
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
		return evalLogical(v, src, syms, target)
	case *Cond:
		return evalCond(v, src, syms, target)
	case *ListLit:
		return evalListLit(v, src, syms, target)
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
	x, err := evalNode(n.X, src, syms, TAny)
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
	l, err := evalNode(n.L, src, syms, TAny)
	if err != nil {
		return Value{}, err
	}
	r, err := evalNode(n.R, src, syms, TAny)
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
// operands unevaluated. An unknown link stops it the same way; the spec states
// this rule only for a conditional expression, but a comparison chain
// short-circuits for the same reason and so needs the same treatment.
func evalCompare(n *Compare, src string, syms Symbols) (Value, error) {
	left, err := evalNode(n.Operands[0], src, syms, TAny)
	if err != nil {
		return Value{}, err
	}
	for i, op := range n.Ops {
		right, err := evalNode(n.Operands[i+1], src, syms, TAny)
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
		// A link whose operands are not all known yields an unknown bool, so the
		// chain's outcome is unknown too. Stop here: the remaining operands
		// cannot change the answer, and evaluating them would only risk a
		// spurious error from a branch that may never run.
		if out.IsUnresolved() {
			return Unresolved(TBool), nil
		}
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
// necessarily a bool. That is why they are not shapes in the operator table.
//
// When the left operand is not yet known, neither is which operand comes back,
// so the result is a placeholder over both types. The spec states this rule only
// for a conditional expression, but and/or short-circuit for the same reason and
// so need the same treatment.
func evalLogical(n *Logical, src string, syms Symbols, target Type) (Value, error) {
	left, err := evalNode(n.L, src, syms, target)
	if err != nil {
		return Value{}, err
	}
	if left.IsUnresolved() {
		right, err := evalNode(n.R, src, syms, target)
		if err != nil {
			// The left operand is still reachable at runtime, so the expression
			// as a whole can still produce a value: report its type rather than
			// an error from a branch that may never be taken.
			return Unresolved(left.Type), nil //nolint:nilerr // deliberate suppression: the right operand's error can never surface at runtime when the left is still reachable
		}
		return Unresolved(UnionOf(left.Type, right.Type)), nil
	}
	switch {
	case n.Op == OpAnd && !truthy(left):
		return left, nil
	case n.Op == OpOr && truthy(left):
		return left, nil
	}
	return evalNode(n.R, src, syms, target)
}

// truthy implements section 2.1.6's falsiness rule: ONLY null and false are
// falsy. Unlike Python, 0, 0.0 and "" are all truthy, which is what makes
// "Param.X or 'fallback'" a null-coalescing operator rather than an
// empty-string test.
//
// A placeholder never reaches here: evalLogical handles it before asking.
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
//
// When the condition is not yet known, the evaluator cannot tell which branch
// will run, so both are evaluated and their types combined. See condResult.
func evalCond(n *Cond, src string, syms Symbols, target Type) (Value, error) {
	cond, err := evalNode(n.If, src, syms, TBool)
	if err != nil {
		return Value{}, err
	}
	if cond.IsUnresolved() {
		if !includes(cond.Type, CodeBool) {
			return Value{}, errorAt(src, n.If.Pos(),
				"the condition of a conditional expression must be a bool, found %s", cond.Type)
		}
		return condResult(n, src, syms, target)
	}
	if cond.Type.Code != CodeBool {
		return Value{}, errorAt(src, n.If.Pos(),
			"the condition of a conditional expression must be a bool, found %s", cond.Type)
	}
	if cond.AsBool() {
		return evalNode(n.Then, src, syms, target)
	}
	return evalNode(n.Else, src, syms, target)
}

// condResult evaluates both branches of a conditional whose condition is not yet
// known, per the spec's "Conditional Expressions with Unknown Conditions" rule.
//
// A branch that fails could never have produced a value at runtime either, so
// its error is suppressed and the other branch's type stands alone. Only when
// BOTH fail is there a real error, and then it names both — a reader cannot tell
// which branch was meant.
func condResult(n *Cond, src string, syms Symbols, target Type) (Value, error) {
	thenVal, thenErr := evalNode(n.Then, src, syms, target)
	elseVal, elseErr := evalNode(n.Else, src, syms, target)
	switch {
	case thenErr == nil && elseErr == nil:
		return Unresolved(UnionOf(thenVal.Type, elseVal.Type)), nil
	case thenErr == nil:
		return Unresolved(thenVal.Type), nil
	case elseErr == nil:
		return Unresolved(elseVal.Type), nil
	}
	return Value{}, errorAt(src, n.Offset,
		"both branches of this conditional expression fail: %v; and %v", thenErr, elseErr)
}
