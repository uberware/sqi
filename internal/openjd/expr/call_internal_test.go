// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"strings"
	"testing"
)

// TestFunctionShapes_IsEmpty pins sub-project B3's scope: the registry exists
// and resolves, but every function belongs to sub-project C. If this fails
// because a function was added, that is scope creep, not progress.
func TestFunctionShapes_IsEmpty(t *testing.T) {
	if len(functionShapes) != 0 {
		t.Fatalf("functionShapes has %d entries, want 0 — functions are sub-project C's", len(functionShapes))
	}
}

func TestEvalCall_Errors(t *testing.T) {
	syms := MapSymbols{
		"Param.Name": String("shot01"),
		"Param.List": List(TInt, []Value{Int(1)}),
	}
	tests := []struct {
		name     string
		src      string
		wantSubs string
	}{
		{"unknown plain function", "len(Param.List)", `unknown function "len"`},
		{"unknown method", "Param.Name.upper()", `unknown function "upper"`},
		{"unknown method on a literal", "[1, 2].len()", `unknown function "len"`},
		{"unknown property", "Param.Name.stem", `unknown property "stem"`},
		{"symbol is not callable", "Param.Name()", "is not a function"},
		{"unknown symbol with segments", "Param.Nope.upper()", "unknown symbol"},
		{"dunder call", "__add__(1, 2)", "not directly callable"},
		{"dunder method", "Param.Name.__property_stem__", "not directly callable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Eval(tc.src, syms, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) = nil error, want one mentioning %q", tc.src, tc.wantSubs)
			}
			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Fatalf("Eval(%q) error = %q, want it to mention %q", tc.src, err.Error(), tc.wantSubs)
			}
		})
	}
}

// TestEvalCall_NonFunctionCallee covers Call.target's fall-through arm: a
// callee that is neither a Name nor an Access, so there is no function name to
// look up at all.
//
// It asserts the WHOLE message, not a substring, and that is the point of the
// test rather than an accident of style. The message used to be built with
// "%T", which rendered the Go type of the tree node — "a *expr.IntLit cannot be
// called" — leaking this package's internals into a diagnostic a template
// author reads, while every other message here names spec types. A substring
// assertion cannot catch that coming back, because a "%T" satisfies any
// substring the fixed wording does; only the full string does.
func TestEvalCall_NonFunctionCallee(t *testing.T) {
	syms := MapSymbols{"Param.Flag": Bool(true)}
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"an int literal", "5(1)", "col 2: this expression is not a function"},
		{"a subscript", "[1,2][0](3)", "col 9: this expression is not a function"},
		{"a conditional", "(1 if Param.Flag else 2)(3)", "col 25: this expression is not a function"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Eval(tc.src, syms, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) = nil error, want %q", tc.src, tc.want)
			}
			if got := err.Error(); got != tc.want {
				t.Fatalf("Eval(%q) error = %q, want exactly %q", tc.src, got, tc.want)
			}
		})
	}
}

// withTestFunction registers a function for the duration of a test, so the call
// path can be exercised end to end while the shipped registry stays empty.
//
// It mutates the package-level functionShapes map directly, which is safe
// only because nothing in this package calls t.Parallel(); the moment a test
// here does, two tests racing on functionShapes becomes a real data race.
func withTestFunction(t *testing.T, name string, shapes []Shape) {
	t.Helper()
	if _, exists := functionShapes[name]; exists {
		t.Fatalf("%q is already registered", name)
	}
	functionShapes[name] = shapes
	t.Cleanup(func() { delete(functionShapes, name) })
}

