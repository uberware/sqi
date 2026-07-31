// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// render prints a tree as a fully parenthesized prefix form, so a test can
// state the shape a precedence rule must produce without asserting on struct
// literals. "1 + 2 * 3" renders as "(+ 1 (* 2 3))".
func render(n Node) string {
	switch v := n.(type) {
	case *IntLit:
		return strconv.FormatInt(v.Val, 10)
	case *FloatLit:
		return strconv.FormatFloat(v.Val, 'g', -1, 64)
	case *StringLit:
		return strconv.Quote(v.Val)
	case *BoolLit:
		if v.Val {
			return "true"
		}
		return "false"
	case *NullLit:
		return "null"
	case *Name:
		return v.String()
	case *Unary:
		return "(" + v.Op.String() + " " + render(v.X) + ")"
	case *Binary:
		return "(" + v.Op.String() + " " + render(v.L) + " " + render(v.R) + ")"
	case *Logical:
		return "(" + v.Op.String() + " " + render(v.L) + " " + render(v.R) + ")"
	case *Cond:
		return "(if " + render(v.If) + " " + render(v.Then) + " " + render(v.Else) + ")"
	case *Compare:
		parts := []string{"cmp"}
		for i, op := range v.Ops {
			parts = append(parts, render(v.Operands[i]), op.String())
		}
		parts = append(parts, render(v.Operands[len(v.Operands)-1]))
		return "(" + strings.Join(parts, " ") + ")"
	}
	return "?"
}

func mustRender(t *testing.T, src string) string {
	t.Helper()
	e, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	return render(e.Root())
}

func TestParse_Literals(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"integer", "42", "42"},
		{"float", "3.5", "3.5"},
		{"string", `'hi'`, `"hi"`},
		{"python true", "True", "true"},
		{"python false", "False", "false"},
		{"python none", "None", "null"},
		{"json true alias", "true", "true"},
		{"json false alias", "false", "false"},
		{"json null alias", "null", "null"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mustRender(t, tt.src); got != tt.want {
				t.Errorf("Parse(%q) = %s; want %s", tt.src, got, tt.want)
			}
		})
	}
}

func TestParse_Names(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"bare", "Frame", "Frame"},
		{"dotted", "Param.Foo", "Param.Foo"},
		{"deeply dotted", "Task.Param.Frame", "Task.Param.Frame"},
		// Section 1.1.3: keywords are contextual and remain valid attribute
		// names. Fixture expr1.1.3--contextual-keywords depends on all of these.
		{"keyword attribute if", "Param.if", "Param.if"},
		{"keyword attribute else", "Param.else", "Param.else"},
		{"keyword attribute and", "Param.and", "Param.and"},
		{"keyword attribute not", "Param.not", "Param.not"},
		{"keyword attribute for", "Param.for", "Param.for"},
		{"keyword attribute in", "Param.in", "Param.in"},
		{"keyword attribute True", "Param.True", "Param.True"},
		{"keyword attribute None", "Param.None", "Param.None"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mustRender(t, tt.src); got != tt.want {
				t.Errorf("Parse(%q) = %s; want %s", tt.src, got, tt.want)
			}
		})
	}
}

func TestParse_ArithmeticPrecedence(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"mul binds tighter than add", "1 + 2 * 3", "(+ 1 (* 2 3))"},
		{"add is left associative", "1 - 2 - 3", "(- (- 1 2) 3)"},
		{"mul is left associative", "8 / 4 / 2", "(/ (/ 8 4) 2)"},
		{"floor division and modulo sit with mul", "7 // 2 % 3", "(% (// 7 2) 3)"},
		{"parentheses override precedence", "(1 + 2) * 3", "(* (+ 1 2) 3)"},
		{"power binds tighter than mul", "2 * 3 ** 4", "(* 2 (** 3 4))"},
		{"power is right associative", "2 ** 3 ** 4", "(** 2 (** 3 4))"},
		{"unary minus binds looser than power", "-2 ** 2", "(- (** 2 2))"},
		{"power accepts a unary right operand", "2 ** -1", "(** 2 (- 1))"},
		{"unary plus", "+Param.X", "(+ Param.X)"},
		{"stacked unary", "--1", "(- (- 1))"},
		{"newlines are whitespace", "Param.X +\n  1", "(+ Param.X 1)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mustRender(t, tt.src); got != tt.want {
				t.Errorf("Parse(%q) = %s; want %s", tt.src, got, tt.want)
			}
		})
	}
}

