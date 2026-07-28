// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"strings"
	"testing"
)

func evalSrc(t *testing.T, src string, syms Symbols) (Value, error) {
	t.Helper()
	e, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	return e.Eval(syms, TAny)
}

func TestEval_Literals(t *testing.T) {
	tests := []struct {
		src  string
		want Value
	}{
		{"42", Int(42)},
		{"0xFF_FF", Int(65535)},
		{"3.5", Float(3.5)},
		{"1e3", Float(1000)},
		{`'hi'`, String("hi")},
		{`r'a\nb'`, String(`a\nb`)},
		{"True", Bool(true)},
		{"false", Bool(false)},
		{"None", Null()},
		{"null", Null()},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			got, err := evalSrc(t, tt.src, nil)
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("= %v (%s); want %v (%s)", got, got.Kind, tt.want, tt.want.Kind)
			}
		})
	}
}

func TestEval_Names(t *testing.T) {
	syms := MapSymbols{
		"Param.X":          Int(10),
		"Task.Param.Frame": Int(7),
		"Param.if":         String("keyword attribute"),
	}
	tests := []struct {
		src  string
		want Value
	}{
		{"Param.X", Int(10)},
		{"Task.Param.Frame", Int(7)},
		{"Param.if", String("keyword attribute")},
		{"Param.X + 3", Int(13)},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			got, err := evalSrc(t, tt.src, syms)
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("= %v; want %v", got, tt.want)
			}
		})
	}
}

func TestEval_UnknownSymbol(t *testing.T) {
	_, err := evalSrc(t, "1 + Param.DoesNotExist", MapSymbols{})
	if err == nil {
		t.Fatal("Eval = nil error; want unknown symbol")
	}
	if !strings.Contains(err.Error(), `unknown symbol "Param.DoesNotExist"`) {
		t.Errorf("error = %q; want it to name the symbol", err.Error())
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is %T; want *Error", err)
	}
	if e.Offset != 4 {
		t.Errorf("Offset = %d; want 4 (the start of the name)", e.Offset)
	}
}

func TestEval_NilSymbolsIsAnEmptyTable(t *testing.T) {
	// A nil Symbols must behave as an empty table, not panic: callers that
	// evaluate a constant expression should not have to build one.
	if _, err := evalSrc(t, "Param.X", nil); err == nil {
		t.Error("Eval with nil symbols = nil error; want unknown symbol")
	}
	if v, err := evalSrc(t, "1 + 1", nil); err != nil || !v.Equal(Int(2)) {
		t.Errorf("Eval(1 + 1, nil) = %v, %v; want 2, nil", v, err)
	}
}

func TestEval_ErrorBlamesTheOperator(t *testing.T) {
	// The offset must point at the operator that failed, not at the start of
	// the expression. This is the whole reason offsets ride on tree nodes.
	_, err := evalSrc(t, "Param.A + Param.B", MapSymbols{
		"Param.A": String("x"),
		"Param.B": Int(1),
	})
	if err == nil {
		t.Fatal("Eval = nil error; want unsupported operand types")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is %T; want *Error", err)
	}
	if e.Offset != 8 {
		t.Errorf("Offset = %d; want 8 (the \"+\")", e.Offset)
	}
	if !strings.Contains(e.Error(), "unsupported operand types for +: string and int") {
		t.Errorf("error = %q; want it to name the operator and both kinds", e.Error())
	}
}

func TestEval_ErrorUnwrapsToTheCause(t *testing.T) {
	_, err := evalSrc(t, "1 / 0", nil)
	if err == nil {
		t.Fatal("Eval = nil error; want division by zero")
	}
	if !errors.Is(err, errDivideByZero) {
		t.Errorf("errors.Is(err, errDivideByZero) = false; err = %v", err)
	}
}

func TestEval_UnaryErrorBlamesTheOperator(t *testing.T) {
	// The offset must point at the unary operator itself, not the start of the
	// expression or the operand it failed to apply to.
	_, err := evalSrc(t, "1 + -'x'", nil)
	if err == nil {
		t.Fatal("Eval = nil error; want unsupported operand type")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is %T; want *Error", err)
	}
	if e.Offset != 4 {
		t.Errorf("Offset = %d; want 4 (the unary \"-\")", e.Offset)
	}
	if !strings.Contains(e.Error(), "unsupported operand type for -: string") {
		t.Errorf("error = %q; want it to name the unary operator and the kind", e.Error())
	}
}

