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
//	v, err := e.Eval(expr.MapSymbols{"Param.Frame": expr.Int(21)})
//	// v.String() == "42"
//
// Parse reports syntax errors; Eval reports evaluation errors. Both return an
// *Error carrying a byte offset into the source, rendered as a line and
// column, so a template author can see which part of an expression failed.
//
// # This is a SUBSET of EXPR
//
// This package currently implements the first slice of the extension: every
// literal form, dotted names, the full ten-level operator grammar, and
// evaluation of operators whose operands have the SAME TYPE. The rest of the
// extension is not implemented, and several of the omissions look like bugs
// when tried by hand. They are not:
//
//   - "1 + 2.5" is an ERROR. Implicit int-to-float conversion is not
//     implemented, so mixed-type arithmetic and mixed-type ordering
//     comparisons both fail with "unsupported operand types". Equality is the
//     exception: "5 == 5.0" is true, because the spec defines equality across
//     every pair of types.
//   - Lists, list literals, comprehensions, subscripts and slices are not
//     implemented. "[1, 2]" and "x[0]" fail to parse.
//   - Function and method calls are not implemented. "len(x)" and "x.upper()"
//     fail. The ~100-function library is not present.
//   - The path and range_expr types do not exist here, so neither do their
//     operators.
//   - String repetition ("'x' * 3") is absent until the operation limits that
//     bound the repeat count exist.
//   - The memory and operation limits themselves are not implemented, so this
//     package must not be handed untrusted expressions in its present state.
//   - A float value does not preserve the original source text it was parsed
//     from. Section 1.3.4's requirement that a float pass through a template
//     unchanged, digit for digit, is not implemented here; Value.String is a
//     diagnostic rendering, not that pass-through.
//   - Static type checking against unbound parameters (the spec's
//     unresolved[T]) is not implemented. Value's Kind tag is deliberately
//     separable from its payload so it can be added without reshaping Value.
//   - Nothing here touches a job template. Parsing a template, binding its
//     parameters, and interpolating an expression's result back into template
//     text are all outside this package; it evaluates expression text handed
//     to it directly.
//
// Anything unimplemented FAILS rather than silently misbehaving. The grammar
// is EXPR's own rather than borrowed from a Python parser precisely so that
// the failure runs in that direction: a borrowed parser would accept syntax
// this package cannot evaluate.
//
// # Extending it
//
// Two shapes carry the weight and should not be reshaped:
//
//   - Value holds a Kind tag alongside an optional payload, rather than
//     inferring the type from which payload is set. That separation is what
//     allows a typed-but-valueless result later.
//   - Operator behavior lives in a map keyed on (operator, left kind, right
//     kind), not a switch. New signatures are new rows. A missing key reports
//     "unsupported operand types", which is also exactly the same-type-only
//     restriction — one mechanism, not two that can drift.
//
// The specification is the OpenJD wiki page "Expression Language [Extension:
// EXPR]", pinned in the third_party/openjd-specifications submodule. Section
// numbers cited in this package's comments refer to it.
package expr