func TestParse_Rejected(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantMsg string
	}{
		{"empty", "", "empty expression"},
		{"whitespace only", "   ", "empty expression"},
		{"trailing operator", "1 +", "unexpected end of expression"},
		{"unclosed paren", "(1 + 2", "unexpected end of expression"},
		{"tuple", "(1, 2)", `expected ")"`},
		{"trailing garbage", "1 2", "unexpected integer literal after expression"},
		{"f-string", `f'{x}'`, "unexpected string literal after expression"},
		{"lambda", "lambda x: x", "unexpected name after expression"},
		{"ellipsis", "...", "unexpected"},
		{"keyword in primary position", "1 + else", `unexpected keyword "else"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.src)
			if err == nil {
				t.Fatalf("Parse(%q) = nil error; want an error", tt.src)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q; want it to contain %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestParse_ErrorCarriesPosition(t *testing.T) {
	// A subscript used to be the rejected construct here, then a call (both
	// now parse — B2 and this task, respectively). A trailing comma in a call's
	// argument list takes their place: section 1.1.2's <ArgList> has no
	// optional trailing comma, unlike <ListExpr>, so it remains a genuine parse
	// error carrying a meaningful, non-zero offset.
	_, err := Parse("Param.X + Param.X(0,)")
	if err == nil {
		t.Fatal("Parse = nil error; want an error")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is %T; want *Error", err)
	}
	if e.Offset != 20 {
		t.Errorf("Offset = %d; want 20 (the closing \")\" after the trailing comma)", e.Offset)
	}
}

func TestExpression_Source(t *testing.T) {
	const src = "1 + 2"
	e, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if e.Source() != src {
		t.Errorf("Source() = %q; want %q", e.Source(), src)
	}
}

func TestParse_ComparisonAndLogical(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"simple comparison", "1 < 2", "(cmp 1 < 2)"},
		{"chained comparison", "1 < 2 < 3", "(cmp 1 < 2 < 3)"},
		{"long chain", "1 <= 2 == 2 != 3 >= 0", "(cmp 1 <= 2 == 2 != 3 >= 0)"},
		{"arithmetic binds tighter than comparison", "1 + 2 < 4", "(cmp (+ 1 2) < 4)"},
		{"membership", "'a' in Param.S", `(cmp "a" in Param.S)`},
		{"negated membership", "'a' not in Param.S", `(cmp "a" not in Param.S)`},
		{"not binds looser than comparison", "not 1 < 2", "(not (cmp 1 < 2))"},
		{"stacked not", "not not true", "(not (not true))"}, //nolint:dupword // "not not" is a legitimate double negation, not a typo
		{"and binds tighter than or", "a or b and c", "(or a (and b c))"},
		{"and is left associative", "a and b and c", "(and (and a b) c)"},
		{"or is left associative", "a or b or c", "(or (or a b) c)"},
		{"not binds tighter than and", "not a and b", "(and (not a) b)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mustRender(t, tt.src); got != tt.want {
				t.Errorf("Parse(%q) = %s; want %s", tt.src, got, tt.want)
			}
		})
	}
}

func TestParse_Conditional(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"simple", "1 if true else 2", "(if true 1 2)"},
		{"right associative", "1 if a else 2 if b else 3", "(if a 1 (if b 2 3))"},
		{"condition is an or expression", "1 if a or b else 2", "(if (or a b) 1 2)"},
		{"branches take whole expressions", "x + 1 if c else y * 2", "(if c (+ x 1) (* y 2))"},
		{"json aliases in a conditional", "'--flag' if true else null", `(if true "--flag" null)`},
		{
			"contextual keyword attributes around keywords",
			"Param.if if Param.flag else Param.else", "(if Param.flag Param.if Param.else)",
		},
		{
			"spans lines", "\"high\"\n  if Param.Q == \"final\"\n  else \"low\"",
			`(if (cmp Param.Q == "final") "high" "low")`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mustRender(t, tt.src); got != tt.want {
				t.Errorf("Parse(%q) = %s; want %s", tt.src, got, tt.want)
			}
		})
	}
}

func TestParse_ConditionalRejected(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantMsg string
	}{
		{"missing else", "1 if true", `expected "else"`},
		{"missing else branch", "1 if true else", "unexpected end of expression"},
		{"missing condition", "1 if else 2", `unexpected keyword "else"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.src)
			if err == nil {
				t.Fatalf("Parse(%q) = nil error; want an error", tt.src)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q; want it to contain %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestParse_CompareNodeShape(t *testing.T) {
	// A chain must be one Compare node so each intermediate value is evaluated
	// once (section 1.3.6), not nested Binary nodes.
	e, err := Parse("1 < 2 < 3")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cmp, ok := e.Root().(*Compare)
	if !ok {
		t.Fatalf("root is %T; want *Compare", e.Root())
	}
	if len(cmp.Ops) != 2 || len(cmp.Operands) != 3 {
		t.Fatalf("got %d ops and %d operands; want 2 and 3", len(cmp.Ops), len(cmp.Operands))
	}
	if cmp.Offset != 2 {
		t.Errorf("Offset = %d; want 2 (the first \"<\")", cmp.Offset)
	}
}

func TestExpression_Names(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"no names", "1 + 2", nil},
		{"one name", "Param.X + 1", []string{"Param.X"}},
		{"sorted and deduplicated", "Param.Z + Param.A + Param.Z", []string{"Param.A", "Param.Z"}},
		{
			"across every node kind", "Param.A if Param.B and Param.C < Param.D else -Param.E",
			[]string{"Param.A", "Param.B", "Param.C", "Param.D", "Param.E"},
		},
		{"keyword attributes are ordinary names", "Param.if", []string{"Param.if"}},
		// A property chain is part of the symbol: the Recommended Library
		// Interface's own example is "Param.File.stem collects
		// {"Param.File.stem"}".
		{"a trailing property stays in the name", "Param.File.stem", []string{"Param.File.stem"}},
		// A function or method name is not a symbol at all — the spec puts
		// those in a separate called_functions set, which this package
		// deliberately does not build (sub-project D's need).
		{"a plain function name is not a symbol", "len(Param.Items)", []string{"Param.Items"}},
		{"a method name is not a symbol", "Param.Name.upper()", []string{"Param.Name"}},
		{"a method on a deep name keeps every other segment", "a.b.c()", []string{"a.b"}},
		{"a method on a literal contributes nothing", "[1, 2].len()", nil},
		{"a parenthesized callee is split the same way", "(Param.F)(1)", []string{"Param"}},
		{"a call argument is still collected", "len(Param.A) + upper(Param.B)", []string{"Param.A", "Param.B"}},
		// A comprehension binds its loop variable, so neither it nor anything
		// rooted at it is external.
		{"a loop variable is not a symbol", "[x for x in Param.Items]", []string{"Param.Items"}},
		{"a property of a loop variable is not a symbol", "[x.stem for x in Param.Files]", []string{"Param.Files"}},
		{"a method call on a loop variable is not a symbol", "[x.upper() for x in Param.Words]", []string{"Param.Words"}},
		{"a loop variable in a filter is not a symbol", "[x for x in Param.A if x.n > 1]", []string{"Param.A"}},
		{"a call inside a comprehension body loses only its own name", "[len(x) for x in Param.Items]", []string{"Param.Items"}},
		// The iterable is evaluated BEFORE the loop variable exists, so a name
		// there is external even when it spells the loop variable.
		{"the iterable is outside the binding", "[x for x in x]", []string{"x"}},
		{"a different loop variable leaves the element external", "[x for y in Param.A]", []string{"Param.A", "x"}},
		{"nested comprehensions bind independently", "[[y for y in x] for x in Param.M]", []string{"Param.M"}},
		// The binding does not leak past the comprehension that made it. This
		// is the one case here where the reference implementation answers
		// differently — it reports {"Param.A"} alone, as if every loop variable
		// name were removed from the whole expression — and the spec's
		// "external symbols" wording is on this side: the trailing "[x]" is a
		// free reference, exactly as it is in Python, where a comprehension's
		// scope is its own.
		{"a name after a comprehension is external again", "[x for x in Param.A] + [x]", []string{"Param.A", "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := Parse(tt.src)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.src, err)
			}
			got := e.Names()
			if len(got) != len(tt.want) {
				t.Fatalf("Names() = %v; want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("Names() = %v; want %v", got, tt.want)
				}
			}
		})
	}
}

