// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

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