func TestEvalCall_DispatchesThroughTheRegistry(t *testing.T) {
	withTestFunction(t, "twice", []Shape{{
		Params: []Type{TInt},
		Ret:    TInt,
		Fn:     func(args []Value) (Value, error) { return Int(args[0].AsInt() * 2), nil },
	}})
	syms := MapSymbols{"Param.N": Int(21), "Param.U": Unresolved(TInt)}
	tests := []struct {
		src      string
		want     string
		wantType string
	}{
		{"twice(21)", "42", "int"},
		{"twice(Param.N)", "42", "int"},
		{"Param.N.twice()", "42", "int"},
		{"twice(Param.U)", "<unresolved[int]>", "unresolved[int]"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("type = %s, want %s", got, tc.wantType)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("value = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestEvalProperty_RealFailureIsNotRelabeledUnknown is the discriminating test
// for the errUnknownFunction sentinel: a property that IS registered but
// fails for a real reason — its Fn errors, or no signature accepts the
// receiver's type — must surface that real cause, not the "unknown property"
// wording that is reserved for a property that was never registered at all.
// Before the sentinel this collapsed all three into "unknown property",
// which is unobservable while the registry is empty and would have shipped
// silently the moment sub-project C added a real property.
func TestEvalProperty_RealFailureIsNotRelabeledUnknown(t *testing.T) {
	tests := []struct {
		name      string
		shapeName string
		shapes    []Shape
		src       string
		syms      MapSymbols
		wantSubs  string
	}{
		{
			name:      "the property function's own error is preserved",
			shapeName: "__property_boom__",
			shapes: []Shape{{
				Params: []Type{TInt},
				Ret:    TInt,
				Fn: func([]Value) (Value, error) {
					return Value{}, errors.New("boom: something failed")
				},
			}},
			src:      "Param.N.boom",
			syms:     MapSymbols{"Param.N": Int(21)},
			wantSubs: "boom: something failed",
		},
		{
			name:      "no signature accepting the receiver is preserved",
			shapeName: "__property_onlystring__",
			shapes: []Shape{{
				Params: []Type{TString},
				Ret:    TString,
				Fn:     func(args []Value) (Value, error) { return args[0], nil },
			}},
			src:      "Param.N.onlystring",
			syms:     MapSymbols{"Param.N": Int(21)},
			wantSubs: `no signature of "__property_onlystring__" accepts`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withTestFunction(t, tc.shapeName, tc.shapes)
			_, err := Eval(tc.src, tc.syms, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) = nil error, want one mentioning %q", tc.src, tc.wantSubs)
			}
			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Fatalf("Eval(%q) error = %q, want it to mention %q", tc.src, err.Error(), tc.wantSubs)
			}
			if strings.Contains(err.Error(), "unknown property") {
				t.Fatalf("Eval(%q) error = %q, want it NOT relabeled \"unknown property\"", tc.src, err.Error())
			}
		})
	}
}

// TestEvalCall_PropertyChainThenMethod covers the one outcome of
// Call.nameTarget's four that TestEvalCall_DispatchesThroughTheRegistry never
// exercises: a receiver walk through one or more LEADING properties before
// the trailing method, "Param.N.doubled.twice()" — Rest is ["doubled",
// "twice"], so the "every segment but the last is a property" loop actually
// runs.
func TestEvalCall_PropertyChainThenMethod(t *testing.T) {
	withTestFunction(t, "__property_doubled__", []Shape{{
		Params: []Type{TInt},
		Ret:    TInt,
		Fn:     func(args []Value) (Value, error) { return Int(args[0].AsInt() * 2), nil },
	}})
	withTestFunction(t, "twice", []Shape{{
		Params: []Type{TInt},
		Ret:    TInt,
		Fn:     func(args []Value) (Value, error) { return Int(args[0].AsInt() * 2), nil },
	}})
	v, err := Eval("Param.N.doubled.twice()", MapSymbols{"Param.N": Int(21)}, TAny)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if got, want := v.String(), "84"; got != want {
		t.Fatalf("Eval = %s, want %s", got, want)
	}
}

// TestEvalCall_NoSignatureAccepts covers callFunction's other failure path —
// the function IS registered, but no shape accepts the argument types given —
// which TestEvalCall_Errors never exercises (every case there hits the
// "unknown function" branch instead).
func TestEvalCall_NoSignatureAccepts(t *testing.T) {
	withTestFunction(t, "twice", []Shape{{
		Params: []Type{TInt},
		Ret:    TInt,
		Fn:     func(args []Value) (Value, error) { return Int(args[0].AsInt() * 2), nil },
	}})
	_, err := Eval("twice('a')", nil, TAny)
	if err == nil {
		t.Fatal("Eval(\"twice('a')\") = nil error, want one mentioning no signature")
	}
	wantSubs := `no signature of "twice" accepts`
	if !strings.Contains(err.Error(), wantSubs) {
		t.Fatalf("Eval error = %q, want it to mention %q", err.Error(), wantSubs)
	}
}

func TestEvalProperty_DispatchesThroughTheRegistry(t *testing.T) {
	withTestFunction(t, "__property_doubled__", []Shape{{
		Params: []Type{TInt},
		Ret:    TInt,
		Fn:     func(args []Value) (Value, error) { return Int(args[0].AsInt() * 2), nil },
	}})
	v, err := Eval("Param.N.doubled", MapSymbols{"Param.N": Int(21)}, TAny)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if got, want := v.String(), "42"; got != want {
		t.Fatalf("Eval = %s, want %s", got, want)
	}
}

// TestReceiverCoercionRestriction pins specification section 1.2.4: when a
// function is called as a method, implicit coercion does not apply to the
// receiver. The spec's own example is startswith(path, string), so this
// reproduces it with a path receiver against a (string, string) signature.
//
// This cannot be observed with the shipped registry, which is empty by design,
// so the function is registered by the test. Sub-project C must not be the
// first place this behavior is exercised.
func TestReceiverCoercionRestriction(t *testing.T) {
	withTestFunction(t, "startswith", []Shape{{
		Params: []Type{TString, TString},
		Ret:    TBool,
		Fn: func(args []Value) (Value, error) {
			return Bool(strings.HasPrefix(args[0].AsStr(), args[1].AsStr())), nil
		},
	}})
	syms := MapSymbols{"Param.Dir": Value{Type: TPath, s: "/foo/bar"}}

	// Function position: the path coerces to string, per section 1.2.4's
	// "coercion applies to all arguments".
	v, err := Eval("startswith(Param.Dir, '/foo')", syms, TAny)
	if err != nil {
		t.Fatalf("function-position call: %v", err)
	}
	if got := v.String(); got != "true" {
		t.Errorf("function-position call = %s, want true", got)
	}

	// Method position: the receiver does NOT coerce, so no signature matches.
	_, err = Eval("Param.Dir.startswith('/foo')", syms, TAny)
	if err == nil {
		t.Fatal("method-position call on a path = nil error, want no matching signature")
	}
	if !strings.Contains(err.Error(), "no signature") {
		t.Errorf("method-position error = %q, want it to mention no signature", err.Error())
	}

	// A receiver that already has the parameter's type works in method
	// position — the restriction removes coercion, not method calls.
	strSyms := MapSymbols{"Param.S": String("/foo/bar")}
	v, err = Eval("Param.S.startswith('/foo')", strSyms, TAny)
	if err != nil {
		t.Fatalf("method-position call on a string: %v", err)
	}
	if got := v.String(); got != "true" {
		t.Errorf("method-position call on a string = %s, want true", got)
	}

	// Non-receiver arguments still coerce in method position.
	pathArg := MapSymbols{"Param.S": String("/foo/bar"), "Param.P": Value{Type: TPath, s: "/foo"}}
	v, err = Eval("Param.S.startswith(Param.P)", pathArg, TAny)
	if err != nil {
		t.Fatalf("method call with a path argument: %v", err)
	}
	if got := v.String(); got != "true" {
		t.Errorf("method call with a path argument = %s, want true", got)
	}
}

// TestReceiverRestriction_TypeVariableBinds pins that a type-variable parameter
// still accepts a receiver: binding is an exact match, so a generic signature
// is usable in method position.
func TestReceiverRestriction_TypeVariableBinds(t *testing.T) {
	withTestFunction(t, "identity", []Shape{{
		Params: []Type{varT},
		Ret:    varT,
		Fn:     func(args []Value) (Value, error) { return args[0], nil },
	}})
	v, err := Eval("Param.Dir.identity()", MapSymbols{"Param.Dir": Value{Type: TPath, s: "/x"}}, TAny)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if got, want := v.Type.String(), "path"; got != want {
		t.Fatalf("type = %s, want %s", got, want)
	}
}
