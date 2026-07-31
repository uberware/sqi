// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"strings"
	"testing"
)

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
		{"unknown plain function", "nosuchfn(Param.List)", `unknown function "nosuchfn"`},
		{"unknown method", "Param.Name.upper()", `unknown function "upper"`},
		{"unknown method on a literal", "[1, 2].nosuchfn()", `unknown function "nosuchfn"`},
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
// path can be exercised end to end against a signature the shipped registry
// does not supply — sub-project C1 registered 22 real functions, but a test
// here still needs its own name and shape whenever the behavior under test
// (an overload set with a specific receiver-restriction interaction, an
// intentionally minimal signature) isn't one of them.
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
// Before the sentinel this collapsed all three into "unknown property". That
// was unobservable while the registry held no properties at all — true for
// C1, which registers no property, and would have shipped silently the
// moment a later C wave (C4, the path engine) added a real one, had this not
// been pinned first.
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
// C1 shipped no (path, string) overload set to pin the spec's own worked
// example against — round(), C1's own function with a receiver-restriction
// interaction, only demonstrates the restriction for a single-signature
// int/float promotion (see doc.go), not the startswith(path, string) case
// the spec names — so the function is still registered by the test rather
// than resolved through functionShapes. This is no longer the ONLY place the
// restriction is exercised end to end: doc.go documents round(3) succeeding
// in function position while Param.N.round() fails in method position
// against the real shipped registry. C2's real string functions will let
// this test target a shipped signature instead of one it registers itself.
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

// TestReceiverRestriction_DisqualifiesAShapeNotTheCall pins the branch that
// separates "this SHAPE is out" from "this CALL is out": with an OVERLOAD SET,
// a receiver the restriction disqualifies from one signature must still select
// another that accepts it exactly.
//
// TestReceiverCoercionRestriction covers only the single-signature case, where
// the two are indistinguishable — the one shape being ruled out is the same
// event as the call failing. Sub-project C ships overload sets throughout
// section 2.2's library, so that distinction stops being academic there; this
// is the test that must exist before it does.
func TestReceiverRestriction_DisqualifiesAShapeNotTheCall(t *testing.T) {
	withTestFunction(t, "startswith", []Shape{
		{
			Params: []Type{TString, TString},
			Ret:    TString,
			Fn:     func([]Value) (Value, error) { return String("string shape"), nil },
		},
		{
			Params: []Type{TPath, TString},
			Ret:    TString,
			Fn:     func([]Value) (Value, error) { return String("path shape"), nil },
		},
	})
	syms := MapSymbols{
		"Param.S":   String("/foo/bar"),
		"Param.Dir": Value{Type: TPath, s: "/foo/bar"},
	}
	tests := []struct {
		name string
		src  string
		want string
	}{
		// The path receiver cannot reach (string, string) — that is what
		// TestReceiverCoercionRestriction pins with the string shape alone —
		// but (path, string) takes it exactly, so the call resolves to that
		// one rather than failing.
		{"a path receiver selects the shape that takes it exactly", "Param.Dir.startswith('/foo')", "path shape"},
		// The string receiver still picks the string shape, so the added
		// overload has not simply swallowed everything.
		{"a string receiver still selects the string shape", "Param.S.startswith('/foo')", "string shape"},
		// In FUNCTION position both are admissible for a path argument, and
		// the exact match still wins on cost — the restriction changes which
		// shapes are candidates, not how the winner is ranked.
		{"function position still prefers the exact shape", "startswith(Param.Dir, '/foo')", "path shape"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Fatalf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
			}
		})
	}
}

// TestPropertyReceiverCoercionRestriction pins the ruling that section 1.2.4's
// receiver restriction reaches a PROPERTY receiver, not just an explicit method
// call: section 1.3.3 makes "x.p" method syntax over "__property_p__", so
// evalProperty (resolve.go) dispatches with methodStyle set and the receiver is
// not coerced on the way in.
//
// A path receiver against a "(string)" property signature is the only case
// where that flag is observable, which is why the test is written this way and
// not with, say, an int receiver: the restriction removes exactly the
// conversions overload selection would otherwise have made on the caller's
// behalf, and path -> string is one of the four the spec names as compatible
// (shape.go's promotable). Any receiver type that is NOT promotable to string
// fails identically whether the restriction is applied or not, so a test built
// on one would pass against the unrestricted code too — which is exactly what
// TestEvalProperty_RealFailureIsNotRelabeledUnknown's "__property_onlystring__"
// case looks like it covers but does not.
func TestPropertyReceiverCoercionRestriction(t *testing.T) {
	// The premise, asserted rather than assumed: without the restriction, a
	// path receiver WOULD reach a string parameter. If this ever stops holding
	// the test below has quietly stopped discriminating anything.
	if !promotable(TPath, TString) {
		t.Fatal("promotable(path, string) = false; this test no longer discriminates the receiver restriction")
	}
	withTestFunction(t, "__property_shouty__", []Shape{{
		Params: []Type{TString},
		Ret:    TString,
		Fn:     func(args []Value) (Value, error) { return String(strings.ToUpper(args[0].AsStr())), nil },
	}})
	syms := MapSymbols{
		"Param.S":   String("/foo/bar"),
		"Param.Dir": Value{Type: TPath, s: "/foo/bar"},
	}

	// A string receiver already has the parameter's type, so the property
	// resolves.
	v, err := Eval("Param.S.shouty", syms, TAny)
	if err != nil {
		t.Fatalf("string receiver: %v", err)
	}
	if got, want := v.String(), "/FOO/BAR"; got != want {
		t.Errorf("string receiver = %s, want %s", got, want)
	}

	// A path receiver does not coerce, so no signature accepts it — and the
	// failure must be the real one, not relabeled "unknown property".
	_, err = Eval("Param.Dir.shouty", syms, TAny)
	if err == nil {
		t.Fatal("path receiver = nil error, want no matching signature")
	}
	wantSubs := `no signature of "__property_shouty__" accepts (path)`
	if !strings.Contains(err.Error(), wantSubs) {
		t.Fatalf("path receiver error = %q, want it to mention %q", err.Error(), wantSubs)
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