func TestEval_NestedSubexpressionErrorBlamesTheInnerOperator(t *testing.T) {
	// "1 + (2 / 0)": the division fails inside the parens. The reported
	// offset must blame the inner "/" and not the outer "+" that never got
	// applied — this pins the innermost-failing-operator-wins, no-double-wrap
	// property.
	_, err := evalSrc(t, "1 + (2 / 0)", nil)
	if err == nil {
		t.Fatal("Eval = nil error; want division by zero")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is %T; want *Error", err)
	}
	if e.Offset != 7 {
		t.Errorf("Offset = %d; want 7 (the inner \"/\")", e.Offset)
	}
	if !errors.Is(err, errDivideByZero) {
		t.Errorf("errors.Is(err, errDivideByZero) = false; err = %v", err)
	}
}

func TestEval_Arithmetic(t *testing.T) {
	tests := []struct {
		src  string
		want Value
	}{
		{"1 + 2 * 3", Int(7)},
		{"(1 + 2) * 3", Int(9)},
		{"10 / 4", Float(2.5)},
		{"-7 // 3", Int(-3)},
		{"-7 % 3", Int(2)},
		{"2 ** 10", Int(1024)},
		{"2 ** -2", Float(0.25)},
		{"-2 ** 2", Int(-4)},
		{"--5", Int(5)},
		{`'a' + 'b'`, String("ab")},
		{"not true", Bool(false)},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			got, err := evalSrc(t, tt.src, nil)
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("= %v (%s); want %v (%s)", got, got.Kind, tt.want, tt.want.Kind)
			}
		})
	}
}

func TestEval_ChainedComparison(t *testing.T) {
	tests := []struct {
		src  string
		want Value
	}{
		{"1 < 2", Bool(true)},
		{"2 < 1", Bool(false)},
		{"1 < 2 < 3", Bool(true)},
		{"1 < 3 < 2", Bool(false)},
		{"3 < 2 < 1", Bool(false)},
		{"1 <= 1 <= 1", Bool(true)},
		{"5 == 5.0", Bool(true)},
		{`'5' == 5`, Bool(false)},
		{"true == 1", Bool(false)},
		{"None == None", Bool(true)},
		{"1 != 2", Bool(true)},
		{`'ell' in 'hello'`, Bool(true)},
		{`'zzz' not in 'hello'`, Bool(true)},
		{`'abc' < 'abd'`, Bool(true)},
		{"false < true", Bool(true)},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			got, err := evalSrc(t, tt.src, nil)
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("= %v; want %v", got, tt.want)
			}
		})
	}
}

func TestEval_ChainShortCircuitsBeforeABadOperand(t *testing.T) {
	// "1 > 2" is false, so the chain stops and Param.Missing is never looked
	// up. If the chain evaluated eagerly this would fail with unknown symbol.
	got, err := evalSrc(t, "1 > 2 > Param.Missing", MapSymbols{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if !got.Equal(Bool(false)) {
		t.Errorf("= %v; want false", got)
	}
}

func TestEval_ChainedComparisonBlamesTheFailingLink(t *testing.T) {
	// A chain's error must blame whichever link actually failed, not always
	// the chain's first operator: "1 < 2 < 'x'" succeeds on "1 < 2" and fails
	// on the SECOND "<" (int vs string has no dispatch-table row).
	tests := []struct {
		name       string
		src        string
		wantOffset int
	}{
		{"second link of a two-link chain", "1 < 2 < 'x'", 6},
		{"third link of a four-link chain", "1 < 2 < 3 < 'x'", 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := evalSrc(t, tt.src, nil)
			if err == nil {
				t.Fatal("Eval = nil error; want unsupported operand types")
			}
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("error is %T; want *Error", err)
			}
			if e.Offset != tt.wantOffset {
				t.Errorf("Offset = %d; want %d (the failing link's own operator)", e.Offset, tt.wantOffset)
			}
		})
	}
}