// TestParse_RejectsPythonBeyondEXPR runs the exact expression bodies from the
// EXPR conformance suite's expr1.1--reject-* fixtures. EXPR's grammar is a
// subset of Python's, and the failure direction matters: a reader that
// accepted any of these would let sqi admit a template it cannot evaluate.
func TestParse_RejectsPythonBeyondEXPR(t *testing.T) {
	tests := []struct {
		fixture string
		src     string
	}{
		{"reject-await", "await Param.X"},
		{"reject-bitwise-and", "5 & 3"},
		{"reject-bitwise-not", "~5"},
		{"reject-bitwise-or", "5 | 3"},
		{"reject-bitwise-xor", "5 ^ 3"},
		{"reject-bstring", "b'hello'"},
		{"reject-complex-literal", "1+2j"},
		{"reject-dict-comp", "{k: k for k in [1,2]}"},
		{"reject-dict-literal", "{'a': 1}"},
		{"reject-double-star-arg", "len(**Items)"},
		{"reject-ellipsis", "..."},
		{"reject-fstring", "f'hello'"},
		{"reject-generator-expr", "(x for x in [1,2])"},
		{"reject-is-not-operator", "Param.X is not None"},
		{"reject-is-operator", "Param.X is None"},
		{"reject-keyword-arg", "len(x=1)"},
		{"reject-lambda", "lambda x: x + 1"},
		{"reject-left-shift", "5 << 1"},
		{"reject-matmul", "Param.X @ Param.X"},
		{"reject-multi-generator", "[x + y for x in [1,2] for y in [3,4]]"},
		{"reject-multi-if-comp", "[x for x in [1,2,3,4,5] if x > 1 if x < 5]"},
		{"reject-right-shift", "5 >> 1"},
		{"reject-set-comp", "{x for x in [1,2]}"},
		{"reject-set-literal", "{1, 2, 3}"},
		{"reject-star-arg", "len(*Items)"},
		{"reject-star-unpack", "[*[1,2], 3]"},
		{"reject-tuple", "(1, 2, 3)"},
		{"reject-tuple-unpack-comp", "[a + b for a, b in [[1,2], [3,4]]]"},
		{"reject-walrus", "(x := 5)"},
		{"unclosed-bracket", "[1, 2, 3"},
		{"unclosed-paren", "(1 + 2"},
		{"syntax-error", "1 + "},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			e, err := Parse(tt.src)
			if err == nil {
				t.Fatalf("Parse(%q) accepted the expression (root %T); want a syntax error", tt.src, e.Root())
			}
			var pe *Error
			if !errors.As(err, &pe) {
				t.Errorf("error is %T; want *Error carrying a position", err)
			}
		})
	}
}

