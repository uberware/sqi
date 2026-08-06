// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"strings"
)

// resolved is what a dotted name resolved to: a symbol, plus whatever segments
// followed it.
type resolved struct {
	// Val is the symbol's value.
	Val Value
	// Prefix is the dotted symbol name that matched, for error messages.
	Prefix string
	// Rest are the segments after the symbol. Each is a property, except a
	// final one immediately followed by "(", which the caller treats as a
	// method.
	Rest []string
}

// resolveName splits a dotted name into a symbol and the segments that follow
// it, by trying the longest prefix first.
//
// This exists because method and property syntax share the "." that separates a
// dotted symbol name: "Param.Name.upper()" must find the symbol "Param.Name"
// and the method "upper". The package deliberately hardcodes no namespaces —
// Symbols' own doc comment makes the caller authoritative about what is in
// scope — so the split cannot be decided at parse time, and it cannot be
// decided by pattern-matching "Param" or "Task" here either.
//
// Longest-first is what makes a caller that binds both "a.b" and "a.b.c"
// resolve "a.b.c" to the symbol rather than to a property of "a.b". Section
// 1.2.2's symbols are two or three segments and a "let" binding is one, so this
// loop runs at most a handful of times.
func resolveName(n *Name, syms Symbols) (resolved, bool) {
	for i := len(n.Parts); i > 0; i-- {
		prefix := strings.Join(n.Parts[:i], ".")
		if v, ok := syms.Lookup(prefix); ok {
			return resolved{Val: v, Prefix: prefix, Rest: n.Parts[i:]}, true
		}
	}
	return resolved{}, false
}

// evalProperty resolves a property access. Section 1.3.3 defines a property p
// as the function __property_p__, so this routes into the function registry.
// C1 registers no property at all (its four groups are general conversions,
// validation, math and list functions); the path engine's properties are
// sub-project C4's.
//
// callFunction can fail three ways: the function is not registered at all, no
// registered signature accepts the receiver, or the matched signature's own
// Fn (or the coercion callShape applies before running it) errors out. Only
// the first of those is "unknown property" — the other two are genuine
// failures of a property that DOES exist, and reporting them as "unknown"
// would send a user chasing a typo that isn't there. errUnknownFunction is
// the sentinel that tells the three apart; anything else is wrapped with its
// real cause preserved, matching evalCall's own wrapAt treatment of a
// callFunction error.
func evalProperty(recv Value, attr string, ec evalCtx, offset, depth int) (Value, error) { //nolint:revive // depth: not yet consumed here, kept for the signature evalCall and evalDispatch already call through
	if isDunder(attr) {
		return Value{}, errorAt(ec.src, offset,
			"%q is a specification naming convention and is not directly callable", attr)
	}
	// Section 1.3.3: the property p is the function __property_p__.
	v, err := callFunction(ec, "__property_"+attr+"__", []Value{recv}, true)
	if err != nil {
		if errors.Is(err, errUnknownFunction) {
			return Value{}, errorAt(ec.src, offset, "unknown property %q on %s", attr, recv.Type)
		}
		return Value{}, wrapAt(ec.src, offset, err)
	}
	return v, nil
}

// evalProperties applies a chain of property accesses in order, e.g. for
// "Param.File.parent.name" attrs is ["parent", "name"].
//
// This is the one place that walk lives. evalName (a plain dotted name's
// trailing properties) and Call.nameTarget (a method call's receiver walk,
// every resolved segment but the last) are the same loop over a different
// slice of the same resolved.Rest, and were duplicated before this helper
// existed.
func evalProperties(v Value, attrs []string, ec evalCtx, offset, depth int) (Value, error) {
	for _, attr := range attrs {
		var err error
		v, err = evalProperty(v, attr, ec, offset, depth)
		if err != nil {
			return Value{}, err
		}
	}
	return v, nil
}

// isDunder reports whether a name is a __double_underscore__ name. Section
// 1.3.3 states those are specification conventions and "are not directly
// callable", so a user writing one must be rejected rather than silently
// missing the function it names.
func isDunder(s string) bool {
	return len(s) > 4 && strings.HasPrefix(s, "__") && strings.HasSuffix(s, "__")
}