func TestEval_ChainedComparisonEvaluatesMiddleOperandExactlyOnce(t *testing.T) {
	// Pins section 1.3.6 against a future refactor: the shared middle operand
	// of "Param.A < Param.B < Param.C" must be evaluated exactly once, not
	// once per link that references it. A countingSymbols records how many
	// times each name is looked up so the claim is checked directly, rather
	// than inferred from the result value (which cannot distinguish one
	// lookup from two for a side-effect-free expression).
	syms := countingSymbols{
		values: MapSymbols{"Param.A": Int(1), "Param.B": Int(2), "Param.C": Int(3)},
		calls:  map[string]int{},
	}
	got, err := evalSrc(t, "Param.A < Param.B < Param.C", syms)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if !got.Equal(Bool(true)) {
		t.Errorf("= %v; want true", got)
	}
	for _, name := range []string{"Param.A", "Param.B", "Param.C"} {
		if syms.calls[name] != 1 {
			t.Errorf("Lookup(%q) called %d times; want exactly 1", name, syms.calls[name])
		}
	}
}

// countingSymbols wraps a MapSymbols and records how many times each name is
// looked up, so a test can assert an exact evaluation count rather than only
// a result value.
type countingSymbols struct {
	values MapSymbols
	calls  map[string]int
}

// Lookup implements Symbols.
func (c countingSymbols) Lookup(name string) (Value, bool) {
	c.calls[name]++
	return c.values.Lookup(name)
}

func TestEval_LogicalReturnsAnOperand(t *testing.T) {
	// Section 2.1.6: and/or are value-returning, and only null and false are
	// falsy. This is what makes "or" a null-coalescing operator.
	syms := MapSymbols{
		"Param.Null":  Null(),
		"Param.False": Bool(false),
		"Param.Zero":  Int(0),
		"Param.Empty": String(""),
		"Param.Name":  String("shot01"),
	}
	tests := []struct {
		src  string
		want Value
	}{
		{"Param.Null or 'fallback'", String("fallback")},
		{"Param.False or 'fallback'", String("fallback")},
		{"Param.Name or 'fallback'", String("shot01")},
		{"Param.Zero or 'fallback'", Int(0)},
		{"Param.Empty or 'fallback'", String("")},
		{"Param.Null and 'never'", Null()},
		{"Param.False and 'never'", Bool(false)},
		{"Param.Name and 'reached'", String("reached")},
		{"Param.Zero and 'reached'", String("reached")},
		{"true and false", Bool(false)},
		{"false or true", Bool(true)},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			got, err := evalSrc(t, tt.src, syms)
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("= %v (%s); want %v (%s)", got, got.Kind, tt.want, tt.want.Kind)
			}
		})
	}
}

func TestEval_LogicalShortCircuits(t *testing.T) {
	// The right operand must not be evaluated when the left settles the
	// answer, or the unknown symbol would surface.
	syms := MapSymbols{"Param.False": Bool(false), "Param.True": Bool(true)}
	for _, src := range []string{
		"Param.False and Param.Missing",
		"Param.True or Param.Missing",
	} {
		t.Run(src, func(t *testing.T) {
			if _, err := evalSrc(t, src, syms); err != nil {
				t.Errorf("Eval(%q) = %v; want the right operand to be skipped", src, err)
			}
		})
	}
}

func TestTruthy_PlaceholderIsNotFalsy(t *testing.T) {
	// truthy used to read the vestigial Kind field, which Unresolved leaves at
	// its zero value — the same zero value as KindNull — so a placeholder was
	// silently misread as null and treated as falsy. Task 13 still owns the
	// real semantics (a placeholder should union with the other operand's
	// type), but this narrow property — a placeholder is not mistaken for
	// null — must survive that rework.
	tests := []struct {
		name string
		v    Value
		want bool
	}{
		{"null is falsy", Null(), false},
		{"false is falsy", Bool(false), false},
		{"a placeholder is not falsy", Unresolved(TBool), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truthy(tt.v); got != tt.want {
				t.Errorf("truthy(%s) = %v; want %v", tt.v, got, tt.want)
			}
		})
	}
}

func TestEval_Conditional(t *testing.T) {
	syms := MapSymbols{"Param.Q": String("final")}
	tests := []struct {
		src  string
		want Value
	}{
		{"1 if true else 2", Int(1)},
		{"1 if false else 2", Int(2)},
		{`'high' if Param.Q == 'final' else 'low'`, String("high")},
		{"'--flag' if true else null", String("--flag")},
		{"'--flag' if false else null", Null()},
		{"1 if false else 2 if true else 3", Int(2)},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			got, err := evalSrc(t, tt.src, syms)
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("= %v; want %v", got, tt.want)
			}
		})
	}
}