// TestParse_AcceptsWhatEXPRRequires is the counterpart: sub-project A's
// grammar must not have become so strict that it rejects valid expressions.
func TestParse_AcceptsWhatEXPRRequires(t *testing.T) {
	for _, src := range []string{
		"Param.X + 3", "Param.X // 3", "2 ** 3", "-Param.X", "(Param.X + 1) * 2",
		"true", "false", "null", "True", "False", "None",
		"Param.if if Param.flag else Param.else",
		"Param.not if not false else Param.None",
		"0xFF_FF", "0b1010_1010", "0o52", "1_000_000", "1.5e-3", ".5",
		`r'C:\renders\shot'`, `'''multi\nline'''`,
		"1 < 2 < 3", "'a' in Param.S", "'a' not in Param.S",
		"Param.A or 'fallback'", "Param.A and Param.B",
	} {
		t.Run(src, func(t *testing.T) {
			if _, err := Parse(src); err != nil {
				t.Errorf("Parse(%q) = %v; want it to parse", src, err)
			}
		})
	}
}

func TestWalk_DescendsIntoCollectionNodes(t *testing.T) {
	inner := &IntLit{Offset: 1, Val: 1}
	tests := []struct {
		name string
		node Node
		want int // total nodes visited, including the root
	}{
		{"list literal", &ListLit{Offset: 0, Elems: []Node{inner, inner}}, 3},
		{"index", &Index{Offset: 0, X: inner, Idx: inner}, 3},
		{"slice with every component", &Slice{Offset: 0, X: inner, Start: inner, Stop: inner, Step: inner}, 5},
		{"slice with absent components", &Slice{Offset: 0, X: inner}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			walk(tc.node, func(Node, walkCtx) { count++ })
			if count != tc.want {
				t.Fatalf("walk visited %d nodes, want %d", count, tc.want)
			}
			if got := tc.node.Pos(); got != 0 {
				t.Fatalf("Pos() = %d, want 0", got)
			}
		})
	}
}

