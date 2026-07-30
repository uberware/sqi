// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"slices"
	"strings"
)

// Op identifies an operator. The same value names an operator in the tree and
// keys the dispatch tables in ops.go, so there is one operator vocabulary
// rather than two that can drift.
type Op int

// The operator constants. Each names one operator that a tree node can
// carry.
const (
	OpAdd Op = iota
	OpSub
	OpMul
	OpDiv
	OpFloorDiv
	OpMod
	OpPow
	OpNeg
	OpPos
	OpNot
	OpAnd
	OpOr
	OpLt
	OpGt
	OpLe
	OpGe
	OpEq
	OpNe
	OpIn
	OpNotIn
)

var opNames = map[Op]string{
	OpAdd:      "+",
	OpSub:      "-",
	OpMul:      "*",
	OpDiv:      "/",
	OpFloorDiv: "//",
	OpMod:      "%",
	OpPow:      "**",
	OpNeg:      "-",
	OpPos:      "+",
	OpNot:      "not",
	OpAnd:      "and",
	OpOr:       "or",
	OpLt:       "<",
	OpGt:       ">",
	OpLe:       "<=",
	OpGe:       ">=",
	OpEq:       "==",
	OpNe:       "!=",
	OpIn:       "in",
	OpNotIn:    "not in",
}

func (o Op) String() string {
	if name, ok := opNames[o]; ok {
		return name
	}
	return "unknown operator"
}

// Node is one node in a parsed expression tree.
//
// Every node records the byte offset in the source it came from, so an
// evaluation error can blame the operator that failed rather than the start of
// the expression. For an operation node the offset is the operator's own.
type Node interface {
	// Pos returns the byte offset in the source where this node begins.
	Pos() int
	node()
}

// IntLit is an integer literal.
type IntLit struct {
	Offset int
	Val    int64
}

// FloatLit is a floating-point literal.
type FloatLit struct {
	Offset int
	Val    float64
}

// StringLit is a string literal, with escape sequences already expanded.
type StringLit struct {
	Offset int
	Val    string
}

// BoolLit is True/true or False/false (spec sections 1.1.1 and 1.1.4).
type BoolLit struct {
	Offset int
	Val    bool
}

// NullLit is None or null (spec sections 1.1.1 and 1.1.4).
type NullLit struct {
	Offset int
}

// Name is a dotted value reference such as Param.Foo or Task.Param.Frame.
// Parts holds the dot-separated segments in order and is never empty.
type Name struct {
	Offset int
	Parts  []string
}

// String returns the fully qualified dotted name.
func (n *Name) String() string { return strings.Join(n.Parts, ".") }

// Unary is a prefix operation: OpNeg, OpPos or OpNot.
type Unary struct {
	Offset int
	Op     Op
	X      Node
}

// Binary is an arithmetic, string or comparison operation whose behavior
// comes from the dispatch table in ops.go.
type Binary struct {
	Offset int
	Op     Op
	L, R   Node
}

// Compare is a comparison chain (spec section 1.3.6). "1 < 2 < 3" holds three
// operands and two operators; len(Operands) is always len(Ops)+1. Chains are
// one node rather than nested Binary nodes because each intermediate value
// must be evaluated exactly once.
//
// OpOffsets holds the byte offset of each operator in Ops, in order, so
// len(OpOffsets) is always len(Ops) too: OpOffsets[i] is where Ops[i] sits in
// the source, letting the evaluator blame the specific link in the chain that
// failed rather than the chain's first operator.
type Compare struct {
	// Offset is the byte offset of the FIRST comparison operator in the
	// chain. It is what Pos() returns and is unrelated to which link, if any,
	// fails during evaluation — see OpOffsets for that.
	Offset    int
	Ops       []Op
	Operands  []Node
	OpOffsets []int
}

// Logical is "and" or "or" (spec section 2.1.6). These are not in the
// dispatch table: they short-circuit and return one of their operands rather
// than a bool, so they cannot be expressed as a Shape with a fixed return type.
type Logical struct {
	Offset int
	Op     Op
	L, R   Node
}

