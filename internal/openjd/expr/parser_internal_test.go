// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// render prints a tree as a fully parenthesised prefix form, so a test can
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
		{"list literal", "[1, 2]", "list expressions are not supported"},
		{"subscript", "Param.X[0]", "subscript and slice expressions are not supported"},
		{"function call", "min(1, 2)", "function and method calls are not supported"},
		{"method call", "'a'.upper()", "property access is not supported"},
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
	_, err := Parse("Param.X + [1]")
	if err == nil {
		t.Fatal("Parse = nil error; want an error")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is %T; want *Error", err)
	}
	if e.Offset != 10 {
		t.Errorf("Offset = %d; want 10 (the \"[\")", e.Offset)
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
