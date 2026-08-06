// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"fmt"
)

// errUnknownFunction is callFunction's sentinel for "no such function is
// registered", as distinct from every other way a call can fail (a signature
// mismatch, a coercion failure, or the function's own Fn erroring out).
//
// evalProperty relies on the distinction: section 1.3.3 property syntax is
// sugar for a call to __property_p__, so a genuine runtime error inside a
// registered property function must not be relabeled "unknown property" —
// that would tell a user their correctly spelled property doesn't exist
// instead of showing them the real failure. errors.Is against this sentinel
// is what evalProperty checks before rewording.
var errUnknownFunction = errors.New("unknown function")

// evalCall evaluates a call or a method call (spec section 1.3.3).
//
// Uniform function call syntax makes "x.f(a)" equivalent to "f(x, a)", so both
// forms end in callFunction with the receiver prepended. The two are NOT
// interchangeable, though: section 1.2.4 suppresses coercion on a method
// receiver, so which form was written has to reach callFunction.
func evalCall(n *Call, ec evalCtx, depth int) (Value, error) {
	name, recv, methodStyle, err := n.target(ec, depth)
	if err != nil {
		return Value{}, err
	}
	args := make([]Value, 0, len(n.Args)+1)
	if methodStyle {
		args = append(args, recv)
	}
	for _, a := range n.Args {
		v, err := evalNode(a, ec, TAny, depth+1)
		if err != nil {
			return Value{}, err
		}
		args = append(args, v)
	}
	out, err := callFunction(ec, name, args, methodStyle)
	if err != nil {
		return Value{}, wrapAt(ec.src, n.Offset, err)
	}
	return out, nil
}

// target works out what a call is calling: the function name, the receiver when
// the call was written in method position, and whether it was.
func (n *Call) target(ec evalCtx, depth int) (name string, recv Value, methodStyle bool, err error) {
	switch callee := n.Callee.(type) {
	case *Access:
		// "[1,2].len()" — the receiver is not a name.
		if isDunder(callee.Attr) {
			return "", Value{}, false, errorAt(ec.src, callee.Offset,
				"%q is a specification naming convention and is not directly callable", callee.Attr)
		}
		v, err := evalNode(callee.X, ec, TAny, depth+1)
		if err != nil {
			return "", Value{}, false, err
		}
		return callee.Attr, v, true, nil
	case *Name:
		return n.nameTarget(callee, ec, depth)
	}
	// Every other callee — a literal, a subscript, a conditional — is a value,
	// and no value in this language is callable.
	//
	// The wording names no type at all, deliberately. A "%T" here would render
	// the Go type of the tree node ("a *expr.IntLit cannot be called"), which is
	// an implementation detail of this package leaking into a diagnostic a
	// TEMPLATE AUTHOR reads; every other message in this package names spec
	// types instead ("a int cannot be subscripted", "unknown property %q on
	// %s"). The callee's spec type is not available here either — it has not
	// been evaluated, and evaluating it just to name it in an error would run a
	// sub-expression for its side effects. So the message describes the
	// POSITION, and the error's own offset points at the "(" that made it a
	// call. It matches nameTarget's "%s is not a function" above by design: the
	// two are the same diagnosis for a named and an unnamed callee.
	return "", Value{}, false, errorAt(ec.src, n.Offset, "this expression is not a function")
}

// nameTarget resolves a call whose callee is a dotted name. Four outcomes, one
// per shape the resolver can report.
func (*Call) nameTarget(callee *Name, ec evalCtx, depth int) (name string, recv Value, methodStyle bool, err error) {
	r, ok := resolveName(callee, ec.syms)
	if !ok {
		if len(callee.Parts) == 1 {
			// A bare identifier that is not a symbol is a function name.
			fn := callee.Parts[0]
			if isDunder(fn) {
				return "", Value{}, false, errorAt(ec.src, callee.Offset,
					"%q is a specification naming convention and is not directly callable", fn)
			}
			return fn, Value{}, false, nil
		}
		return "", Value{}, false, errorAt(ec.src, callee.Offset, "unknown symbol %q", callee.String())
	}
	if len(r.Rest) == 0 {
		// The whole name is a symbol, and no value in this language is
		// callable.
		return "", Value{}, false, errorAt(ec.src, callee.Offset,
			"%s is not a function", r.Prefix)
	}
	// Every segment but the last is a property; the last is the method.
	cur, err := evalProperties(r.Val, r.Rest[:len(r.Rest)-1], ec, callee.Offset, depth)
	if err != nil {
		return "", Value{}, false, err
	}
	method := r.Rest[len(r.Rest)-1]
	if isDunder(method) {
		return "", Value{}, false, errorAt(ec.src, callee.Offset,
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
//
// ec is threaded through to callShape so that a shape's optional FnCtx (see
// Shape.FnCtx, shape.go) can read the evaluation's settings — path(string) and
// path(list[string]) are the only two rows that need it.
func callFunction(ec evalCtx, name string, args []Value, methodStyle bool) (Value, error) {
	shapes, ok := functionShapes[name]
	if !ok {
		return Value{}, fmt.Errorf("%w %q", errUnknownFunction, name)
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
	return callShape(ec, s, b, args)
}