// Cond is the conditional expression "Then if If else Else"
// (spec section 1.3.5).
type Cond struct {
	Offset         int
	Then, If, Else Node
}

// ListLit is a list literal (spec section 1.1.2). Elems is empty for "[]",
// whose type is list[nulltype] (section 1.2.6).
type ListLit struct {
	// Offset is the byte offset of the opening "[".
	Offset int
	Elems  []Node
}

// Index is a subscript, "X[Idx]" (spec section 2.1.7).
type Index struct {
	// Offset is the byte offset of the "[", so an out-of-bounds error blames
	// the subscript rather than the start of the expression.
	Offset int
	X, Idx Node
}

// Slice is a slice, "X[Start:Stop:Step]" (spec sections 1.3.8 and 2.1.8).
//
// An ABSENT component is nil, which is distinct from an explicit one: section
// 1.3.8 defaults start to 0 or to the end and stop to the length or to
// -length-1, depending on the SIGN OF THE STEP, so a zero value could not carry
// the distinction.
type Slice struct {
	// Offset is the byte offset of the "[".
	Offset int
	X      Node
	Start  Node
	Stop   Node
	Step   Node
}

// ListComp is a list comprehension (spec section 1.3.7):
// "[" Elem "for" Var "in" Iter ("if" Cond)? "]".
//
// Cond is nil when the comprehension has no filter. Section 1.3.7 permits
// exactly one filter and no second "for" — Python's multi-generator form is
// rejected by the parser, not represented here.
type ListComp struct {
	// Offset is the byte offset of the opening "[".
	Offset int
	Elem   Node
	// Var is the loop variable, which section 1.3.7 requires to be a
	// <UserIdentifier> so it cannot shadow a spec-defined symbol like Param.
	Var string
	// VarOffset is where Var sits in the source, so a shadowing error blames
	// the variable rather than the opening bracket.
	VarOffset int
	Iter      Node
	Cond      Node
}

// Access is a property access, "X.Attr" (spec section 1.3.3, where a property
// p resolves to the function __property_p__).
//
// It is produced ONLY when the receiver is not a Name — "[1,2].len" or
// "(x).y". A dotted name keeps all its segments inside one Name node, and
// resolution splits it, so "Param.File.stem" produces no Access.
type Access struct {
	// Offset is the byte offset of the ".".
	Offset int
	X      Node
	Attr   string
}

// Call is a call, "Callee(Args...)" (spec section 1.3.3).
//
// Callee carries the un-split callee: a Name whose segments resolution walks,
// or an Access whose X is the receiver. Which of those it is decides whether
// section 1.2.4's receiver coercion restriction applies, so the distinction
// must survive parsing rather than being desugared away here.
type Call struct {
	// Offset is the byte offset of the "(".
	Offset int
	Callee Node
	Args   []Node
}

// Pos returns the byte offset in the source where this literal begins.
func (n *IntLit) Pos() int { return n.Offset }

// Pos returns the byte offset in the source where this literal begins.
func (n *FloatLit) Pos() int { return n.Offset }

// Pos returns the byte offset in the source where this literal begins.
func (n *StringLit) Pos() int { return n.Offset }

// Pos returns the byte offset in the source where this literal begins.
func (n *BoolLit) Pos() int { return n.Offset }

// Pos returns the byte offset in the source where this literal begins.
func (n *NullLit) Pos() int { return n.Offset }

// Pos returns the byte offset in the source where this name begins.
func (n *Name) Pos() int { return n.Offset }

// Pos returns the byte offset of this unary operation's operator.
func (n *Unary) Pos() int { return n.Offset }

// Pos returns the byte offset of this binary operation's operator.
func (n *Binary) Pos() int { return n.Offset }

// Pos returns the byte offset of this comparison chain's first operator.
func (n *Compare) Pos() int { return n.Offset }

// Pos returns the byte offset of this logical operation's operator.
func (n *Logical) Pos() int { return n.Offset }

// Pos returns the byte offset of this conditional expression's "if".
func (n *Cond) Pos() int { return n.Offset }