// TestWalk_UsesConstantStackDepth is the regression test for walk's rewrite
// from recursion to an explicit stack (ast.go). It asserts the property
// directly — the Go stack does not grow with the tree — rather than by walking
// a tree big enough to overflow it.
//
// That choice is deliberate, and follows the same reasoning
// TestListOperators_RepeatOverflow states for its own omitted case: the tree
// that actually distinguishes the two implementations is 10,000,000 operators
// deep, which costs 458 MB of heap and, under -race, 3.6 GB of resident memory.
// A test that OOM-kills the suite on a revert is worse than one that fails.
// The measurement itself was made by hand and is recorded here instead: with
// the recursive walk, 10,000,000 chained Binary nodes died with "fatal error:
// stack overflow" (the uncatchable runtime.throw, exactly as maxParseDepth and
// maxEvalDepth exist to prevent); with the iterative one they walk in 0.72 s.
//
// A reverted walk fails this test at any chain length past a few hundred,
// because each Binary node it descends through costs one real Go frame.
func TestWalk_UsesConstantStackDepth(t *testing.T) {
	var pcs [8192]uintptr
	deepestFrames := func(chain int) int {
		leaf := &IntLit{Val: 1}
		var root Node = leaf
		for range chain {
			root = &Binary{Op: OpAdd, L: root, R: leaf}
		}
		deepest := 0
		walk(root, func(Node, walkCtx) {
			if n := runtime.Callers(0, pcs[:]); n > deepest {
				deepest = n
			}
		})
		return deepest
	}
	shallow, deep := deepestFrames(100), deepestFrames(4000)
	if shallow != deep {
		t.Fatalf("walk reached %d stack frames on a 100-node chain and %d on a 4000-node chain; "+
			"want the same count — walk must not recurse", shallow, deep)
	}
}

func TestParse_ListLiteral(t *testing.T) {
	tests := []struct {
		src       string
		wantElems int
	}{
		{"[]", 0},
		{"[1]", 1},
		{"[1, 2, 3]", 3},
		{"[1, 2, 3,]", 3},
		{"[[1, 2], [3]]", 2},
		{"[1 + 2, Param.X if Param.C else 0]", 2},
		{"[\n  1,\n  2\n]", 2},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			e, err := Parse(tc.src)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.src, err)
			}
			lit, ok := e.root.(*ListLit)
			if !ok {
				t.Fatalf("Parse(%q) root = %T, want *ListLit", tc.src, e.root)
			}
			if len(lit.Elems) != tc.wantElems {
				t.Fatalf("Parse(%q) has %d elements, want %d", tc.src, len(lit.Elems), tc.wantElems)
			}
		})
	}
}

func TestParse_ListLiteralErrors(t *testing.T) {
	tests := []struct {
		src      string
		wantSubs string
	}{
		// EOF hits expect()'s existing "unexpected end of expression" wording
		// (same convention as the "unclosed paren" case above), not "expected
		// \"]\"" — there is no next token to point at.
		{"[1, 2", "unexpected end of expression"},
		{"[,]", "unexpected"},
		{"[1 2]", `"]"`},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			_, err := Parse(tc.src)
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want one mentioning %q", tc.src, tc.wantSubs)
			}
			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Fatalf("Parse(%q) error = %q, want it to mention %q", tc.src, err.Error(), tc.wantSubs)
			}
		})
	}
}

