// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"fmt"
	"runtime"
)

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
// boundary, and inside operator dispatch when a shape is matched. It propagates
// into a sub-expression from exactly four node kinds and no others — the
// package doc's forwarding table is the single statement of which, and of why
// each row is what it is; do not restate it here, and consult it rather than
// this comment when the question is whether some position takes a target.
// Section 1.3.1 leaves the question open, and the table's governing rule is
// that a node forwards only when its result literally IS a sub-expression's
// value: propagating a string target into "Param.Count + 1" would concatenate
// its operands into "11" rather than adding them, and section 1.3.2's own
// example of "Count: {{ len(myList) }}" yielding "Count: 5" describes
// evaluating naturally and converting afterward.
func (e *Expression) Eval(syms Symbols, target Type, opts ...Option) (Value, error) {
	ec := newEvalCtx(e.src, syms, opts)
	if ec.limitErr != nil {
		return Value{}, ec.limitErr
	}
	// The target is threaded inward for the node kinds that forward it (see
	// evalNode); the boundary coercion below still applies to the result of the
	// whole expression, whether or not any node consumed the target.
	v, err := evalNode(e.root, ec, target, 0)
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
func Eval(src string, syms Symbols, target Type, opts ...Option) (Value, error) {
	e, err := Parse(src)
	if err != nil {
		return Value{}, err
	}
	return e.Eval(syms, target, opts...)
}

// PathFormat selects the semantics the path type uses during evaluation.
//
// The specification makes this an evaluator setting rather than a property of
// the host, and that distinction is load-bearing here: sqi parses templates
// server-side, so deriving it from the running machine would let one template
// expand into different tasks depending on which OS submitted it.
type PathFormat int

const (
	// PathPOSIX is Python's PurePosixPath. It is the DEFAULT, which differs
	// from the specification's own default of host-native on purpose — the
	// spec itself names POSIX as what TEMPLATE scope wants, "to ensure
	// consistent behavior regardless of the submission machine's OS", and
	// template parsing is exactly that scope.
	PathPOSIX PathFormat = iota
	// PathWindows is Python's PureWindowsPath.
	PathWindows
	// PathNative is the specification's own default: the host's semantics.
	// Nothing in sqi selects it yet; sub-project E does, for host contexts.
	PathNative
)

// resolve turns PathNative into a real flavor. Everything downstream sees
// POSIX or Windows and never has to ask again.
func (f PathFormat) resolve() PathFormat {
	if f != PathNative {
		return f
	}
	if runtime.GOOS == "windows" {
		return PathWindows
	}
	return PathPOSIX
}

// Option configures one evaluation.
//
// It is variadic so that existing three-argument calls keep compiling, and so
// sub-project E can add the section 1.3.9 and 1.3.10 limits through the same
// channel rather than changing the signature a second time.
type Option func(*evalCtx)

// WithPathFormat selects the path semantics for this evaluation.
func WithPathFormat(f PathFormat) Option {
	return func(ec *evalCtx) { ec.pathFormat = f.resolve() }
}

// WithPathMapping supplies the session's path-mapping rules to apply_path_mapping.
// Absent, apply_path_mapping passes its input through, NORMALIZED as a path value
// in the evaluation's flavor — passthrough means "no rule rewrote it", not "the
// text is returned verbatim", so '/a//b/' still comes back as the path "/a/b".
// Sub-project E injects the rules per host-context scope; before E, no production
// caller sets this, so apply_path_mapping is passthrough everywhere it is
// currently reached.
func WithPathMapping(rules []PathMapRule) Option {
	return func(ec *evalCtx) { ec.pathMapping = rules }
}

// WithMemoryLimit bounds the LIVE memory, in bytes, of the values one
// evaluation holds at once (section 1.3.9). It defaults to 100,000,000.
//
// There is no unlimited mode: a non-positive limit is a caller error, reported
// by Eval. A value that reads like "off" must not BE off in a package reachable
// from POST /api/v1/jobs. Tests wanting effectively-unbounded evaluation pass a
// large number.
//
// Note that limits.go's fixed bounds sit UNDERNEATH this one and are not
// configurable, so raising this limit does not raise what a single operation may
// allocate: "'a' * 20000000" still fails on maxStringBytes with errTooLarge, not
// on errMemoryLimit.
func WithMemoryLimit(bytes int64) Option {
	return func(ec *evalCtx) {
		if bytes <= 0 {
			ec.limitErr = fmt.Errorf("memory limit must be positive, got %d", bytes)
			return
		}
		ec.m.memLimit = bytes
	}
}

// WithOperationLimit bounds the number of operations one evaluation may perform
// (section 1.3.10). It defaults to 10,000,000. A non-positive limit is a caller
// error, for the same reason WithMemoryLimit's is.
func WithOperationLimit(ops int64) Option {
	return func(ec *evalCtx) {
		if ops <= 0 {
			ec.limitErr = fmt.Errorf("operation limit must be positive, got %d", ops)
			return
		}
		ec.m.opLimit = ops
	}
}

// evalCtx is the state one evaluation threads through every node.
//
// It bundles what used to be two parameters on seventeen functions. The point
// is not brevity: sub-project E must thread operation and memory counters the
// same way.
//
// CORRECTION (sub-project E1): an earlier revision of this comment said that
// sub-project E would thread its operation and memory counters here and that
// "a struct absorbs those as fields instead of as another sweep over every
// signature". The first half is right and the second is WRONG, and the error
// was not harmless. evalCtx flows by VALUE, so a counter stored as a plain
// field would be incremented on a copy and discarded when the callee returned:
// the counter would read near zero however much work an expression did, and
// every test written against it would pass. The counters live behind the
// POINTER field m instead, which is what makes them accumulate without the
// signature sweep the original claim was trying to avoid.
type evalCtx struct {
	src        string
	syms       Symbols
	pathFormat PathFormat
	// pathMapping holds the session's path-mapping rules for apply_path_mapping.
	// Nil (the default) makes apply_path_mapping pass its input through, still
	// re-parsed as a path in pathFormat — see WithPathMapping.
	pathMapping []PathMapRule
	// m carries the section 1.3.9 and 1.3.10 budgets. It is a POINTER because
	// evalCtx is copied at every call boundary -- see meter's doc comment.
	// newEvalCtx always sets it, so it is never nil during evaluation.
	m *meter
	// limitErr records a caller error in the limit options, reported by Eval
	// rather than by panicking inside an Option closure.
	limitErr error
}

func newEvalCtx(src string, syms Symbols, opts []Option) evalCtx {
	if syms == nil {
		syms = MapSymbols(nil)
	}
	ec := evalCtx{src: src, syms: syms, pathFormat: PathPOSIX}
	ec.m = newMeter(defaultMemoryLimit, defaultOperationLimit)
	for _, o := range opts {
		o(&ec)
	}
	return ec
}

// evalNode dispatches on node type, counting one level of evaluation recursion
// against maxEvalDepth on the way in.
//
// The parser's own depth guard does NOT cover this. A left-deep tree costs the
// parser no recursion at all — parseBinaryLevel and parseLogicalLevel build one
// in a LOOP — so "true or true or true or …" and "1 + 1 + 1 + …" parse happily
// at any length and then overflow the Go stack here, in evalBinary's and
// evalLogical's descent into their left operand. Measured before this guard
// existed: both died with "fatal error: stack overflow" between 500,000 and
// 600,000 operators, which is a runtime.throw that recover() cannot catch, so it
// has to be turned into a value before it happens. See maxEvalDepth.
//
// target is the type the surrounding context expects (section 1.3.1). It is
// forwarded ONLY by the node kinds whose result IS a sub-expression's value —
// see the forwarding table in evalCond, evalLogical and evalListLit. Nodes that
// COMPUTE a value pass TAny to their operands: propagating a string target into
// "Param.Count + 1" would concatenate its operands into "11" rather than adding
// them.
//
// Two conventions coexist for the depth ARGUMENT a descent passes to this
// function. evalUnary, evalBinary, evalCompare, evalLogical, evalCond,
// evalListLit, evalIndex and evalSlice pass depth straight through, so each
// AST level costs exactly the one unit counted above. The Access arm below,
// evalCall's receiver and argument descents (call.go), and every descent in
// comp.go instead pass depth+1, so a chain of Access, Call or comprehension
// nodes costs TWO units per level rather than one — an "Access chain" ~5,000
// deep reaches maxEvalDepth where a subscript chain needs ~10,000. This split
// was inherited, not designed: it was not unified when Access, Call and the
// comprehension were added, and is left as found rather than fixed here now
// that it has been noticed, because the effect is strictly
// CONSERVATIVE (it can only reject a legal-depth expression earlier, never let
// one past the point where the Go stack would actually be at risk — see the
// measurements above), and unifying it touches three files' worth of already
// well-tested recursion accounting for a correctness property nothing
// currently depends on. Revisit if sub-project E's own configurable depth
// limit makes the discrepancy user-visible.
func evalNode(n Node, ec evalCtx, target Type, depth int) (Value, error) {
	if depth >= maxEvalDepth {
		return Value{}, errorAt(ec.src, n.Pos(), "this expression is nested too deeply to evaluate")
	}
	return evalDispatch(n, ec, target, depth+1)
}

// evalDispatch is evalNode's type switch, split out so that the depth check and
// the dispatch each stay under the repo's complexity cap. Call evalNode, never
// this: entering here does not count the frame.
//
// The five leaf literal kinds are peeled off into evalLiteral first — adding
// *ListComp as a fifteenth case here pushed the switch itself over cyclop's
// cap, so the literals (which need none of this function's parameters beyond
// n) move out rather than the cap moving up.
func evalDispatch(n Node, ec evalCtx, target Type, depth int) (Value, error) {
	if v, ok := evalLiteral(n); ok {
		return v, nil
	}
	switch v := n.(type) {
	case *Name:
		return evalName(v, ec, depth)
	case *Unary:
		return evalUnary(v, ec, depth)
	case *Binary:
		return evalBinary(v, ec, depth)
	case *Compare:
		return evalCompare(v, ec, depth)
	case *Logical:
		return evalLogical(v, ec, target, depth)
	case *Cond:
		return evalCond(v, ec, target, depth)
	case *ListLit:
		return evalListLit(v, ec, target, depth)
	case *Index:
		return evalIndex(v, ec, depth)
	case *Slice:
		return evalSlice(v, ec, depth)
	case *ListComp:
		return evalListComp(v, ec, target, depth)
	case *Call:
		return evalCall(v, ec, depth)
	case *Access:
		recv, err := evalNode(v.X, ec, TAny, depth+1)
		if err != nil {
			return Value{}, err
		}
		return evalProperty(recv, v.Attr, ec, v.Offset, depth)
	}
	return Value{}, errorAt(ec.src, n.Pos(), "internal error: cannot evaluate %T", n)
}

// evalLiteral evaluates a leaf literal node. ok is false for any other node
// kind, so evalDispatch falls through to its own switch.
func evalLiteral(n Node) (v Value, ok bool) {
	switch lit := n.(type) {
	case *IntLit:
		return Int(lit.Val), true
	case *FloatLit:
		return Float(lit.Val), true
	case *StringLit:
		return String(lit.Val), true
	case *BoolLit:
		return Bool(lit.Val), true
	case *NullLit:
		return Null(), true
	}
	return Value{}, false
}

// evalName evaluates a dotted name: a symbol, optionally followed by property
// accesses. See resolveName for why the split happens here rather than in the
// parser.
func evalName(n *Name, ec evalCtx, depth int) (Value, error) {
	r, ok := resolveName(n, ec.syms)
	if !ok {
		// Name the longest candidate: it is what the author wrote, and a
		// shorter prefix would misreport which part is unknown.
		return Value{}, errorAt(ec.src, n.Offset, "unknown symbol %q", n.String())
	}
	return evalProperties(r.Val, r.Rest, ec, n.Offset, depth)
}

func evalUnary(n *Unary, ec evalCtx, depth int) (Value, error) {
	x, err := evalNode(n.X, ec, TAny, depth)
	if err != nil {
		return Value{}, err
	}
	out, err := applyUnary(ec, n.Op, x)
	if err != nil {
		return Value{}, wrapAt(ec.src, n.Offset, err)
	}
	return out, nil
}

func evalBinary(n *Binary, ec evalCtx, depth int) (Value, error) {
	l, err := evalNode(n.L, ec, TAny, depth)
	if err != nil {
		return Value{}, err
	}
	r, err := evalNode(n.R, ec, TAny, depth)
	if err != nil {
		return Value{}, err
	}
	out, err := applyBinary(ec, n.Op, l, r)
	if err != nil {
		// n.Offset is the operator's own position, so the error blames the "+"
		// rather than the start of the expression.
		return Value{}, wrapAt(ec.src, n.Offset, err)
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
func evalCompare(n *Compare, ec evalCtx, depth int) (Value, error) {
	left, err := evalNode(n.Operands[0], ec, TAny, depth)
	if err != nil {
		return Value{}, err
	}
	for i, op := range n.Ops {
		right, err := evalNode(n.Operands[i+1], ec, TAny, depth)
		if err != nil {
			return Value{}, err
		}
		out, err := applyBinary(ec, op, left, right)
		if err != nil {
			// n.OpOffsets[i] is THIS link's own operator, not the chain's
			// first one (n.Offset) — a chain of three or more operators must
			// blame whichever link actually failed.
			return Value{}, wrapAt(ec.src, n.OpOffsets[i], err)
		}
		// A link whose operands are not all known yields an unknown bool, so the
		// chain's outcome is unknown too. Stop here: the remaining operands
		// cannot change the answer, and evaluating them would only risk a
		// spurious error from a branch that may never run.
		if out.IsUnresolved() {
			return Unresolved(TBool), nil
		}
		if out.Type.Code != CodeBool {
			return Value{}, wrapAt(ec.src, n.OpOffsets[i],
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
func evalLogical(n *Logical, ec evalCtx, target Type, depth int) (Value, error) {
	left, err := evalNode(n.L, ec, target, depth)
	if err != nil {
		return Value{}, err
	}
	if left.IsUnresolved() {
		right, err := evalNode(n.R, ec, target, depth)
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
	return evalNode(n.R, ec, target, depth)
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
func evalCond(n *Cond, ec evalCtx, target Type, depth int) (Value, error) {
	cond, err := evalNode(n.If, ec, TBool, depth)
	if err != nil {
		return Value{}, err
	}
	if cond.IsUnresolved() {
		if !includes(cond.Type, CodeBool) {
			return Value{}, errorAt(ec.src, n.If.Pos(),
				"the condition of a conditional expression must be a bool, found %s", cond.Type)
		}
		return condResult(n, ec, target, depth)
	}
	if cond.Type.Code != CodeBool {
		return Value{}, errorAt(ec.src, n.If.Pos(),
			"the condition of a conditional expression must be a bool, found %s", cond.Type)
	}
	if cond.AsBool() {
		return evalNode(n.Then, ec, target, depth)
	}
	return evalNode(n.Else, ec, target, depth)
}

// condResult evaluates both branches of a conditional whose condition is not yet
// known, per the spec's "Conditional Expressions with Unknown Conditions" rule.
//
// A branch that fails could never have produced a value at runtime either, so
// its error is suppressed and the other branch's type stands alone. Only when
// BOTH fail is there a real error, and then it names both — a reader cannot tell
// which branch was meant.
func condResult(n *Cond, ec evalCtx, target Type, depth int) (Value, error) {
	thenVal, thenErr := evalNode(n.Then, ec, target, depth)
	elseVal, elseErr := evalNode(n.Else, ec, target, depth)
	switch {
	case thenErr == nil && elseErr == nil:
		return Unresolved(UnionOf(thenVal.Type, elseVal.Type)), nil
	case thenErr == nil:
		return Unresolved(thenVal.Type), nil
	case elseErr == nil:
		return Unresolved(elseVal.Type), nil
	}
	return Value{}, errorAt(ec.src, n.Offset,
		"both branches of this conditional expression fail: %v; and %v", thenErr, elseErr)
}