// Pos returns the byte offset of this list literal's opening bracket.
func (n *ListLit) Pos() int { return n.Offset }

// Pos returns the byte offset of this subscript's opening bracket.
func (n *Index) Pos() int { return n.Offset }

// Pos returns the byte offset of this slice's opening bracket.
func (n *Slice) Pos() int { return n.Offset }

// Pos returns the byte offset of this comprehension's opening bracket.
func (n *ListComp) Pos() int { return n.Offset }

// Pos returns the byte offset of this property access's dot.
func (n *Access) Pos() int { return n.Offset }

// Pos returns the byte offset of this call's opening parenthesis.
func (n *Call) Pos() int { return n.Offset }

func (*IntLit) node()    {}
func (*FloatLit) node()  {}
func (*StringLit) node() {}
func (*BoolLit) node()   {}
func (*NullLit) node()   {}
func (*Name) node()      {}
func (*Unary) node()     {}
func (*Binary) node()    {}
func (*Compare) node()   {}
func (*Logical) node()   {}
func (*Cond) node()      {}
func (*ListLit) node()   {}
func (*Index) node()     {}
func (*Slice) node()     {}
func (*ListComp) node()  {}
func (*Access) node()    {}
func (*Call) node()      {}

// walk calls fn for n and every node beneath it, parents before children and
// siblings left to right — the same order the recursive version visited in.
//
// It is ITERATIVE, with an explicit stack, and that is the whole point. The
// recursive version was the last unbounded recursion in the package: the
// parser and the evaluator both carry depth bounds (limits.go's maxParseDepth
// and maxEvalDepth) because exhausting the Go stack is a runtime.throw that
// recover() cannot catch, and walk had no such bound — 1,000,000 operators
// walked fine, 10,000,000 killed the process outright with "fatal error: stack
// overflow". Expression.Names is its only caller, and sub-project E calls that
// on every expression in a template, so the hazard was on its way to being
// reachable.
//
// A depth bound was the other option and is the wrong one here. The parser and
// the evaluator bound their recursion because they genuinely need to REJECT —
// both are handed source that may be hostile, and both already return errors.
// Walking a tree that has already parsed cannot fail on anything: every node
// came from the bounded parser, Names returns []string with no error channel,
// and a bound would therefore have to either lie by truncating the name set or
// panic. An explicit stack removes the failure mode rather than converting it
// into one, and for this node set it costs one small helper.
//
// The stack is LIFO, so children are pushed in reverse to preserve the
// left-to-right order fn used to see. Nothing depends on that today (Names
// sorts its result), which is exactly why it is stated here rather than left
// to be rediscovered.
func walk(n Node, fn func(Node)) {
	if n == nil {
		return
	}
	stack := []Node{n}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		fn(cur)
		stack = pushChildren(stack, cur)
	}
}

// pushChildren appends n's child nodes to a walk stack in REVERSE order, so
// that popping them yields the leftmost child first. A nil child — an absent
// slice component, which section 1.3.8 distinguishes from an explicit zero — is
// skipped rather than pushed, since the caller's fn must never be handed one.
func pushChildren(stack []Node, n Node) []Node {
	push := func(children ...Node) {
		for _, child := range slices.Backward(children) {
			if child != nil {
				stack = append(stack, child)
			}
		}
	}
	switch v := n.(type) {
	case *Unary:
		push(v.X)
	case *Binary:
		push(v.L, v.R)
	case *Logical:
		push(v.L, v.R)
	case *Cond:
		push(v.Then, v.If, v.Else)
	case *Compare:
		push(v.Operands...)
	case *ListLit:
		push(v.Elems...)
	case *Index:
		push(v.X, v.Idx)
	case *Slice:
		push(v.X, v.Start, v.Stop, v.Step)
	case *ListComp:
		push(v.Elem, v.Iter, v.Cond)
	case *Access:
		push(v.X)
	case *Call:
		push(append([]Node{v.Callee}, v.Args...)...)
	}
	return stack
}