func TestParse_SubscriptAndSlice(t *testing.T) {
	tests := []struct {
		src  string
		want string // "index" or "slice", plus which slice components are present
	}{
		{"Param.X[0]", "index"},
		{"Param.X[-1]", "index"},
		{"[1, 2][0]", "index"},
		{"Param.X[0][1]", "index"},
		{"Param.X[1:3]", "slice start stop"},
		{"Param.X[:3]", "slice stop"},
		{"Param.X[2:]", "slice start"},
		{"Param.X[:]", "slice"},
		{"Param.X[::2]", "slice step"},
		{"Param.X[::-1]", "slice step"},
		{"Param.X[0:5:2]", "slice start stop step"},
		{"Param.X[-3:]", "slice start"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			e, err := Parse(tc.src)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.src, err)
			}
			got := describeTrailer(e.root)
			if got != tc.want {
				t.Fatalf("Parse(%q) produced %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// describeTrailer names the outermost trailer node and, for a slice, which of
// its components are present.
func describeTrailer(n Node) string {
	switch v := n.(type) {
	case *Index:
		return "index"
	case *Slice:
		out := "slice"
		if v.Start != nil {
			out += " start"
		}
		if v.Stop != nil {
			out += " stop"
		}
		if v.Step != nil {
			out += " step"
		}
		return out
	}
	return fmt.Sprintf("%T", n)
}

func TestParse_SubscriptErrors(t *testing.T) {
	tests := []struct {
		src      string
		wantSubs string
	}{
		// EOF hits expect()'s existing "unexpected end of expression" wording
		// (same convention as the "unclosed paren" and list-literal EOF cases
		// above), not "expected \"]\"" — there is no next token to point at.
		{"Param.X[0", "unexpected end of expression"},
		{"Param.X[]", "unexpected"},
		{"Param.X[1:2:3:4]", `"]"`},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			_, err := Parse(tc.src)
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want one mentioning %q", tc.src, tc.wantSubs)
			}
			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Fatalf("Parse(%q) error = %q, want it to mention %q", tc.src, err.Error(), tc.wantSubs)
			}
		})
	}
}

// TestParse_NestingDepthIsBounded asserts that unbounded nesting comes back as
// an ordinary parse error rather than killing the process.
//
// This is not the usual "assert the error message" test. Before maxParseDepth
// existed, each of these inputs — at a large enough depth — produced
// "fatal error: stack overflow", which is a runtime.throw and NOT a panic:
// recover() cannot catch it, no deferred function runs, and the whole process
// exits. So a regression here does not fail this test, it takes the test binary
// down with it, which is exactly the failure the guard prevents in sqi-server.
//
// The depths are kept just past the limit rather than near the depth that
// actually overflows: the point is that the guard fires first, and a test that
// had to build a 200,000-deep source to prove it would be measuring the stack
// rather than the bound.
func TestParse_NestingDepthIsBounded(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		depth int
	}{
		{"list literals", "[", 600},
		{"parentheses", "(", 600},
		// parseNot and parseUnary recurse into themselves without ever passing
		// through the entry production, so a guard placed only there would
		// leave both of these unbounded. Both were verified to overflow the
		// stack for real before the guard existed.
		{"not operators", "not ", 600},
		{"unary minus", "-", 600},
		// parsePower reads its exponent through parseUnary, which falls straight
		// back through to parsePower when the next token is not a sign. That
		// cycle passed through no guard at all until parsePower took one:
		// Parse(strings.Repeat("2**", 1000000) + "2") died with "fatal error:
		// stack overflow", taking the whole process with it.
		{"power operators", "2**", 600},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var src string
			switch tc.src {
			case "[":
				src = strings.Repeat("[", tc.depth) + "1" + strings.Repeat("]", tc.depth)
			case "(":
				src = strings.Repeat("(", tc.depth) + "1" + strings.Repeat(")", tc.depth)
			default:
				src = strings.Repeat(tc.src, tc.depth) + "1"
			}
			_, err := Parse(src)
			if err == nil {
				t.Fatalf("Parse of %d-deep %q = nil error, want a depth error", tc.depth, tc.name)
			}
			if !strings.Contains(err.Error(), "nested too deeply") {
				t.Fatalf("Parse of %d-deep %q error = %q, want it to mention nesting depth",
					tc.depth, tc.name, err.Error())
			}
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("Parse of %d-deep %q returned %T, want an *Error carrying a position",
					tc.depth, tc.name, err)
			}
		})
	}
}

