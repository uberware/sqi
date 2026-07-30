// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "fmt"

// functionShapes is the function registry: a name mapped to its accepted
// signatures, in the same Shape form ops.go uses for operators, so type-variable
// binding and cost-ranked overload selection are shared rather than
// reimplemented.
//
// IT IS DELIBERATELY EMPTY. Sub-project B3 builds the call, method and property
// machinery; the ~100-function library of specification section 2.2 is
// sub-project C's, and C adds entries here. A call therefore resolves and then
// fails with "unknown function", which is a real diagnostic rather than a
// "not supported" parse error.
//
// Properties follow section 1.3.3's convention: the property p is the function
// __property_p__, registered here under that name.
var functionShapes = map[string][]Shape{}

// evalCall evaluates a call or a method call (spec section 1.3.3).
//
// Uniform function call syntax makes "x.f(a)" equivalent to "f(x, a)", so both
// forms end in callFunction with the receiver prepended. The two are NOT
// interchangeable, though: section 1.2.4 suppresses coercion on a method
// receiver, so which form was written has to reach callFunction.
func evalCall(n *Call, src string, syms Symbols, depth int) (Value, error) {
	name, recv, methodStyle, err := n.target(src, syms, depth)
	if err != nil {
		return Value{}, err
	}
	args := make([]Value, 0, len(n.Args)+1)
	if methodStyle {
		args = append(args, recv)
	}
	for _, a := range n.Args {
		v, err := evalNode(a, src, syms, TAny, depth+1)
		if err != nil {
			return Value{}, err
		}
		args = append(args, v)
	}
	out, err := callFunction(name, args, methodStyle)
	if err != nil {
		return Value{}, wrapAt(src, n.Offset, err)
	}
	return out, nil
}

// target works out what a call is calling: the function name, the receiver when
// the call was written in method position, and whether it was.
func (n *Call) target(src string, syms Symbols, depth int) (name string, recv Value, methodStyle bool, err error) {
	switch callee := n.Callee.(type) {
	case *Access:
		// "[1,2].len()" — the receiver is not a name.
		if isDunder(callee.Attr) {
			return "", Value{}, false, errorAt(src, callee.Offset,
				"%q is a specification naming convention and is not directly callable", callee.Attr)
		}
		v, err := evalNode(callee.X, src, syms, TAny, depth+1)
		if err != nil {
			return "", Value{}, false, err
		}
		return callee.Attr, v, true, nil
	case *Name:
		return n.nameTarget(callee, src, syms, depth)
	}
	return "", Value{}, false, errorAt(src, n.Offset, "a %T cannot be called", n.Callee)
}

// nameTarget resolves a call whose callee is a dotted name. Four outcomes, one
// per shape the resolver can report.
func (*Call) nameTarget(callee *Name, src string, syms Symbols, depth int) (name string, recv Value, methodStyle bool, err error) {
	r, ok := resolveName(callee, syms)
	if !ok {
		if len(callee.Parts) == 1 {
			// A bare identifier that is not a symbol is a function name.
			fn := callee.Parts[0]
			if isDunder(fn) {
				return "", Value{}, false, errorAt(src, callee.Offset,
					"%q is a specification naming convention and is not directly callable", fn)
			}
			return fn, Value{}, false, nil
		}
		return "", Value{}, false, errorAt(src, callee.Offset, "unknown symbol %q", callee.String())
	}
	if len(r.Rest) == 0 {
		// The whole name is a symbol, and no value in this language is
		// callable.
		return "", Value{}, false, errorAt(src, callee.Offset,
			"%s is not a function", r.Prefix)
	}
	// Every segment but the last is a property; the last is the method.
	cur := r.Val
	for _, attr := range r.Rest[:len(r.Rest)-1] {
		v, err := evalProperty(cur, attr, src, callee.Offset, depth)
		if err != nil {
			return "", Value{}, false, err
		}
		cur = v
	}
	method := r.Rest[len(r.Rest)-1]
	if isDunder(method) {
		return "", Value{}, false, errorAt(src, callee.Offset,
			"%q is a specification naming convention and is not directly callable", method)
	}
	return method, cur, true, nil
}

// callFunction selects a signature and runs it.
//
// methodStyle carries section 1.2.4's restriction: when a function is called as
// a method, implicit coercion does not apply to the receiver, so
// "path('/x').startswith('/')" fails where "startswith(path('/x'), '/')"
// succeeds. That is a property of the CALL SITE, not of the signature, which is
// why it cannot be expressed with Shape.Promote.
func callFunction(name string, args []Value, methodStyle bool) (Value, error) {
	shapes, ok := functionShapes[name]
	if !ok {
		return Value{}, fmt.Errorf("unknown function %q", name)
	}
	types := make([]Type, len(args))
	for i, a := range args {
		types[i] = a.Type
	}
	s, b, ok := matchShapesExactFirst(shapes, types, methodStyle)
	if !ok {
		return Value{}, fmt.Errorf("no signature of %q accepts (%s)", name, joinTypes(types))
	}
	for _, a := range args {
		if a.IsUnresolved() {
			return unresolvedResult(s, b), nil
		}
	}
	return callShape(s, b, args)
}

// matchShapesExactFirst is matchShapes with section 1.2.4's receiver
// restriction. Sub-project B3 Task 7 implements exactFirst; until then it
// delegates.
func matchShapesExactFirst(shapes []Shape, args []Type, exactFirst bool) (Shape, bindings, bool) { //nolint:revive // exactFirst is implemented in the next task
	return matchShapes(shapes, args)
}
