// SPDX-License-Identifier: AGPL-3.0-or-later

// Package expr reads and evaluates the OpenJD EXPR extension's expression
// language.
//
// The package is a self-contained leaf with respect to internal/openjd
// itself: the only intra-openjd import is internal/openjd/intrange, a leaf
// shared between the two (see the intrange bullet below), not internal/openjd
// proper. The dependency with internal/openjd proper still runs the other way
// — it will import expr once template integration lands — so expr can be
// tested with no template machinery present.
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
// declared but still unbound. Every literal form — including list literals —
// dotted names and the full ten-level operator grammar are implemented on top
// of that type system. The rest of the extension is not implemented, and
// several of the remaining omissions look like bugs when tried by hand. They
// are not:
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
//
//   - Equality and ordering diverge in how far they reach across types.
//     Section 1.2.5 defines equality for every pair of types, so "5 == 5.0"
//     is true and "'5' == 5" is false — equality never fails, it just isn't
//     always true — and a list or range_expr value follows the same
//     cross-type rule: "[1, 2] == [1.0, 2.0]" is true, and a range_expr
//     compares equal to the list of integers it expands to. Section 2.1.4
//     permits ordering to cross only two named compatible pairs, int/float
//     and string/path; every other cross-type comparison is an ERROR, so
//     "5 < 'a'" fails with "unsupported operand types" even though
//     "5 == 'a'" evaluates fine (to false). List ordering (lexicographic,
//     section 1.2.5) reaches across element types by exactly those same two
//     compatible pairs, applied ELEMENTWISE: "[1] < [1.0]" is false, not an
//     error, and so is "[[1]] < [[1.0]]" one level down, while "['a'] < [1]"
//     stays an "unsupported operand types" error because string/int is not one
//     of the pairs. That composes section 1.2.5 (list ordering is elementwise,
//     and says nothing about the elements' types) with section 2.1.4 (an
//     ordering operator's operands "may differ for compatible pairs"); it was
//     an error here until this wave, and the error was an artifact of the list
//     ordering shape's single shared type variable rather than a reading of the
//     spec. The unification happens during shape matching (shape.go's
//     orderingUnified), so both operands are coerced to the common element type
//     BEFORE any element is compared. Ordering also reaches the empty list, in
//     either operand position and at any nesting depth: section 1.2.6 rule 6 makes
//     list[nulltype] convertible to list[T] for any T, so "[] < [1]" is true,
//     "[1] < []" is false, and "[[]] < [[1]]" and "[[1]] < [[]]" answer the
//     same way one level down. The empty literal's binding of the shared
//     element-type variable is provisional rather than pinning (shape.go's
//     emptyListBinding), and the exception is applied in BOTH directions —
//     an empty binding meeting a real argument, and a real binding meeting an
//     empty argument — which is what makes the orders agree. Only the first of
//     those two directions existed at first, which is exactly why the nested
//     case worked one way and errored the other.
//
//   - List literals, subscripts and slices are implemented, and list[T] has
//     real values — "[1, 2, 3]" parses and evaluates. A literal with no
//     target type infers its element type from its own elements per section
//     1.2.6's unification rules; a list[T] target instead coerces every
//     element to T directly. "x[0]" and "x[1:3]" work on a list, a string or
//     a range_expr (sections 2.1.7 and 2.1.8) — subscript is bounds-checked
//     and errors out of range, while slice clamps like Python. Both also work
//     on a UNION of those, which matters because this package manufactures
//     such a union itself: slicing a range_expr whose length is not yet known
//     is typed "range_expr | list[int]", and section 1.3.1's
//     unknown-condition rule types a conditional as the union of both
//     branches. Every member is indexed and the results combined, so
//     "Param.Range[:][0]" is an int and "([1, 2] if Param.Flag else
//     [3.0])[0]" a float; the operation is rejected only when some member
//     genuinely cannot be indexed. list[T] also
//     has its own operators: concatenation ("+"), repetition ("*" by an int,
//     section 2.1.3) and membership ("in"/"not in"). What is still missing is
//     list comprehensions (section 1.3.7) — "[x for x in [1]]" fails to
//     parse — along with every function and method call; both are
//     sub-project B3's. Subscript and slice have NO differential coverage
//     against the OpenJD reference implementation (make test-expr-oracle),
//     though: verified directly against it under every target tried (int,
//     list[int], float, string, and even a matching type), the reference
//     mis-infers a subscript's or slice's own static result type whenever
//     ANY target type is supplied at all, and is correct only with none — a
//     bug in the reference, not here (see the root-cause-B entries in
//     test/oracle/baseline.txt, which baseline all 22 such corpus cases as a
//     known reference defect). So subscript and slice are checked by this
//     package's own tests and the conformance fixtures only.
//
//   - Function and method calls are not implemented. "len(x)" and "x.upper()"
//     fail, and comprehensions (section 1.3.7) do not parse either — that is
//     sub-project B3's call and method syntax (sections 1.3.3, 1.2.4). The
//     ~100-function library and the type-variable binding a call site needs
//     are sub-project C's; the type-variable codes (CodeVarT and friends) and
//     the matcher's binding of them already live here in shape.go, ready for
//     C's signatures to use.
//
//   - path and range_expr exist as types — TPath and TRangeExpr participate
//     in Type, coercion and cross-type equality, and a declared parameter can
//     be typed as either. path has no literal syntax of its own, but a value
//     IS produced by evaluation: string -> path is section 1.2.3's own
//     coercion rule, so Eval(src, syms, expr.TPath) returns a path value for
//     any expression that evaluates to a string. Neither path nor range_expr
//     has an operator of its own beyond what coercion gives it for free:
//     path's POSIX/Windows semantics, its URI awareness and its own
//     operators (section 2.1.5) belong to sub-projects D and E. The
//     range_expr -> list[int] conversion (section 1.2.3) is fully
//     implemented — coercing a range_expr value to a list[int] target expands
//     it — and so is section 1.2.5's list/range_expr cross-type equality
//     rule, both exercised above. What coercion gives a range_expr OPERAND for
//     free is deliberately narrow, and the narrowing is an adjudication rather
//     than a gap: section 1.2.3's range_expr -> string rule reaches a string
//     PARAMETER only for the operators whose own tables name a range_expr row,
//     which is "+" alone — sections 2.1.2 and 2.1.3 write those rows out
//     explicitly, and would not need to if the coercion fired on its own during
//     overload selection. So "Param.Range + '!'" still concatenates as text,
//     while "Param.Range * 2" and "'1' in Param.Range" are "unsupported operand
//     types" errors rather than the string repetition "1-101-10" and the
//     substring test over "1-10" they used to be. The reference implementation
//     rejects both as well.
//
//   - range_expr has real values, but nothing in the language's own grammar
//     constructs one: no literal syntax, coercion or operator produces a
//     range_expr from anything else — "nothing coerces to it" still holds.
//     The only way to get one is expr.RangeExpr(text) directly, or a caller's
//     symbol table binding a name to a value built that way; parsing and
//     evaluating an expression never yields a range_expr on its own. That
//     stays true until sub-project C's range_expr() function or sub-project
//     E's CHUNK[INT] symbol gives the language a way to construct one itself.
//     For the same reason, range_expr has NO differential coverage against
//     the OpenJD reference implementation either: the oracle corpus format
//     (make test-expr-oracle) is a target type plus a bare expression with no
//     symbol table, and nothing in the grammar can write a case that
//     produces a range_expr, so its only check is this package's own tests,
//     written from the spec's worked table in section 3.4.1.1.1 rather than
//     from what this code happens to do.
//
//   - A union target that names a value's own type exactly now admits it
//     unchanged, list types included: Eval("[1.0, 2.0]", nil,
//     expr.UnionOf(expr.ListOf(expr.TFloat), expr.ListOf(expr.TInt)))
//     returns the list rather than failing with "list[float] cannot be
//     coerced to list[float] | list[int]". An earlier revision of this
//     comment recorded that as a known gap; it is closed
//     (coerce.go's directUnionMember). Note the shape of the check, because
//     the two are easy to conflate: coercible() still reports FALSE for a
//     type the target already admits — "does a conversion apply" is a
//     different question from "may this value pass" — so the carve-out lives
//     in coerce(), and it compares whole types rather than type codes, since
//     a code-only test cannot tell a list[int] from a list[string] inside a
//     union.
//
//   - A hard, fixed bound (limits.go's maxElements and maxStringBytes, both
//     10,000,000) applies to any list, string or range_expr this package
//     produces, whether by repetition ("'x' * 3", "[0] * 3") or by
//     concatenation ("'a' + 'b'", "[1] + [2]"). Both producers are checked
//     on both types; string concatenation was the one gap, and a chain of
//     individually-legal repetitions walked 18x past the bound through it.
//     A THIRD bound, maxParseDepth (500), applies before any value exists:
//     the parser's recursive descent is depth-limited, because exhausting the
//     Go stack is a runtime.throw that recover() cannot catch — a 200,000-deep
//     list literal killed the process outright rather than returning an error.
//     "Depth-limited" means every recursion CYCLE in the descent is counted,
//     which is a stronger claim than guarding the entry production and was not
//     true when it was first written: parsePower reads its exponent through
//     parseUnary, and parseUnary falls back through to parsePower, a loop that
//     passed no guard at all, so "2**2**2**…" a million operators long still
//     killed the process. It is counted now (parser.go's enter carries the full
//     enumeration). A FOURTH bound, maxEvalDepth (10,000), does the same for
//     EVALUATION, which the parse guard cannot cover: a left-associative run
//     like "true or true or …" or "1 + 1 + …" is built by a loop, so it costs
//     the parser no recursion at all and passes maxParseDepth however long it
//     is, while the left-deep tree it produces is then walked recursively by
//     evalNode — which overflowed the stack for real between 400,000 and
//     500,000 operators.
//     None of the four is the spec's own configurable memory and operation
//     limits (sections 1.3.9 and 1.3.10) — those remain unimplemented, still
//     sub-project E's, unchanged from sub-project A — so this package must
//     still not be handed untrusted expressions in its present state: the hard
//     bounds stop one operation from allocating unbounded memory and stop the
//     parser and the evaluator from overflowing the stack, not a pathological
//     expression from doing unbounded total work. The one remaining walk over a
//     parsed tree — ast.go's walk, which Expression.Names uses — needs no bound
//     at all any more: it is ITERATIVE, with an explicit stack, so its Go stack
//     depth is constant however deep the tree is. It was recursive, and it was
//     the last unbounded recursion here: 10,000,000 chained operators killed
//     the process with "fatal error: stack overflow", measured. A bound would
//     have been the wrong fix — Names returns []string with no error channel,
//     and a tree that already parsed cannot fail to be walked — so the hazard
//     was removed rather than converted into an error.
//
//   - A float value does not preserve the original source text it was parsed
//     from — "1.100" evaluates to a value that renders as "1.1", not
//     "1.100". Section 1.3.4's requirement that a float pass through a
//     template unchanged, digit for digit, is not implemented here;
//     Value.String is a rendering for diagnostics and tests, not that
//     pass-through, and the same underlying gap now shows up for a list too:
//     Value.String renders a list's string elements unquoted ("[a, b]"),
//     while the reference implementation's to_string() JSON-quotes them
//     ("[\"a\", \"b\"]") — confirmed directly against the reference, not just
//     inferred. Both are sub-project E's to fix, unchanged from sub-project
//     A.
//
//   - Nothing here touches a job template. Parsing a template, binding its
//     parameters, and interpolating an expression's result back into template
//     text are all outside this package; it evaluates expression text handed
//     to it directly.
//
//   - Section 1.1.5's escape table is narrower than its own opening claim
//     that "all Python escape sequences are supported": \a, \b, \f, \v, \0,
//     octal escapes and a backslash-newline line continuation are absent from
//     the table, so this package keeps them verbatim, backslash included,
//     rather than decoding them — "'\a'" evaluates to the two characters
//     "\a". This is a deliberate reading of the table over the prose, not a
//     deferral to a later sub-project.
//
// A target type propagates into a sub-expression from exactly three node
// kinds, and nowhere else — a rule not answerable from outside the package,
// so it is spelled out here rather than left to be inferred from eval.go:
//
//	Cond, the chosen branch (or both, under an unknown condition)  forwards the target
//	Logical ("and"/"or"), both operands                            forwards the target
//	ListLit, each element (through listElemTarget)                 forwards the target
//	Unary / Binary / Compare, all operands                         always TAny instead
//	Index / Slice, the receiver being indexed or sliced             always TAny instead
//	Index's own index; Slice's start/stop/step                     always TInt instead
//
// The three "forwards" rows compute a value that literally IS one of the
// sub-expression's values, so the caller's target applies to it directly.
// Everything else COMPUTES a new value from its operands, so forwarding the
// target would leak context across an operator boundary — a string target
// reaching into "Param.Count + 1" would concatenate its operands into "11"
// rather than add them — and a subscript's own index or a slice's bound is
// fixed at TInt regardless of the target because that position must already
// be an int no matter what the whole expression is being coerced to.
//
//   - This package imports internal/openjd/intrange for range_expr's own
//     grammar, section 3.4.1.1.1's <IntRangeExpr>. internal/openjd expands
//     the identical grammar for a different purpose — a step's task parameter
//     space — with its own pre-existing divergences from both this package
//     and the spec: it orders values first-seen rather than increasing,
//     rejects a range whose start exceeds its end where the spec yields a
//     single value, and rejects a negative step the spec permits. Both
//     callers share intrange's Range type but use different entry points
//     (Parse here, ParseWithPolicy there) precisely because they disagree
//     about what to accept — see the intrange package doc for the full
//     comparison.
//
//   - Test coverage, as of this writing: the OpenJD conformance suite's
//     EXPR/job_templates group is 140/209 passing, 69 fixtures baselined
//     (make test-conformance), and the differential oracle test has 127/158
//     cases agreeing with the reference implementation, 31 baselined
//     divergences (make test-expr-oracle). Most baselined divergences are the
//     reference's own bugs, adjudicated against the spec text and recorded
//     one by one in test/oracle/baseline.txt — that file, not this comment,
//     is the place to check any individual ruling.
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