// TestParse_NestingWithinTheBoundStillParses pins the other side of the guard:
// a depth comfortably under the limit must still parse, so the bound cannot be
// tightened into rejecting real expressions without a test noticing.
func TestParse_NestingWithinTheBoundStillParses(t *testing.T) {
	for _, depth := range []int{1, 10, 50} {
		t.Run(strconv.Itoa(depth), func(t *testing.T) {
			src := strings.Repeat("[", depth) + "1" + strings.Repeat("]", depth)
			if _, err := Parse(src); err != nil {
				t.Fatalf("Parse of %d-deep list literals = %v, want it to parse", depth, err)
			}
		})
	}
}

func TestWalk_DescendsIntoCallAndCompNodes(t *testing.T) {
	inner := &IntLit{Offset: 1, Val: 1}
	tests := []struct {
		name string
		node Node
		want int // total nodes visited, including the root
	}{
		{"comprehension without a filter", &ListComp{Offset: 0, Elem: inner, Var: "x", Iter: inner}, 3},
		{"comprehension with a filter", &ListComp{Offset: 0, Elem: inner, Var: "x", Iter: inner, Cond: inner}, 4},
		{"property access", &Access{Offset: 0, X: inner, Attr: "stem"}, 2},
		{"call with no arguments", &Call{Offset: 0, Callee: inner}, 2},
		{"call with arguments", &Call{Offset: 0, Callee: inner, Args: []Node{inner, inner}}, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			walk(tc.node, func(Node, walkCtx) { count++ })
			if count != tc.want {
				t.Fatalf("walk visited %d nodes, want %d", count, tc.want)
			}
			if got := tc.node.Pos(); got != 0 {
				t.Fatalf("Pos() = %d, want 0", got)
			}
		})
	}
}

func TestParse_Comprehension(t *testing.T) {
	tests := []struct {
		src      string
		wantVar  string
		wantCond bool
	}{
		{"[x for x in Param.Items]", "x", false},
		{"[x * 2 for x in Param.Items]", "x", false},
		{"[x for x in Param.Items if x > 2]", "x", true},
		{"[_i + 1 for _i in Param.Items]", "_i", false},
		{"[[y for y in x] for x in Param.Items]", "x", false},
		{"[a if b else c for x in Param.Items]", "x", false},
		{"[x for x in (a if b else c)]", "x", false},
		{"[\n  x\n  for x in Param.Items\n]", "x", false},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			e, err := Parse(tc.src)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.src, err)
			}
			comp, ok := e.root.(*ListComp)
			if !ok {
				t.Fatalf("root = %T, want *ListComp", e.root)
			}
			if comp.Var != tc.wantVar {
				t.Errorf("Var = %q, want %q", comp.Var, tc.wantVar)
			}
			if (comp.Cond != nil) != tc.wantCond {
				t.Errorf("Cond present = %v, want %v", comp.Cond != nil, tc.wantCond)
			}
			if comp.Elem == nil || comp.Iter == nil {
				t.Error("Elem and Iter must both be set")
			}
		})
	}
}

func TestParse_ComprehensionErrors(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		wantSubs string
	}{
		{"multi for", "[x + y for x in a for y in b]", "only one \"for\""},
		{"multi if", "[x for x in a if p if q]", "only one \"if\""},
		{"generator expression", "(x for x in [1,2])", "generator expressions are not supported"},
		{"uppercase loop variable", "[x for X in a]", "must start with a lowercase letter or underscore"},
		{"dotted loop variable", "[x for a.b in c]", "\"in\""},
		{"missing in", "[x for x a]", "\"in\""},
		{"missing iterable", "[x for x in]", "unexpected"},
		{"unclosed", "[x for x in a", "unexpected end of expression"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.src)
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want one mentioning %q", tc.src, tc.wantSubs)
			}
			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Fatalf("Parse(%q) error = %q, want it to mention %q", tc.src, err.Error(), tc.wantSubs)
			}
		})
	}
}

