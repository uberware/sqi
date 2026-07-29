// SPDX-License-Identifier: AGPL-3.0-or-later

// Package expr reads and evaluates the OpenJD EXPR extension's expression
// language.
//
// The package is a self-contained leaf: it imports nothing from
// internal/openjd. The dependency runs the other way — internal/openjd will
// import expr once template integration lands — so expr can be tested with no
// template machinery present.
//
// # Usage
//
//	e, err := expr.Parse("Param.Frame * 2")
//	v, err := e.Eval(expr.MapSymbols{"Param.Frame": expr.Int(21)}, expr.TAny)
//	// v.String() == "42"
//
// Parse reports syntax errors; Eval reports evaluation errors. Both return an
// *Error carrying a byte offset into the source, rendered as a line and
// column, so a template author can see which part of an expression failed.
//
// # This is a SUBSET of EXPR
//
// This package implements the spec's full type system: a recursive Type with
// all sixteen type codes (the Recommended Library Interface's Type Codes
// table — section 1.2.1 itself lists twelve rows and omits any, noreturn and
// the type variables), automatic coercion between them
// (section 1.2.3), and static type checking against placeholders for values
// that do not exist yet (section 1.3.1's unresolved[T]) — "Param.Frame + 1"
// type-checks to unresolved[int] before any parameter has a value, and
// "Param.Name + 5" is rejected as a type error the same way, with Param.Name
// declared but still unbound. Every literal form, dotted names and the full
// ten-level operator grammar are implemented on top of that type system. The
// rest of the extension is not implemented, and several of the remaining
// omissions look like bugs when tried by hand. They are not:
//
//   - "1 + 2.5" now evaluates to 3.5, and "1 < 2.5" to true: section 2.1.1's
//     int-to-float promotion is implemented, and an operator is selected by
//     trying an exact-type match first and a promoting one second. But
//     promotion only ever uses a conversion that cannot fail on any value —
//     int-to-float, path-to-string, range_expr-to-string,
//     range_expr-to-list[int] — never one that can, such as string-to-int.
//     That is why "'a' + 1" is still an ERROR rather than "a1": section
//     1.2.3's single-scalar catch-all conversion applies when a context
//     (a target type, a coercion) demands a specific type and is prepared for
//     the value not to fit, not when the language itself is choosing which
//     operator overload to run on the caller's behalf. "true + true" is an
//     error for the same reason: no shape accepts (bool, bool), and bool has
//     no promoting route into one that does.
//   - Equality and ordering diverge in how far they reach across types.
//     Section 1.2.5 defines equality for every pair of types, so "5 == 5.0"
//     is true and "'5' == 5" is false — equality never fails, it just isn't
//     always true. Section 2.1.4 permits ordering to cross only two named
//     compatible pairs, int/float and string/path; every other cross-type
//     comparison is an ERROR, so "5 < 'a'" fails with "unsupported operand
//     types" even though "5 == 'a'" evaluates fine (to false).
//   - KNOWN DIVERGENCE: a range_expr orders against a string, which section
//     2.1.4 does not name as a compatible pair — it restricts ordering
//     operands to int, float, string, path and bool. promotable (coerce.go),
//     the predicate argCost (shape.go) uses to admit a cross-type argument
//     into a same-type shape, is shared by every operator rather than
//     parameterized per one, so section 1.2.3's own range_expr -> string
//     coercion rule leaks into ordering's (string, string) shape the same
//     way path -> string legitimately does. It cannot produce a false green:
//     the conformance suite has no fixture asserting that ordering a
//     range_expr must be rejected. Left as a documented gap rather than
//     restructuring admissibility per-operator, which is a larger design
//     change than this fix wave should make.
//   - Lists, list literals, comprehensions, subscripts and slices are not
//     implemented. "[1, 2]" and "x[0]" fail to parse. list[T] exists as a
//     Type and participates in coercion — B1 can answer whether a
//     list[T] would coerce to a list[U], element type by element type — but
//     it has no values, no literal syntax and no operators of its own —
//     including no subscript or slice operator; that performing half,
//     indexing and slicing, list-literal type inference (section 1.2.6), and
//     the element-wise conversion itself all belong to sub-project B2.
//   - Function and method calls are not implemented. "len(x)" and "x.upper()"
//     fail, and comprehensions (section 1.3.7) do not parse either — that is
//     sub-project B3's call and method syntax (sections 1.3.3, 1.2.4). The
//     ~100-function library and the type-variable binding a call site needs
//     are sub-project C's; the type-variable codes (CodeVarT and friends) and
//     the matcher's binding of them already live here in shape.go, ready for
//     C's signatures to use.
//   - path and range_expr now exist as types — TPath and TRangeExpr
//     participate in Type, coercion and cross-type equality, and a declared
//     parameter can be typed as either. path has no literal syntax of its
//     own, but a value IS produced by evaluation: string -> path is section
//     1.2.3's own coercion rule, so Eval(src, syms, expr.TPath) returns a
//     path value for any expression that evaluates to a string. range_expr
//     has no such route — nothing coerces to it — so it has no value ever
//     produced by evaluation. Neither has an operator of its own; path's
//     POSIX/Windows semantics, its URI awareness and its operators (section
//     2.1.5) belong to sub-projects D and E. The range_expr -> list[int]
//     conversion is recognized at the type level (coercible says yes when the
//     target admits list[int]), but performing it needs list values, which is
//     sub-project B2's, same as every other list conversion. Cross-type
//     equality is only PARTLY implemented: section 1.2.5's string/path rule
//     (the path converts to string for the comparison) is implemented in
//     valuesEqual, but its list/range_expr rule is not — that needs list
//     values, which is sub-project B2's along with everything else list.
//   - String repetition ("'x' * 3") is absent until the operation limits that
//     bound the repeat count exist.
//   - The memory and operation limits themselves are not implemented, so this
//     package must not be handed untrusted expressions in its present state.
//     Both belong to sub-project E, unchanged from sub-project A.
//   - A float value does not preserve the original source text it was parsed
//     from. Section 1.3.4's requirement that a float pass through a template
//     unchanged, digit for digit, is not implemented here; Value.String is a
//     diagnostic rendering, not that pass-through. This is sub-project E's,
//     unchanged from sub-project A.
//   - Nothing here touches a job template. Parsing a template, binding its
//     parameters, and interpolating an expression's result back into template
//     text are all outside this package; it evaluates expression text handed
//     to it directly.
//   - Section 1.1.5's escape table is narrower than its own opening claim
//     that "all Python escape sequences are supported": \a, \b, \f, \v, \0,
//     octal escapes and a backslash-newline line continuation are absent from
//     the table, so this package keeps them verbatim, backslash included,
//     rather than decoding them — "'\a'" evaluates to the two characters
//     "\a". This is a deliberate reading of the table over the prose, not a
//     deferral to a later sub-project.
//
// Anything unimplemented FAILS rather than silently misbehaving, with one
// deliberate exception: the escape sequences named above pass through
// verbatim instead of erroring or being decoded. The grammar is EXPR's own
// rather than borrowed from a Python parser precisely so that the failure
// runs in that direction: a borrowed parser would accept syntax this package
// cannot evaluate.
//
// # Extending it
//
// Two shapes carry the weight and should not be reshaped:
//
//   - Type is a code plus its type parameters (Params), built through
//     normalizing constructors — UnionOf, ListOf, UnresolvedOf and the rest —
//     rather than as struct literals, so that two types meaning the same
//     thing always have the same shape and Equal is sufficient everywhere
//     downstream. Value carries a Type rather than inferring one from which
//     payload field is set; an unresolved value carries no payload at all,
//     which is what makes a typed-but-valueless result possible.
//   - Operator behavior is an ordered list of Shapes per operator (shape.go,
//     ops.go), not a switch or a map keyed on operand types — a Type
//     containing a slice cannot be a map key at all. Each Shape declares the
//     types it takes AND the type it returns, which is what lets a missing
//     operand value still produce a typed result. New signatures are new
//     Shape entries; a candidate list with no admissible match is still
//     reported as "unsupported operand types", the same single mechanism
//     sub-project A used for its narrower same-type-only dispatch.
//
// The specification is the OpenJD wiki page "Expression Language [Extension:
// EXPR]", pinned in the third_party/openjd-specifications submodule. Section
// numbers cited in this package's comments refer to it.
package expr