func TestEval_ConditionalOnlyEvaluatesTheChosenBranch(t *testing.T) {
	if _, err := evalSrc(t, "1 if true else Param.Missing", MapSymbols{}); err != nil {
		t.Errorf("Eval = %v; want the else branch to be skipped", err)
	}
	if _, err := evalSrc(t, "Param.Missing if false else 2", MapSymbols{}); err != nil {
		t.Errorf("Eval = %v; want the then branch to be skipped", err)
	}
}

func TestEval_ConditionalRequiresABoolCondition(t *testing.T) {
	// Section 1.3.5: there is no truthiness in a conditional, in deliberate
	// contrast to and/or.
	for _, src := range []string{"1 if 1 else 2", "1 if 'x' else 2", "1 if null else 2"} {
		t.Run(src, func(t *testing.T) {
			_, err := evalSrc(t, src, nil)
			if err == nil {
				t.Fatalf("Eval(%q) = nil error; want a bool-condition error", src)
			}
			if !strings.Contains(err.Error(), "must be a bool") {
				t.Errorf("error = %q; want it to say the condition must be a bool", err.Error())
			}
		})
	}
}

func TestEvalFunc(t *testing.T) {
	// The package-level one-step form.
	got, err := Eval("Param.X * 2", MapSymbols{"Param.X": Int(21)}, TAny)
	if err != nil || !got.Equal(Int(42)) {
		t.Errorf("Eval = %v, %v; want 42, nil", got, err)
	}
	if _, err := Eval("1 +", nil, TAny); err == nil {
		t.Error("Eval of a syntax error = nil; want a parse error")
	}
}

func TestEval_CoercesToTheTarget(t *testing.T) {
	tests := []struct {
		name   string
		src    string
		target Type
		want   Value
	}{
		{"no constraint keeps the natural type", "1 + 2", TAny, Int(3)},
		{"int result to a float target", "1 + 2", TFloat, Float(3)},
		{"int result to a string target", "1 + 2", TString, String("3")},
		{"float result to a string target", "1.5 + 1", TString, String("2.5")},
		{"bool result to a string target", "1 < 2", TString, String("true")},
		{"string result to an int target", "'42'", TInt, Int(42)},
		{"an exact float to an int target", "4.0", TInt, Int(4)},
		{"a null against an optional target", "null", OptionalOf(TInt), Null()},
		{"a union target that already admits the result", "1", UnionOf(TInt, TString), Int(1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := Parse(tt.src)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.src, err)
			}
			got, err := e.Eval(nil, tt.target)
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("Eval(%q, %s) = %s (%s); want %s (%s)",
					tt.src, tt.target, got, got.Type, tt.want, tt.want.Type)
			}
		})
	}
}

func TestEval_TargetMismatchIsAnError(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		target  Type
		wantMsg string
	}{
		{"a fractional float to an int target", "3.75", TInt, "cannot be represented"},
		{"a non-numeric string to an int target", "'abc'", TInt, "cannot be represented"},
		{"an int to a bool target", "1", TBool, "cannot be coerced"},
		{"a null to a required target", "null", TInt, "cannot be coerced"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := Parse(tt.src)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.src, err)
			}
			_, err = e.Eval(nil, tt.target)
			if err == nil {
				t.Fatalf("Eval(%q, %s) = nil error; want an error", tt.src, tt.target)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q; want it to contain %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestEval_TargetDoesNotFlowInward(t *testing.T) {
	// The resolution of section 1.3.1's ambiguity. With a string target, pushing
	// the target inward would make both operands text and concatenate them into
	// "11"; the correct reading adds them and converts once, at the end.
	e, err := Parse("1 + 1")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := e.Eval(nil, TString)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if want := String("2"); !got.Equal(want) {
		t.Errorf("Eval(\"1 + 1\", string) = %s; want %s — the target must not reach the operands", got, want)
	}
}

func TestEval_TargetErrorCarriesAPosition(t *testing.T) {
	// A coercion failure at the boundary is still a positioned error, so a
	// template author is told where to look.
	e, err := Parse("3.75")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = e.Eval(nil, TInt)
	if err == nil {
		t.Fatal("Eval = nil error; want an error")
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("error is %T; want *Error", err)
	}
	if pe.Offset != 0 {
		t.Errorf("Offset = %d; want 0", pe.Offset)
	}
}