// TestParse_BracesStillDieInTheLexer pins that dict and set comprehensions need
// no parser work: "{" is not a token, so they fail before parsing. Section
// 1.3.7 supports neither, and the conformance suite has a fixture for each.
func TestParse_BracesStillDieInTheLexer(t *testing.T) {
	for _, src := range []string{"{k: k for k in [1,2]}", "{x for x in [1,2]}"} {
		_, err := Parse(src)
		if err == nil {
			t.Fatalf("Parse(%q) = nil error, want a lexer rejection", src)
		}
		if !strings.Contains(err.Error(), "unexpected character '{'") {
			t.Fatalf("Parse(%q) error = %q, want the lexer's brace rejection", src, err.Error())
		}
	}
}

func TestParse_CallAndAccess(t *testing.T) {
	tests := []struct {
		src  string
		want string // shape summary
	}{
		{"len(x)", "call(name:len, 1 arg)"},
		{"len()", "call(name:len, 0 args)"},
		{"max(a, b)", "call(name:max, 2 args)"},
		{"Param.Name.upper()", "call(name:Param.Name.upper, 0 args)"},
		{"[1, 2].len()", "call(access:len, 0 args)"},
		{"Param.File.stem", "name:Param.File.stem"},
		{"(x).y", "access:y"},
		{"Param.Name.upper().strip()", "call(access:strip, 0 args)"},
		{"len(x)[0]", "index"},
		{"f(g(x))", "call(name:f, 1 arg)"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			e, err := Parse(tc.src)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.src, err)
			}
			if got := describeCallShape(e.root); got != tc.want {
				t.Fatalf("Parse(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// describeCallShape summarizes the outermost node, so the test asserts the tree
// shape rather than re-implementing the parser.
func describeCallShape(n Node) string {
	switch v := n.(type) {
	case *Call:
		unit := "args"
		if len(v.Args) == 1 {
			unit = "arg"
		}
		switch callee := v.Callee.(type) {
		case *Name:
			return fmt.Sprintf("call(name:%s, %d %s)", callee.String(), len(v.Args), unit)
		case *Access:
			return fmt.Sprintf("call(access:%s, %d %s)", callee.Attr, len(v.Args), unit)
		default:
			return fmt.Sprintf("call(%T, %d %s)", v.Callee, len(v.Args), unit)
		}
	case *Access:
		return "access:" + v.Attr
	case *Name:
		return "name:" + v.String()
	case *Index:
		return "index"
	}
	return fmt.Sprintf("%T", n)
}

func TestParse_CallErrors(t *testing.T) {
	tests := []struct {
		src      string
		wantSubs string
	}{
		{"len(", "unexpected end of expression"},
		{"len(x", "unexpected end of expression"},
		{"len(x,)", "unexpected"},
		// "x." reads its trailing dot inside parseIdentPrimary's own dotted-name
		// loop, not parseAccess: a bare identifier consumes every dot that
		// follows it before parsePostfix ever sees one. That loop's own
		// expect(tokIdent) hits EOF here, which — same convention as every
		// other EOF case in this file — reads as "unexpected end of
		// expression", not "expected name, found end of expression".
		{"x.", "unexpected end of expression"},
		// "x.1" cannot exercise parseAccess's "attribute must be a name" error:
		// the lexer (unchanged by this task) reads a leading-dot float across
		// any "." immediately followed by a digit, so "x.1" tokenizes as
		// ident("x"), float(".1") — no tokDot at all, confirmed against
		// Python's own tokenizer, which splits it the same way. "(x).+" tests
		// the same error through parseAccess instead, since a parenthesized
		// receiver's dot cannot be absorbed into any identifier loop and "+"
		// cannot be absorbed into a number.
		{"(x).+", "name"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			_, err := Parse(tc.src)
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want one mentioning %q", tc.src, tc.wantSubs)
			}
			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Fatalf("Parse(%q) error = %q, want it to mention %q", tc.src, err.Error(), tc.wantSubs)
			}
		})
	}
}
