// SPDX-License-Identifier: AGPL-3.0-or-later

// Package expr reads and evaluates the OpenJD EXPR extension's expression
// language.
//
// The package is a self-contained leaf with respect to internal/openjd
// itself: the only intra-openjd import is internal/openjd/intrange, a leaf
// shared between the two (see the intrange bullet below), not internal/openjd
// proper. The dependency runs the other way — internal/openjd and
// internal/worker/fmtres import expr — so expr can be tested with no template
// machinery present.
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
// # Behavior and rulings
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
// of that type system. Several behaviors look like bugs when tried by hand.
// They are not:
//
//   - "1 + 2.5" evaluates to 3.5, and "1 < 2.5" to true: section 2.1.1's
//     int-to-float promotion is implemented, and an operator is selected by
//     trying an exact-type match first and a promoting one second. But
//     promotion only ever uses a conversion that cannot fail on any value —
//     int-to-float, path-to-string, range_expr-to-string,
//     range_expr-to-list[int] — never one that can, such as string-to-int.
//     That is why "'a' + 1" is an ERROR rather than "a1": section
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
//     ordering operator's operands "may differ for compatible pairs"). The
//     unification happens during shape matching (shape.go's orderingUnified),
//     so both operands are coerced to the common element type BEFORE any
//     element is compared. Ordering also reaches the empty list, in either
//     operand position and at any nesting depth: section 1.2.6 rule 6 makes
//     list[nulltype] convertible to list[T] for any T, so "[] < [1]" is true,
//     "[1] < []" is false, and "[[]] < [[1]]" and "[[1]] < [[]]" answer the
//     same way one level down. The empty literal's binding of the shared
//     element-type variable is provisional rather than pinning (shape.go's
//     emptyListBinding), and the exception is applied in BOTH directions —
//     an empty binding meeting a real argument, and a real binding meeting an
//     empty argument — which is what makes the orders agree.
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
//     cannot be indexed. list[T] also has its own operators:
//     concatenation ("+"), repetition ("*" by an int, section 2.1.3) and
//     membership ("in"/"not in"), plus list comprehensions (section 1.3.7);
//     see the comprehension bullet below for what that does and does not
//     cover.
//
//   - List comprehensions (section 1.3.7) parse and evaluate:
//     "[x for x in [1, 2, 3] if x > 1]" evaluates. The loop variable's binding
//     follows the section's own shadowing rule — "a loop variable that shadows
//     an existing binding is an error" — checked against the CALLER's symbol
//     table (comp.go's evalListComp), on top of the parser's own
//     <UserIdentifier> casing rule that already keeps a loop variable from
//     colliding with a spec-defined symbol like Param. This package carries no
//     scope information of its own, so the shadowing check is only as precise
//     as the table it is handed: a caller that binds a name which is not
//     actually in scope at this particular expression will produce a false
//     rejection. internal/openjd's checkTemplateExpressions hands it a
//     per-position table built from section 3.6.2's scope model, so a step's
//     own "let" is visible in its own script but not a sibling step's
//     (internal/openjd/exprcheck_let_test.go pins it, section by section).
//     The iterable and the filter are both
//     parsed at the OR level (parser.go's parseOr), not the full
//     <ConditionalExpr> section 1.1.2's own grammar names for them, because
//     that grammar is ambiguous there: parseConditional consumes a bare "if"
//     expecting an "else" to follow it, so "[x for x in y if c]" would fail
//     hunting for one. A conditional in either position therefore needs
//     parentheses — "[x for x in (a if b else c)]" — which is how
//     Python resolves the same ambiguity (comp_for and comp_if both take an
//     or_test, not a full conditional). A string is deliberately not
//     iterable: the spec never defines iterating one, and section 2.1.2
//     already gives "in" over a string a different meaning, a substring test,
//     so "[c for c in 'abc']" is a "cannot be iterated" error rather than a
//     list of characters. A UNION iterable follows the same every-member rule
//     a subscript and a slice apply to a union receiver: it is
//     iterable only when EVERY member is, with the members' element types
//     unified per section 1.2.6. That keeps "range_expr | list[int]" — the
//     union this package manufactures itself for a slice of a range_expr whose
//     length is not yet known — iterable as int, while "list[int] | nulltype"
//     and "range_expr | list[string]" are both rejected rather than
//     typed as their list member. An EMPTY iterable produces "[]" with the
//     element expression never evaluated, and therefore never type-checked:
//     "[Bogus for i in Param.Empty]" is "[]", not an unknown-symbol error.
//     The spec requires no static check of a body that never runs, and the
//     reference implementation answers identically. Rejected outright, each
//     with its own parser error: a second "for" clause and a second "if"
//     filter (Python's multi-generator and multi-filter forms) and a generator
//     expression ("(x for x in y)"). A dict or set comprehension ("{k: k for
//     k in y}", "{x for x in y}") is rejected earlier still, in the lexer —
//     "{" is not a token this grammar defines, so it never reaches a parser
//     production. The comprehension is a second way to do unbounded total
//     work in a single expression, alongside nested repetition: nesting one
//     comprehension inside another's element expression multiplies the inner
//     work by the outer iteration count at every level, the same shape of
//     growth "[[0]*10000000]*10" has. checkElementCount (limits.go's
//     maxElements) bounds one comprehension's own RESULT, not the cumulative
//     work of producing every nested level along the way; section 1.3.10's
//     operation limit bounds that: runComp (comp.go) charges rule 2 for every
//     element the comprehension iterates, at EVERY nesting level, so the
//     multiplied work spends against the same operation budget as the rest of
//     the expression, one charge per level per iteration, and the limit fires
//     before the multiplication runs away
//     (TestOperationLimit_CatchesANestedComprehension). See the BOUNDED
//     EVALUATION bullet below for the two limits themselves.
//
//   - Function, method and property calls parse and resolve — "len([1])",
//     "[1,2].upper()" and "Param.Name.stem" — through the registry
//     functionShapes (funcs.go, assembled by its own mergeFuncs), which holds
//     80 entries: general conversions (len, bool, string, int, float, list,
//     range_expr — funcsconv.go); validation (fail, in the same Go file but
//     its own RFC 0006 category); math (abs, min, max, sum, floor, ceil,
//     round — funcsmath.go); list functions (range, flatten, sorted,
//     reversed, unique, any, all — funcslist.go); the string library, across
//     four files: case transforms and classification (upper, lower,
//     capitalize, title, isdigit, isalpha, isalnum, isspace, isupper,
//     islower, isascii — funcsstrcase.go), trim, affix and search/replace
//     (strip, lstrip, rstrip, removeprefix, removesuffix, startswith,
//     endswith, count, find, rfind, index, rindex, replace — funcsstrfind.go),
//     split, rsplit and join (funcsstrsplit.go) and padding (ljust, rjust,
//     center, zfill — funcsstrpad.go); regular expressions (re_match,
//     re_search, re_findall, re_sub, re_escape, re_split — funcsre.go, behind
//     its own translated dialect in repattern.go — see the regex rulings
//     below); repr_* (repr_sh, repr_cmd, repr_pwsh — funcsreprshell.go — and
//     repr_py, repr_json — funcsreprdata.go — see the repr_* rulings below);
//     the path engine, all of it in funcspath.go over the three-flavor
//     pure-path engine in pathval.go: construction and predicates (path,
//     as_posix, is_absolute), the six properties, which are registered under
//     their __property_*__ spellings and so are six of the 80
//     (__property_name__, __property_stem__, __property_suffix__,
//     __property_suffixes__, __property_parent__, __property_parts__), the
//     with_* family and the relative pair (with_name, with_stem, with_suffix,
//     is_relative_to, relative_to), and with_number, whose substitution
//     scanner lives in pathnumber.go; and apply_path_mapping, registered from
//     its own pathMappingFuncs group beside the engine it wraps
//     (pathmapping.go) rather than from pathFuncs, because it is the one
//     function in the registry that reads SESSION state — the path-mapping
//     rules WithPathMapping threads through evalCtx — see the PATH MAPPING
//     bullet below. Section 2.1.5's "/" and "+" path operators are
//     operator-table rows (ops.go), not registry entries, so they are not
//     among the 80 — see the PATH ENGINE bullet below.
//
//     Section 2.2's roughly 100-function library is entirely registered. An
//     unknown call fails at RUNTIME with "unknown function", a real
//     diagnostic rather than a parse error —
//     "no_such_function('/mnt/share')" reports
//     `unknown function "no_such_function"`, not a syntax error. The example
//     deliberately names something that is not, and never will be, a library
//     function: a real library name would demonstrate a different mechanism.
//     "with_suffix('/foo/bar.txt', '.png')" reports `no signature of
//     "with_suffix" accepts (string, string)` (overload selection), and
//     "apply_path_mapping('/mnt/share')" returns the path "/mnt/share" (a
//     clean evaluation).
//     (Not every call reaches the registry at all: a dunder
//     callee like "__add__(1, 2)" is rejected before lookup with "is a
//     specification naming convention and is not directly callable", and a
//     callee whose whole dotted name resolves to a symbol — "Param.Name()"
//     when Param.Name is bound — fails "Param.Name is not a function".)
//
//     THREE string-function rulings look like bugs when tried by hand, and
//     are not. RFC 0006 defines none of the three, so each is adjudicated
//     here and recorded in test/oracle/baseline.txt where it diverges from
//     the reference.
//
//     isdigit() is ASCII-ONLY, so isdigit('٣') is false — verified directly,
//     and it matches the reference, preserving the guard-then-convert idiom:
//     under a Unicode definition isdigit('٣') would be true while int('٣')
//     still fails. isalnum() then COMPOSES from isalpha and isdigit, which
//     DIVERGES from the reference: sqi answers isalnum('٣') false, since
//     isalpha('٣') and isdigit('٣') are both false, while the reference
//     answers isalnum('٣') true on that same input — a self-contradiction
//     RFC 0006 gives no basis to follow, so the reference's own
//     inconsistency, not agreement with it, is the adjudication (recorded in
//     test/oracle/baseline.txt).
//
//     capitalize('ﬁne day') is "FIne day", not "Fine day" — verified
//     directly, and the reference agrees, so this is NOT a baselined
//     divergence. upper() and lower() apply FULL Unicode case mapping via
//     golang.org/x/text/cases, which expands the ﬁ ligature to two uppercase
//     letters, and capitalize is RFC 0006's "Capitalize first character,
//     lowercase rest" read literally, so the first rune is uppercased rather
//     than titlecased. Python answers "Fine day" only because it titlecases
//     the first character instead.
//
//     title() breaks words on isalnum's OWN predicate, so title('a_b c') is
//     "A_B C" (an underscore separates two words) while title('a1b c') is
//     "A1b C" (a digit does not) — both verified directly. Using the
//     reference's Unicode notion of a word boundary while isalnum stays
//     ASCII-only would put two conflicting definitions of "alphanumeric" in
//     one file, so title('²x y') is the one baselined divergence that ruling
//     produces (test/oracle/baseline.txt).
//
//     Uniform function call syntax (section 1.3.3) makes "x.f(a)" resolve
//     through the same callFunction as "f(x, a)", with the receiver
//     prepended to the argument list — EXCEPT that section 1.2.4 suppresses
//     implicit coercion on a method receiver specifically, a call-site
//     property this package implements (callFunction's methodStyle). The
//     shipped registry shows it directly: round's single-argument row is
//     declared over TFloat alone, so "round(3)" resolves in FUNCTION position
//     by promoting the int argument to float (returning the int 3, since that
//     row's own return type is int), while "Param.N.round()" with Param.N
//     bound to that same int FAILS — "no signature of \"round\" accepts
//     (int)" — because the receiver never gets the promotion the argument
//     position received, verified directly against both call forms.
//     TestReceiverCoercionRestriction (call_internal_test.go) uses the spec's
//     own worked example, startswith(path, string), against the shipped
//     startswith in funcsstrfind.go: the restriction is observable there by
//     COERCION, because a path receiver must fail to coerce into its
//     (string, string) signature. One further case: [].len() and len([])
//     both resolve and both return 0 (TestLen_EmptyListReceiver) — not
//     because the receiver restriction is relaxed, but because there is
//     nothing for it to restrict. list[T]'s receiver-position binding of the
//     unbound element variable T to nulltype (the empty literal's own type)
//     converts no value at all; section 1.2.4 suppresses a COERCION on the
//     receiver, and binding a type variable is not one. That restriction is
//     applied to a PROPERTY receiver as well (resolve.go's evalProperty
//     passes methodStyle), which is an adjudication and not an inherited
//     detail: section 1.3.3 defines "x.p" as method syntax over
//     "__property_p__", and section 1.2.4 suppresses coercion on the receiver
//     of a method call, so a property's receiver is not coerced either. For
//     the path engine's properties it decides, for a
//     "__property_stem__(path)", that "Param.S.stem" on a STRING parameter is
//     an error instead of a silent string-to-path conversion. The reference
//     implementation agrees, checked directly: "'/a/b.txt'.stem" reports
//     "'stem' property is not available for string. Available for: path".
//     TestPropertyReceiverCoercionRestriction pins it with a path receiver
//     against a "(string)" property signature registered locally by the test,
//     which is the only receiver type where the flag is observable in the
//     first place: every other type that reaches a string parameter does so
//     by a conversion overload selection would refuse anyway, so the test
//     would pass with the restriction switched off. Property syntax (section
//     1.3.3) is sugar for a call to __property_p__, dispatched through the
//     same registry, so a property access to a name NOTHING has registered
//     fails through the same unknown-function mechanism — but not with the
//     same message: it is reworded to "unknown property" (resolve.go's
//     evalProperty) so that a genuine runtime error inside a property that
//     DOES exist is never relabeled as a missing one. "Param.Name.nosuchprop"
//     (Param.Name bound to a string) reports `unknown property "nosuchprop"
//     on string`, not `unknown function "__property_nosuchprop__"`.
//     "Param.Name.stem" on that same string receiver takes the other branch:
//     __property_stem__ exists, so it reports `no signature of
//     "__property_stem__" accepts (string)` — the receiver-restriction
//     message, not the unknown-property one. A dotted name that reaches a
//     method or property call is split by LONGEST-PREFIX resolution against
//     the caller's own symbol table (resolve.go's resolveName):
//     "Param.Name.upper()" tries "Param.Name.upper" as a bound symbol first,
//     then "Param.Name", stopping at the first prefix the caller's table
//     actually binds. This package hardcodes no namespaces of its own — the
//     caller's table is authoritative, as it is for the comprehension
//     shadowing check above — so a caller that binds both "a.b" and "a.b.c"
//     gets "a.b.c" resolved to the SYMBOL rather than to a property "c" of
//     "a.b". The type-variable codes (CodeVarT and friends) and the matcher's
//     binding of them live in shape.go; the list[varT] rows — len(), bool(),
//     string()'s list row, and flatten(), sorted(), reversed() and unique()
//     from the list-function group — bind that variable for genuinely
//     polymorphic signatures.
//
//     FIVE regular-expression and repr_* rulings look like bugs when tried by
//     hand, and are not. RFC 0006 defines none of them precisely enough to
//     settle by reading alone, so each was verified directly against Go's
//     regexp package, Python's re module and the reference implementation,
//     and is recorded here and in test/oracle/baseline.txt where it diverges
//     from the reference.
//
//     The accepted regex dialect is NARROWER than the reference's. RFC 0006
//     defines the dialect as "the intersection of Python's re module and
//     Rust's regex crate", and sqi enforces that stated rule rather than
//     merely the spec's own explicit list of rejected constructs, so
//     re_search('3', r'\p{Nd}'), re_search('ab', r'(?<n>a)b') and
//     re_search('a', r'[[:alpha:]]') are all rejected — verified directly —
//     even though the reference accepts all three. Checked against Python's
//     re module directly, not just against the spec text: it rejects \p{Nd}
//     ("bad escape \p") and (?<n>a) ("unknown extension ?<n") outright,
//     matching sqi, but it does not reject [[:alpha:]] at all — it reads the
//     whole bracket expression LITERALLY, as a class holding the characters
//     '[', ':', 'a', 'l', 'p', 'h' followed by a literal ']', so "a" alone
//     does not match it and "a]" does. That is a different rejection from
//     sqi's (sqi refuses to compile the pattern at all; Python compiles it
//     into something that matches almost nothing anyone intended), which is
//     why RFC 0006 excludes POSIX classes from the intersection rather than
//     leaving them to either engine's own reading.
//
//     \W and \S inside a NEGATED character class are rejected — a stated
//     compliance gap: re_search('a!', r'[^\Wa]') errors with "\W inside a
//     negated character class needs set subtraction, which Go's engine
//     cannot express", verified directly, because "not a word character,
//     minus 'a'" is set subtraction and RE2 (which backs Go's regexp, and the
//     reference's own engine) has no operator for it — the reference itself
//     does not reject this pattern, but returns null (no match) for every
//     input, which is not a well-formed reading of the dialect either; see
//     test/oracle/baseline.txt. Inside a POSITIVE class the same shorthand
//     works, by REWRITING the class into an alternation rather than emitting
//     it in place — re_search('a!', r'[\Wa]') matches, verified directly —
//     and a class may hold any number of \W/\S occurrences this way, each
//     distinct shorthand becoming its own alternation branch (scanClass,
//     classNeedsAlternation).
//
//     \d, \w and \s are UNICODE, not Go's ASCII-only defaults — every pattern
//     is translated by translatePattern (repattern.go) before it ever reaches
//     Go's engine, and that translation is what makes them Unicode: verified
//     directly, re_search('٣', '\d') matches the Arabic-Indic digit ٣, which
//     Go's own \d does not.
//
//     repr_sh and repr_py follow the Python functions RFC 0006 names them
//     after — shlex.quote and repr respectively — which produce different
//     TEXT from the reference implementation for the same input, verified
//     directly: repr_sh("it's") is 'it'"'"'s' here (shlex.quote's
//     close-quote/literal-quote/reopen-quote splice) and "it's" there (a
//     different, but shell-safe, quoting strategy); repr_py("it's") is
//     "it's" here (Python repr()'s own quote-switching rule: prefer a single
//     quote, switch to double when the string holds a single quote and no
//     double) and 'it\'s' there (always single-quote, escape instead of
//     switching). repr_sh's difference is TEXTUAL ONLY — both forms are
//     shell-equivalent and safe, unlike a quoting bug, which is why
//     funcsreprshell.go lives apart from the serialization functions in
//     funcsreprdata.go (see its own comment). Both divergences are baselined
//     in test/oracle/baseline.txt as the reference's own bug: RFC 0006 names
//     the Python function, and sqi's output is what that function actually
//     produces.
//
//     repr_json does NOT share writeJSONValue, string()'s list-rendering
//     helper (funcsconv.go) — string(list) and repr_json differ on non-ASCII
//     input, in the reference too, verified directly against both:
//     string(['café']) is ["café"] (non-ASCII left literal, matching the
//     reference exactly) while repr_json('café') is "caf\u00e9" (ASCII-only
//     output with a generic four-hex-digit escape, matching Python's
//     json.dumps default ensure_ascii behavior, which the reference also
//     matches). RFC 0006 calls string()'s list row "the JSON string
//     representation" with no ensure_ascii qualification and separately names
//     repr_json after json.dumps, whose DEFAULT is ensure_ascii=True — two
//     different specified behaviors landing in two different functions on
//     purpose.
//
//   - flatten()'s row ORDER matters — listFuncs (funcslist.go) is the first
//     table in the package where it does. "flatten([[1],[2]])" matches
//     BOTH its nested row (list[list[T]] -> list[T]) and its flat row
//     (list[T] -> list[T]) at cost 0, since [[1],[2]] is simultaneously a
//     list[list[int]] (T = int) and a list[list[int]] read as list[T] with
//     T = list[int]; matchShapesExactFirst breaks an exact-cost tie to the
//     EARLIEST registered shape, not by any property of the argument. The
//     nested row is listed first for that reason: reversing the two
//     would make flatten() the identity on every argument, silently, with no
//     type error to catch it.
//
//   - path and range_expr exist as types — TPath and TRangeExpr participate
//     in Type, coercion and cross-type equality, and a declared parameter can
//     be typed as either. path has no literal syntax of its own, but a value
//     IS produced by evaluation: string -> path is section 1.2.3's own
//     coercion rule, so Eval(src, syms, expr.TPath) returns a path value for
//     any expression that evaluates to a string. path has semantics of its
//     own on top of that — its three flavors, its URI awareness, its
//     properties and functions, and section 2.1.5's "/" and "+" — described
//     in the PATH ENGINE bullet below; apply_path_mapping has the PATH
//     MAPPING bullet to itself. range_expr, by contrast, has no operator of
//     its own beyond what coercion gives it. The range_expr -> list[int]
//     conversion (section 1.2.3) is fully implemented — coercing a range_expr
//     value to a list[int] target expands it — and so is section 1.2.5's
//     list/range_expr cross-type equality rule, both exercised above. What
//     coercion gives a range_expr OPERAND is deliberately narrow, and the
//     narrowing is an adjudication rather than a gap: section 1.2.3's
//     range_expr -> string rule reaches a string PARAMETER only for the
//     operators whose own tables name a range_expr row, which is "+" alone —
//     sections 2.1.2 and 2.1.3 write those rows out explicitly, and would not
//     need to if the coercion fired on its own during overload selection. So
//     "Param.Range + '!'" concatenates as text, while "Param.Range * 2" and
//     "'1' in Param.Range" are "unsupported operand types" errors, not the
//     string repetition "1-101-10" or a substring test over "1-10". The
//     reference implementation rejects both as well.
//
//   - range_expr has real values, and range_expr() (funcsconv.go) constructs
//     one. range_expr(string) parses the spec's <IntRangeExpr> grammar
//     (via internal/openjd/intrange, the same leaf internal/openjd itself
//     uses) and keeps the source text verbatim rather than canonicalizing it;
//     range_expr(list[int]) sorts and de-duplicates its input, then builds
//     the canonical string through canonicalRange (rangeexpr.go) — the same
//     function this package uses to derive a range_expr from a SLICE of one
//     (slice.go's sliceRangeExpr). A CHUNK[INT] task parameter is the other
//     source of one: TaskParamType (paramtypes.go) types it range_expr.
//     Because a bare expression can construct a range_expr with no symbol
//     table, it has DIFFERENTIAL coverage (make test-expr-oracle,
//     corpus.txt's range_expr(...) cases), which shows a canonicalization
//     difference: range_expr(string) keeps "1-10:2" as entered, while the
//     reference reports the canonical "1-9:2" (the end corrected to the last
//     value the step actually produces), and the same difference resurfaces
//     wherever that string is read back out — string(range_expr(...)) and
//     range_expr as an operand of "+". range_expr(list[int]) is NOT part of
//     it: canonicalRange agrees with the reference on every list-constructed
//     case in the corpus. All of it is baselined in test/oracle/baseline.txt.
//
//   - THE PATH ENGINE implements three flavors, every path property and
//     function RFC 0006 defines, and section 2.1.5's "/" and "+" operators.
//     apply_path_mapping has the PATH MAPPING bullet below to itself, because
//     its source matching is deliberately NOT this engine's path equality. A
//     path value here is PURE — no filesystem is touched, nothing is resolved
//     or stat'ed — and it is NORMALIZED BY CONSTRUCTION, because Value.Path
//     re-parses its text. That is what makes RFC 0006 line 767's own
//     guarantee, path(p.parts) == p, hold for every shape, and it is why "/"
//     and "+" re-normalize their results where the reference implementation
//     stores the joined text raw.
//
//     path_format is an EVALUATION option (WithPathFormat, eval.go), not a
//     property of the host, and its default here is POSIX — which differs from
//     the specification's own default of host-native, deliberately. sqi parses
//     templates SERVER-SIDE, so a host-derived default would let one template
//     expand into different tasks depending on which machine submitted it; the
//     spec itself names POSIX as what TEMPLATE scope wants, "to ensure
//     consistent behavior regardless of the submission machine's OS", and
//     template parsing is that scope. PathNative resolves to the running
//     GOOS; internal/worker/fmtres selects it for host-context evaluation on
//     the worker. Verified with no option at all: path('C:/a') renders
//     "C:/a" and its is_absolute() is false (a drive letter means nothing to
//     POSIX), while WithPathFormat(PathWindows) gives "C:\a" and true.
//
//     A URI IS NOT A FLAVOR. It is detected from the text under every
//     path_format, by the spec's own scheme grammar, and a URI NORMALIZES
//     NOTHING: consecutive slashes, "." segments and a trailing slash all
//     survive, because a URI path component is an opaque identifier and "a//b"
//     and "a/b" may name different objects in a store. Verified:
//     path('s3://bucket/a//b/./c') renders back verbatim under both filesystem
//     flavors, and its parts are ["s3://bucket", "a", "", "b", ".", "c"] — the
//     empty component and the "." are real components, where a POSIX or
//     Windows path would have collapsed both.
//
//     "+" ON A PATH RETURNS A PATH. RFC 0006's section 2.1.5 table declares
//     __add__(path, string) with a path return, so "path('/a/b') + 'c'" is
//     the path "/a/bc", verified. path + path reaches that same row through
//     section 1.2.3's path -> string coercion at cost 1, beating the
//     (string, string) row at cost 2, so it is typed path too — the reference
//     types it string, and the divergence is baselined. A caller that wants a
//     string result must ask for one (a string target, or string()).
//
//     THE RULINGS BELOW look like bugs when tried by hand and are not. Each
//     was adjudicated against third_party/openjd-specifications/ and, where it
//     diverges from the reference, recorded case by case in
//     test/oracle/baseline.txt — that file, not this comment, is the place to
//     check an individual row.
//
//     PATH COMPARISON IS BYTE-EXACT AND CASE-SENSITIVE FOR EVERY FLAVOR,
//     WINDOWS INCLUDED. path('C:/A') == path('c:/a') is FALSE here under
//     WithPathFormat(PathWindows); CPython's PureWindowsPath casefolds and
//     answers True (measured). The specification is silent, and the reference
//     agrees with sqi. It is kept for the same reason path_format defaults to
//     POSIX: an expression evaluated server-side must not answer differently
//     because of a host convention, and a case-folding equality would make
//     two templates that differ only in the case of a path literal expand
//     into the same tasks on one flavor and different tasks on another.
//     Determinism and host-independence outrank imitating one platform's
//     filesystem.
//
//     as_posix IS FLAVOR-AWARE: it replaces the FLAVOR'S OWN separator with
//     "/", so it is the IDENTITY under POSIX and on a URI, and a "\" -> "/"
//     rewrite only under Windows. path('/renders/shot_a\b.exr').as_posix() is
//     therefore unchanged, verified. An unconditional backslash rewrite would
//     contradict this engine's own values: under POSIX a backslash is an
//     ordinary filename character, so that same path's parts are ["/",
//     "renders", "shot_a\b.exr"] and its name is "shot_a\b.exr", while such a
//     rewrite answers "/renders/shot_a/b.exr" — a different path with
//     different parts. It would also rewrite a URI object key
//     ("s3://bucket/a\b" to the DIFFERENT key "s3://bucket/a/b"),
//     contradicting the "a URI NORMALIZES NOTHING" rule above. CPython settles
//     the direction: PurePath.as_posix() is str(self).replace(self.parser.sep,
//     '/'), and RFC 0006 line 857 names as_posix among the functions that
//     "match Python's pathlib API" — the same clause that settles stem/suffix
//     below. The reference does the unconditional rewrite and has the
//     identical contradiction (its parts keep the backslash too), so this is
//     a baselined divergence, argued case by case in test/oracle/baseline.txt.
//
//     with_number REPLACES THE LAST MATCH, per RFC 0006 line 822 — "searches
//     the filename stem from the end for these patterns and replaces the last
//     match". A match is one bounded run of ONE placeholder kind (a printf
//     specifier, a '#' run, a digit run), not everything from where a run
//     starts to the end of the stem, so "##a3" holds TWO candidates and only
//     the digits are replaced: with_number('##a3', 7) is "##a7", verified. The
//     reference lets a '#' run swallow the remainder of the stem and answers
//     "07", destroying the literal "a" the caller wrote — a defect against its
//     own specification's wording, not a second reading of it. Do not change
//     sqi to match it; corpus.txt also carries the rows where the two agree,
//     so an overcorrection fails there.
//
//     stem, suffix AND suffixes FOLLOW CURRENT CPython pathlib, which rewrote
//     them to strip the whole LEADING DOT RUN before splitting (and therefore
//     to treat a TRAILING dot as a suffix); the reference implements the older
//     rule, which guarded only a single leading dot. So path('a.').stem is "a"
//     with suffix "." and suffixes ["."], and path('/a/..b').stem is "..b"
//     with no suffix — all verified here, and measured directly against
//     CPython. The same splitStemSuffix backs with_stem, with_suffix and
//     with_number, so the rule reaches those too. THE VERSION BOUNDARY: 3.12
//     still has the old rule and 3.14 has the new one, measured on
//     python3.12 and python3.14 side by side; 3.13 was not tested, so the
//     boundary is somewhere in (3.12, 3.14] and this comment does not name a
//     single version. RFC 0006 says "matches Python pathlib.PurePath behavior"
//     and links the CURRENT documentation, which settles it in favor of the
//     current rule rather than the reference's.
//
//     A TRAILING EMPTY COMPONENT ON THE BASE of relative_to/is_relative_to is
//     CONSUMED. Section 2.1.5 rules that "a trailing slash on the left operand
//     is consumed by the join (matching pathlib behavior)", and the base of
//     these two functions is an operand of exactly that kind, so an
//     operator-written prefix like "s3://renders/" must match the objects
//     under it. Parsing, meanwhile, PRESERVES an empty component verbatim
//     (Expression-Language lines 754-755) — the URI rule above. Together the
//     two rules produce a coherence artifact that looks like a bug:
//     path('s3://b//') == path('s3://b') is FALSE (their parts differ:
//     ["s3://b", "", ""] against ["s3://b"]), and yet each IS relative to the
//     other and relative_to returns "." in BOTH directions — all four
//     verified. Equality compares the parsed values; relative_to consumes a
//     boundary run on the base before comparing. The reference consumes the
//     same run everywhere the receiver has something after the base, and then
//     does not consume it in the self case, which makes its answers internally
//     inconsistent rather than differently ruled.
//
//     THE ORACLE EVALUATES THE REFERENCE UNDER POSIX ONLY
//     (scripts/expr-oracle.py pins PathFormat.POSIX), so it does not
//     adjudicate the Windows flavor: under it the reference accepts a
//     backslash as a replacement name, and never switches on a drive even for
//     a drive-looking receiver like "C:/a/b" (both measured). Every Windows
//     expectation in this engine is therefore pinned by this package's own
//     unit tests against CPython's PureWindowsPath, NOT by make
//     test-expr-oracle, and the corpus carries no Windows-flavor case at all.
//     URI behavior IS oracle-measurable, because a URI is detected under
//     POSIX too, and the corpus covers it heavily.
//
//     THREE SMALLER DIVERGENCES, each deliberate, each recorded beside the
//     code that produces it rather than only here:
//
//     The "\\?\UNC\" extended-length prefix is NOT parsed. pathlib gives that
//     one literal prefix its own start-at-offset-8 parsing, splitting a
//     further server+share out of what follows, so "\\?\UNC\srv\share\x" is
//     ONE opaque root plus "x" to Python; here it runs through the ordinary
//     offset-2 UNC algorithm and comes out with root "\\?\UNC\" and "srv",
//     "share", "x" as ordinary components — a different split, not merely a
//     different rendering, and the only extended-length or device shape known
//     to diverge (a plain "\\?\a\b" or "\\.\a\b" agrees with CPython, measured
//     both ways). Reserved device NAMES ("NUL", "CON") are not special-cased
//     either, and neither are they by pathlib's PURE paths. See parseWindows.
//
//     path('//srv/share/x') / '//srv/share' ANCHORS here, giving
//     "\\srv\share\", where CPython keeps the parent's "x" and answers
//     "\\srv\share\x" (measured). sqi is arguably the more spec-faithful side:
//     section 2.1.5 says an absolute right operand replaces the left entirely,
//     and "//srv/share" is_absolute() is true in both engines. The mechanical
//     cause is that splitRootWindows also ports pathlib's root-SYNTHESIS
//     heuristic, which ntpath.splitroot does not have, so a complete
//     server+share pair carries a root here and the join's anchor arm fires.
//     See pathJoin.
//
//     path('C:') + '//b' YIELDS A URI, "C://b", with parts ["C://b"] and
//     is_absolute() true, under every path_format including Windows — because
//     the spec's own scheme grammar, ^[a-zA-Z][a-zA-Z0-9+.-]*://, accepts a
//     ONE-LETTER scheme (a star, not a plus) and URI detection runs before any
//     flavor is consulted. Special-casing a single-letter scheme so a drive
//     wins would be sqi inventing a rule the specification does not have, on a
//     shape no real template produces. See splitURI.
//
//   - PATH MAPPING. apply_path_mapping runs over a prefix-mapping engine
//     local to this package (pathmapping.go's mapPath). It is registered
//     FLAT — always resolvable, in every scope — from its own
//     pathMappingFuncs group rather than from pathFuncs, because it is the
//     only function in the registry that reads session state: the rules
//     WithPathMapping (eval.go) threads through evalCtx.
//
//     WITH NO RULES IT PASSES ITS INPUT THROUGH, NORMALIZED AS A PATH VALUE
//     IN THE EVALUATION'S FLAVOR: apply_path_mapping('/mnt/share') evaluates
//     with no option at all to the path "/mnt/share" (verified directly).
//     "Passthrough" names the MAPPING outcome — no rule rewrote the text — not
//     a verbatim string return: the result is always re-parsed by boundedPath
//     in ec.pathFormat, so apply_path_mapping('/a//b/') is the path "/a/b" and
//     apply_path_mapping('/a/b') under WithPathFormat(PathWindows) is "\a\b"
//     (both verified). That normalization is required rather than incidental —
//     the vendored fixture expr2.3.2--apply-path-mapping's output_windows line
//     for its POSIX_UNMAPPED case expects a slash-shaped input that matched no
//     rule to come back with Windows separators, which only happens because the
//     passthrough is re-parsed. It is the specified behavior when nothing
//     matches, not a placeholder. internal/worker/fmtres supplies the
//     session's rules through WithPathMapping. Rules are tried in order of
//     DECREASING SourcePath length and the first match wins, so the more
//     specific rule wins whatever order the caller listed them in: with
//     "/a" -> "/short" listed BEFORE "/a/b" -> "/long",
//     apply_path_mapping('/a/b/c') is "/long/c" (verified).
//
//     A WINDOWS SOURCE MATCHES CASE-INSENSITIVELY AND
//     SEPARATOR-INSENSITIVELY, which is deliberately NOT the path engine's
//     equality. Path EQUALITY (==) and relative_to/is_relative_to compare
//     components byte-exactly and case-SENSITIVELY on every flavor including
//     Windows (the PATH ENGINE bullet above explains why: determinism for a
//     server-side expansion). The contrast is with those operations only, not
//     with everything else in the package: the / operator already case-folds,
//     because pathJoin (pathval.go) compares the two operands' DRIVES with
//     strings.EqualFold, as ntpath.join does — so path('C:/a') / 'c:b' keeps
//     the parent and gives "c:\a\b", where path('C:/a') / 'd:b' discards it
//     and gives "d:b" (both verified under WithPathFormat(PathWindows)). That
//     drive test is the only case-insensitive comparison the path engine
//     makes, and the package's only strings.EqualFold outside pathmapping.go.
//     Source MATCHING is a different operation from path EQUALITY, and the
//     two answers disagree on the same pair of paths: a WINDOWS rule whose
//     SourcePath is `C:\studio` maps 'c:/STUDIO/project/scene.ma', while
//     path('C:/Studio') == path('c:/studio') is FALSE and
//     is_relative_to(path('C:/Studio/a'), path('c:/studio')) is FALSE, both
//     under WithPathFormat(PathWindows) — all three verified directly. The
//     insensitivity is confined to the rule comparator (applyFileRule passes
//     strings.EqualFold to relativeParts, which is otherwise the SAME function
//     is_relative_to uses); a POSIX rule does not casefold, so a "/Projects"
//     rule leaves '/projects/a.exr' untouched (verified).
//
//     ITS PARAMETER IS A STRING AND ITS RESULT IS A PATH, which makes the
//     section 1.2.4 receiver restriction visible in a new place: the result
//     chains, so apply_path_mapping('/projects/shot01/out.exr').stem is "out",
//     but a PATH receiver is refused —
//     path('/projects/a').apply_path_mapping() reports `no signature of
//     "apply_path_mapping" accepts (path)`, because a method receiver is not
//     coerced. Both are pinned by
//     TestApplyPathMapping_ResultChainsButAPathReceiverIsRefused. Two calls
//     therefore do not compose directly; ask for a string in between.
//
//     IT HAS NO ORACLE COVERAGE, and cannot get any through the current
//     harness: the reference's evaluation entry points that
//     scripts/expr-oracle.py calls accept no host context, so there is no
//     channel for session mapping rules, and none of test/oracle/corpus.txt's
//     cases name this function. Everything above is pinned by unit tests in
//     this package alone, as the path engine's Windows semantics are.
//
//     Not all of those unit tests encode sqi's own reading. The conformance
//     suite ships three fixtures written against apply_path_mapping itself —
//     EXPR/jobs/expr2.3.2--apply-path-mapping.test.yaml and its
//     --uri-source-path-mapping-posix and --uri-source-path-mapping-windows
//     siblings — carrying 31 hand-authored expected strings across their
//     expected.output/output_posix/output_windows blocks, rules and all. The
//     scored harness never reads them: collectTemplateFixtures
//     (test/conformance/suite_test.go) keeps only the job_templates and
//     env_templates kinds and skips job-execution fixtures, which need a live
//     session runtime. They are therefore transcribed into
//     TestApplyPathMapping_VendoredFixtureExpectations (35 assertions — the
//     four flavor-independent ones run under both formats), which is
//     specification-authored ground truth rather than sqi's own reading, and
//     is the only external check this function has.
//
//     SQI CARRIES TWO PATH-MAPPING IMPLEMENTATIONS, by design.
//     internal/worker/pathmap does a strings.ReplaceAll SUBSTRING swap
//     anywhere in a command string; this engine matches a PREFIX on component
//     boundaries and rebuilds the remainder in the destination flavor. They
//     serve different jobs, and this leaf package cannot import that one
//     anyway.
//
//     HOST-CONTEXT ENFORCEMENT IS NOT HERE. RFC 0006 makes
//     apply_path_mapping valid only in host-context (@fmtstring[host]) scopes
//     and an error anywhere else, and this package has no scope model to
//     enforce that with; internal/openjd's checker does (exprcheck.go's
//     hostOnlyFunctions).
//
//   - BOUNDED EVALUATION implements the specification's two configurable
//     resource limits — section 1.3.9's memory-bounded evaluation and section
//     1.3.10's operation-bounded evaluation — both through meter.go.
//
//     TWO OPTIONS, ON BY DEFAULT, NO UNLIMITED MODE. WithMemoryLimit(bytes)
//     bounds the live memory, in bytes, that one evaluation's values hold at
//     once; it defaults to 100,000,000. WithOperationLimit(ops) bounds the
//     number of section 1.3.10 operations one evaluation may perform; it
//     defaults to 10,000,000 (meter.go). Both apply with NO option passed at
//     all, so a caller that forgets the option is not left unbounded. There
//     is no unlimited escape hatch: a non-positive value to either option is
//     a caller error, reported by Eval (verified directly: WithMemoryLimit(0)
//     and WithOperationLimit(-1) both fail with "... must be positive, got
//     ...").
//
//     THE FLOOR RELATIONSHIP. limits.go's five hard, non-configurable bounds
//     — maxElements, maxStringBytes, maxParseDepth, maxEvalDepth and
//     maxSourceBytes, all described in the hard-bound bullet below — sit
//     UNDERNEATH these two configurable ones and are not raised by them:
//     raising WithMemoryLimit does not raise what one operation may allocate.
//     Evaluating "'a' * 20000000" under WithMemoryLimit(1_000_000_000) — a
//     budget ten times the string it would produce — still fails, and on the
//     SAME error as with no option at all: "col 5: the result is too large:
//     repeating 1 elements 20000000 times exceeds the limit of 10000000",
//     with errors.Is(err, errTooLarge) true and errors.Is(err,
//     errMemoryLimit) false (both measured directly). checkRepeat's
//     maxStringBytes ceiling (limits.go) is checked before any
//     WithMemoryLimit accounting ever sees the string.
//
//     LIMITS ARE PER-Eval, NOT PER-TEMPLATE. Each call to Expression.Eval
//     (and Eval, EvalWithMetrics, EvalForBalanceCheck, which route through
//     it) starts a fresh meter with its own budget; nothing here accumulates
//     a running total across multiple expressions evaluated for one
//     template. The cumulative, template-wide budget is internal/openjd's
//     (exprcheck.go's templateBudget).
//
//     THE UNRESOLVED-OPERAND RULING. A dispatched call whose argument is
//     still unresolved — the type-check path taken before a parameter is
//     bound to a value — charges section 1.3.10 rule 1 only, the single
//     operation for the call itself (call.go's callFunction, plus three
//     mirroring sites in ops.go — binary, unary and the fast-path equality
//     check — four unresolved-operand charge sites in all). Rules 2 and 3
//     do not apply: the call was dispatched,
//     but no list was iterated and no string was processed, because nothing
//     downstream of the unresolved operand actually ran. This ruling has no
//     counterpart to probe against the reference implementation, which has
//     no comparable unresolved-operand evaluation path.
//
//     RULE 2'S ENUMERATION IS OPEN, AND THE TWO SPECIFICATION DOCUMENTS
//     DISAGREE ABOUT IT. It is settled here, once, so that the package cites
//     one reading. The divergence is in the specification, not in a
//     misreading of it:
//
//     wiki/2026-02-Expression-Language.md lines 1080-1087 introduces rule
//     2's function list with "such as" — an OPEN enumeration.
//     rfcs/0005-expression-language.md lines 1173-1182 introduces the same
//     list with "This applies to:" — read literally, a CLOSED one.
//
//     THE RULING IS OPEN, on four grounds. (1) The wiki page is THE
//     SPECIFICATION for this package (see the closing note at the bottom of
//     this file); RFC 0005 is the proposal that produced it, and where the
//     two disagree the specification governs — the same precedence this
//     package already applies to the reference implementation. (2) BOTH
//     documents state the same operative sentence first — "When a function
//     or the evaluator iterates through every element of a list, the number
//     of elements is added to the operation count" — and both introduce
//     rule 3's parallel list with "and similar", openly, in the RFC too. A
//     closed rule 2 beside an open rule 3 in one document, on two rules of
//     identical construction, is not a reading either document argues for.
//     (3) A closed reading makes rule 2's own words "or the evaluator"
//     INOPERATIVE for anything unnamed: no evaluator-performed iteration
//     except the list comprehension is in the enumeration, and the
//     comprehension is listed separately because the evaluator, not a
//     function, performs it — so the general clause must reach further than
//     the list, or it says nothing. (4) A closed reading defeats the rule's
//     stated purpose: under it, five constructs that do O(receiver) work are
//     charged a flat 1, and
//     "len([range_expr('1-1000000')[0] for x in range(500)])" ran 1.8
//     seconds for 2,002 counted operations, scaling to roughly half an hour
//     while staying under a fifth of the default budget. Charging them (see
//     below) closes that.
//
//     WHAT THE OPEN READING DOES NOT LICENSE. It is not a mandate to charge
//     everything: the question each row still answers is whether the work
//     is element-count-dominated or byte-scan-dominated, and whether the
//     implementation PROVABLY does that work. split() stays uncharged for
//     rule 2 because its work is byte-dominated; "not", unary minus and
//     unary plus stay uncharged because they touch no list and no string at
//     all; min/max's fixed-arity 2- and 3-argument overloads stay uncharged
//     because they take no list-typed parameter. "Unnamed by the
//     enumeration" is not a reason to charge nothing, and "named by the
//     enumeration" is not required to charge.
//
//     FIVE CONSTRUCTS THAT DO O(receiver) WORK, CHARGED FOR IT. Each of
//     these expands or decodes its whole receiver inside a single call, and
//     each is charged as the SLICE form of the identical expression is:
//
//     subscript of a string receiver   (list.go's evalIndex) — indexValue
//     does []rune(recv.AsStr()), decoding every byte of the receiver to
//     find rune boundaries. Charges rule 3 on the receiver, before the
//     decode: a 500,000-byte receiver costs 1,954, not 1.
//     subscript of a range_expr receiver (same) — indexValue calls
//     rangeInts, which fully expands. Charges rule 2 on the
//     receiver's element count, from rangeExprCount (arithmetic for a
//     single sub-range, a bounded expansion for two or more): a
//     1,000,000-element receiver costs 1,000,001, not 1.
//     "in"/"not in" with a range_expr container (ops.go) —
//     containsRangeInt calls rangeInts. Cost{ArgElements: {1}},
//     index 1 being the container, matching the string and list rows
//     beside it.
//     min() and max() over a range_expr (funcsmath.go) — both bodies are
//     byte-identical rangeInts calls, and both charge
//     Cost{ArgElements: {0}}, as sum()'s range_expr row one table over
//     does for the same body.
//
//     A SUBSCRIPT OF A LIST STAYS AT A FLAT 1, deliberately: AsList returns
//     the backing slice with no copy (value.go), so a list subscript
//     touches one element and iterates nothing. The receiver-kind
//     split is the same one slice.go's sliceValue establishes and
//     the same asymmetry the implementations themselves have.
//
//     LIST ORDERING CHARGES ITS WALK, like list equality. "[...] < [...]"
//     and "[...] == [...]" run the SAME compareLists/valuesEqual walk over
//     the same operands. Rule 2 names "list/range equality comparisons" by
//     token and does not name ordering, but under the open reading the
//     general sentence is satisfied either way, and charging only equality
//     measured a 1,666x discrepancy: a 20,000-int list compared 2,000 times
//     completed at 6,002 operations under "<" while the identical "=="
//     expression tripped the 10,000,000 limit. orderingShapes' list/list row
//     declares Cost{ArgElements: {0}}, charging the LEFT operand exactly as
//     applyBinary's equality path does. The four scalar ordering rows
//     charge rule 1 only; they iterate nothing.
//
//     THE COUNTING RULINGS. Where sqi's operation counter and the
//     reference's diverge on a corpus case, the case is adjudicated and
//     recorded in test/oracle/baseline-ops.txt, grouped under root-cause
//     families (its own "# ── " section headers enumerate them; that file,
//     not this comment, is where to check one entry). Most are the reference
//     over-counting by a flat +1 on operators RFC 0005's own
//     AST-transformation table routes around section 1.3.10 rule 1 entirely
//     rather than through a genuine function call — ordering, equality,
//     "not", "and"/"or", the ternary, "in"/"not in" — adjudicated as the
//     reference's own bug rather than a gap here. A second class is the two
//     pricing the same call differently (string(list), the repr_* family,
//     unique()'s per-comparison charge — see below), or sqi charging nothing
//     where the reference charges (round(x, ndigits>0), min/max's fixed 2-
//     and 3-arg rows) — each argued from the RFC 0006 text in that file's
//     own section for it.
//
//     THE SECTION 1.3.10 WORKED EXAMPLE'S OWN ARITHMETIC IS WRONG. The
//     specification states 'a' * 100000 costs 393 operations; the correct
//     total is 392: ceil(100000/256) is 391 (256*390 = 99840, leaving 160),
//     plus 1 for the repetition call itself. Measured directly —
//     EvalWithMetrics("'a' * 100000", nil, TString) reports 392, not 393 —
//     and the reference implementation agrees with 392. The RULE
//     (meter.go's ceilDiv256) is right; only the specification's own
//     addition is off, and meter.go records the correction beside the
//     function so a reader checking it against the spec does not conclude
//     it has an off-by-one.
//
//     Value.String() AND string(list) RENDER A LIST IDENTICALLY, quoting and
//     escaping included. openjd-specifications#176 requires format-string
//     interpolation to use "this same conversion" as string() and the result
//     to "parse as JSON"; Go's spelling of a control character (\x01, and
//     \a/\v for two JSON does not name) is not JSON at all. Both renderers
//     call funcsconv.go's jsonQuoteElement, so the agreement holds BY
//     CONSTRUCTION for every input. TestValueString_VersusStringFunction
//     checks thirteen cases (an embedded double quote, non-ASCII, angle
//     brackets and an ampersand, a newline, a backslash, tab, backspace,
//     formfeed, carriage return, an emoji, U+2028/U+2029, and the empty
//     string), and TestValueString_ListQuotingIsJSONEverywhere pins six more
//     where strconv.Quote and encoding/json disagree (the C0 controls,
//     vertical tab, DEL and invalid UTF-8). The separator still differs from
//     internal/openjd's canonical STORAGE form ("," there, ", " here), and
//     paramjson.go states that difference where it lives.
//
//     unique() CHARGES PER COMPARISON, NOT PER ELEMENT. A registry-level
//     Cost{ArgElements: {0}} charge prices only the INPUT length, and under
//     it unique(range(20000)) completed in roughly 4 seconds with no error,
//     running on the order of 4*10^8 real valuesEqual comparisons while only
//     20,001 operations were counted against the 10-million default limit.
//     uniqueList (funcslist.go) is an FnCtx function charging one operation
//     per valuesEqual comparison, and the charge fires BEFORE the comparison
//     it prices, so the limit stops the quadratic work while it is in
//     progress rather than billing for it after the fact
//     (TestUnique_IsBoundedByTheOperationLimit).
//
//     THE TOP-LEVEL COERCION IS METERED. coerceTop (eval.go), which every
//     public entry point — Expression.Eval, Eval, EvalWithMetrics,
//     EvalForBalanceCheck — runs on the whole expression's result, releases
//     the pre-coercion value and allocates the coerced result with
//     WithMemoryLimit's bound checked, at all four entry points. Coercion
//     (coerce.go) can turn a small live range_expr into an arbitrarily large
//     list[int]; unmetered, that materialization would be bounded only by
//     the fixed maxElements floor (10,000,000 elements, roughly 640MB at 64
//     bytes a value), with WithMemoryLimit never consulted on that path.
//
//     UNTRUSTED EXPRESSIONS. With both limits on by default
//     (TestOperationLimit_*, TestMemoryLimit_* and
//     TestUnique_IsBoundedByTheOperationLimit), this package may be handed
//     untrusted expressions, with three residuals: the fixed floor
//     (limits.go) still bounds what a single operation may allocate,
//     unchanged by either configurable limit; maxParseDepth and
//     maxSourceBytes still guard PARSING, a phase no evaluation-time budget
//     can reach at all, so what a hostile expression may spend before a
//     single operation charge exists to stop it is whatever those two hard
//     bounds allow and nothing narrower; and limits are per-Eval, so a
//     template with many expressions gets many independent budgets, one per
//     expression evaluated — the cumulative per-template budget is
//     internal/openjd's.
//
//     NONE OF IT BOUNDS PARSING. Every limit named in this bullet — the two
//     configurable ones, the floor beneath them, the template-wide budget
//     internal/openjd layers on top and the deadline below — is consulted
//     from exactly two places, meter.charge and meter.reserve, and both of
//     those run during EVALUATION. Parse runs first: tokenize builds the
//     entire token slice and the parser the entire tree before any of the
//     above exists to be spent. maxParseDepth is no help, because it bounds
//     recursive-descent STACK FRAMES and a flat left-associative chain is
//     read in a loop. Measured without a source-length bound, with EXPR
//     supported and the walk live: one 4,000,001-byte "1+1+1+…" expression
//     parsed SUCCESSFULLY in 544 ms, holding 427.6 MB of live heap and
//     churning 1,403.3 MB; through the submission path a 4 MiB request body
//     — what POST /api/v1/jobs accepts, anonymously when auth is off — cost
//     ~765 MB of peak heap per in-flight request, and six concurrent
//     requests reached 4,582 MB. An already-expired deadline changed
//     nothing: the parse finished before anything read the clock.
//     maxSourceBytes (limits.go, 10,000 bytes, checked at the top of Parse
//     before tokenize) is what closes it — the one bound in this package that
//     applies BEFORE evaluation begins, and therefore the only one that can.
//     It is a 4xx verdict, not a 503: a source that is too long is a
//     deterministic property of the template, so it reaches a caller as an
//     ordinary parse error (a ValidationError through internal/openjd) rather
//     than through ErrDeadlineExceeded.
//
//     THE WALL-CLOCK DEADLINE (WithDeadline) is a bound of a different
//     shape. Everything above bounds WORK — bytes held, operations
//     performed, elements built, nesting depth. None of them bounds TIME,
//     and they cannot: section 1.3.10 prices 256 bytes at one operation, so
//     '("x" * 900000).title()' spends ~7,000 of a 10,000 operation budget
//     while costing ~57 ms of CPU, and a request that stays inside every
//     budget above still measures in minutes. WithDeadline(t) sets an
//     absolute wall-clock instant after which evaluation stops. The zero
//     time — the default — means no deadline and reads no clock.
//
//     THREE WAYS IT DIFFERS FROM EVERY OTHER BOUND HERE:
//
//     (1) IT IS WALL-CLOCK, therefore NON-DETERMINISTIC. The same template
//     on the same input breaches it on a loaded host and not on an idle one.
//     Every other bound in this section is a function of the expression
//     alone.
//
//     (2) IT IS PER-REQUEST, NOT PER-Eval. The option takes an absolute
//     time rather than a duration so that one deadline spans the many
//     evaluations a template walk performs — a per-evaluation duration
//     would let each of hundreds of positions spend the whole allowance,
//     which is the opposite of a bound. That is also why meter.charge checks
//     it on the FIRST charge of every evaluation and not only on the
//     interval: a template built entirely from cheap expressions must still
//     stop, and each of those expressions gets a fresh meter.
//
//     (3) A BREACH MEANS THE SERVER GAVE UP, NOT THAT THE TEMPLATE IS
//     INVALID. Every other bound here is a verdict about the expression, and
//     a caller is right to reject the submission (422). ErrDeadlineExceeded
//     is exported, and distinct from every budget error, so that a caller can
//     tell the two apart STRUCTURALLY (errors.Is) rather than by reading a
//     message, and report it as a transient server condition (503).
//
//     PRECEDENCE, WHEN A SINGLE CHARGE BREACHES BOTH: the operation limit
//     wins. meter.charge checks it first, deliberately — the deterministic
//     verdict is the more useful one, and reporting an over-budget template
//     as a transient server condition would invite a client to retry
//     something that can never succeed
//     (TestDeadline_OperationLimitBeatsTheDeadline). meter.reserve orders
//     the two the same way.
//
//     IT IS A BACKSTOP, NOT A BUDGET, AND ITS GRANULARITY IS COARSE. The
//     clock is SAMPLED — on the first charge and every deadlineCheckInterval
//     (1024) charges thereafter — because charge is the hot path of every
//     expression and a time.Now() on each call taxes evaluations that are
//     nowhere near a deadline. So the guarantee is "the deadline plus at most
//     one evaluation's remaining operation budget", not "the deadline":
//     up to 1023 further charge calls may run, each of them potentially a
//     bulk operation, and the call count is held to the operation budget
//     only because a charge that prices bulk work adds at least one
//     operation to it. meter.reserve narrows the worst of that window by
//     checking unconditionally, with the count in hand, immediately before
//     each large materialization — so the single biggest construction inside
//     the window never starts rather than being caught after it completes.
//     The rest of the window is not closed here.
//
//   - A union target that names a value's own type exactly admits it
//     unchanged, list types included: Eval("[1.0, 2.0]", nil,
//     expr.UnionOf(expr.ListOf(expr.TFloat), expr.ListOf(expr.TInt)))
//     returns the list rather than failing with "list[float] cannot be
//     coerced to list[float] | list[int]". coerce() (coerce.go) checks
//     SATISFACTION first — a value whose type the target already admits is
//     returned unchanged — comparing whole types rather than type codes,
//     since a code-only test cannot tell a list[int] from a list[string]
//     inside a union. Do not conflate that with coercible(), which answers a
//     different question: whether an implicit CONVERSION rule carries a
//     TYPE to the target while a function call is being resolved.
//
//     nulltype is the one case where the two questions coincide:
//     coercible(nulltype, to) reports TRUE whenever `to` admits null
//     (coerce.go), because for null "the target already admits it" and
//     "coercible" are the same question — there is no separate
//     null-to-something conversion rule to ask about, only whether the
//     target's union names null at all. coercible(nulltype, "string?") is
//     true because the target admits it; TestCoercible's nulltype rows pin
//     this, alongside the negative boundary ("nulltype does not reach a
//     union that does not name null") showing the rule still checks target
//     membership rather than admitting null everywhere.
//
//   - A hard, fixed bound (limits.go's maxElements and maxStringBytes, both
//     10,000,000) applies to any list, string or range_expr this package
//     produces, whether by repetition ("'x' * 3", "[0] * 3") or by
//     concatenation ("'a' + 'b'", "[1] + [2]"). Both producers are checked
//     on both types; without the string-concatenation check, a chain of
//     individually-legal repetitions walks 18x past the bound.
//     A THIRD bound, maxParseDepth (500), applies before any value exists:
//     the parser's recursive descent is depth-limited, because exhausting the
//     Go stack is a runtime.throw that recover() cannot catch — a 200,000-deep
//     list literal would kill the process outright rather than return an
//     error. "Depth-limited" means every recursion CYCLE in the descent is
//     counted, not only the entry production: parsePower reads its exponent
//     through parseUnary, and parseUnary falls back through to parsePower, so
//     an unguarded cycle lets "2**2**2**…" a million operators long kill the
//     process (parser.go's enter carries the full enumeration). A FOURTH
//     bound, maxEvalDepth (10,000), does the same for EVALUATION, which the
//     parse guard cannot cover: a left-associative run like "true or true or
//     …" or "1 + 1 + …" is built by a loop, so it costs the parser no
//     recursion at all and passes maxParseDepth however long it is, while the
//     left-deep tree it produces is then walked recursively by evalNode —
//     which, unbounded, overflows the stack between 400,000 and 500,000
//     operators.
//     A FIFTH bound, maxSourceBytes (10,000 bytes), applies EARLIER THAN ANY
//     of them — at the top of Parse, before tokenize reads a byte — because
//     the four above leave the whole PARSE unmetered: without it, a single 4
//     MB flat chain parses successfully in 544 ms while holding 427.6 MB of
//     live heap, passing maxParseDepth untouched for the reason the fourth
//     bound's sentence gives, and reaching maxEvalDepth only after every one
//     of those bytes has been tokenized and turned into nodes. It bounds
//     source LENGTH rather than node count on purpose; see limits.go for why,
//     and for the headroom over real templates (the largest expression in
//     any vendored fixture, sample or preset is 99 bytes).
//     None of the five is the spec's own configurable memory and operation
//     limits (sections 1.3.9 and 1.3.10) — those are a SEPARATE mechanism,
//     covered in the BOUNDED EVALUATION bullet above. What the five bounds
//     here do: stop one operation from allocating unbounded memory, stop the
//     parser and the evaluator from overflowing the stack, and stop one
//     expression's SOURCE from being large enough that merely reading it
//     costs hundreds of megabytes — a narrower job than the configurable
//     limits, which stop a pathological expression from doing unbounded
//     TOTAL work across many operations. The one remaining walk over a
//     parsed tree — ast.go's walk, which Expression.Names uses — needs no
//     bound: it is ITERATIVE, with an explicit stack, so its Go stack depth
//     is constant however deep the tree is (a recursive walk dies with
//     "fatal error: stack overflow" on 10,000,000 chained operators,
//     measured). A bound would be the wrong fix — Names returns []string
//     with no error channel, and a tree that already parsed cannot fail to
//     be walked.
//
//   - A float LITERAL in expression source does not preserve the text it was
//     parsed from — "1.100" evaluates to a value that renders as "1.1", not
//     "1.100". Value.String is a rendering for diagnostics and tests, not
//     section 1.3.4's pass-through, and no producer sets the literal path's
//     carry. Value has an optional rendered form (value.go's fs field,
//     exported construction via FloatText), set by round(x, ndigits) for a
//     positive ndigits because RFC 0006 requires round(3.5, 2) to render
//     "3.50" — a form no float64 carries on its own
//     (TestRound_CarryIsRoundOnlyAndDoesNotPropagate) — and by ValueFromText
//     (paramtypes.go) for the other half of section 1.3.4: a FLOAT JOB
//     PARAMETER's submitted text, which sqi has verbatim because parameters
//     are stored as map[string]string. internal/openjd's phase-2
//     concreteJobParamValue (exprcheck.go) and the worker both bind through
//     it, so Param.<name> for a submitted "3.500" carries that text and
//     Value.String reports it. internal/worker/fmtres renders phase-3 values
//     onto a real command line, and pins that a submitted "3.500" arrives as
//     3.500 through both plain substitution and repr_py. Value.String quotes
//     a list's string elements as string()'s JSON row (funcsconv.go) does;
//     the BOUNDED EVALUATION bullet above states how far the two renderings
//     agree.
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
//     "\a". This is a deliberate reading of the table over the prose.
//
// A target type propagates into a sub-expression from exactly four node
// kinds, and nowhere else — a rule not answerable from outside the package,
// so it is spelled out here rather than left to be inferred from eval.go:
//
//	Cond, the chosen branch (or both, under an unknown condition)  forwards the target
//	Logical ("and"/"or"), both operands                            forwards the target
//	ListLit, each element (through listElemTarget)                 forwards the target
//	ListComp, the element expression (through listElemTarget)      forwards the target
//	ListComp, its iterable; its filter                             always TAny; always TBool
//	Unary / Binary / Compare, all operands                         always TAny instead
//	Index / Slice, the receiver being indexed or sliced             always TAny instead
//	Index's own index; Slice's start/stop/step                     always TInt instead
//	Call, its receiver and every argument                          always TAny instead
//	Access, the receiver being accessed                             always TAny instead
//
// The four "forwards" rows compute a value that literally IS one of the
// sub-expression's values, so the caller's target applies to it directly —
// ListComp's element expression joins ListLit's for the same reason: each of
// a comprehension's produced elements IS the element expression's value, one
// per iteration, as each of a literal's elements is. Everything else
// COMPUTES a new value from its operands, so forwarding the target would leak
// context across an operator or call boundary — a string target reaching
// into "Param.Count + 1" would concatenate its operands into "11" rather than
// add them — and a subscript's own index or a slice's bound is fixed at TInt
// regardless of the target because that position must already be an int no
// matter what the whole expression is being coerced to. A call's return type
// is fixed by the signature callFunction selects, not by the caller's target,
// so a target reaching into an argument would let the CALLER'S context change
// which overload the callee resolves to.
//
//   - COMPILED REGULAR EXPRESSIONS ARE CACHED FOR THE LIFETIME OF ONE
//     EVALUATION, and for that lifetime only (recache.go). Without the cache,
//     every re_match/re_search/re_findall/re_sub/re_split invocation runs
//     translatePattern's full byte scan and regexp.Compile again, so a
//     pattern inside a comprehension is compiled once PER ELEMENT while
//     section 1.3.10 charges it roughly one operation plus ceil(len/256) —
//     under-priced by about three orders of magnitude, and on the WORKER
//     there is no wall-clock deadline to backstop it at all. Measured on one
//     development machine, '[s for s in Param.Files if re_match(s,
//     r"shot\d+\.exr") != null]' over 10,000 elements: 30.4 ms and 48.6 MB
//     uncached, 12.0 ms and 13.1 MB cached (380,097 allocations against
//     70,062).
//
//     IT CHANGES NO RESULT AND NO COUNT. Section 1.3.10's charges are applied
//     by callShape (ops.go) around the whole call, before Fn or FnCtx runs, so
//     a cached compile is charged exactly what an uncached one is — the
//     differential oracle's operation-count dimension is unmoved. A failing
//     pattern is cached too, and its error value is returned unchanged on
//     every hit, so an invalid pattern in a comprehension is rejected once
//     with the message it always had.
//
//     IT IS CAPPED AT maxCachedPatterns ENTRIES. A pattern is an arbitrary
//     string from a submitted template, not necessarily a literal, so
//     "[re_match(s, s) for s in Param.Files]" presents one DISTINCT pattern
//     per element; an uncapped map would retain one compiled program per
//     element and turn a comprehension into a memory amplifier. Past the cap
//     the cache stops storing — nothing is evicted and the map never grows
//     again — so what it retains is a constant, not a function of the input.
//     Being per-evaluation is what makes that bound hold across requests, and
//     is also why the cache needs no eviction policy and no lock.
//
//   - This package imports internal/openjd/intrange for range_expr's own
//     grammar, section 3.4.1.1.1's <IntRangeExpr>. internal/openjd expands
//     the identical grammar for a different purpose — a step's task parameter
//     space — with its own divergences from both this package and the spec:
//     it orders values first-seen rather than increasing, rejects a range
//     whose start exceeds its end where the spec yields a single value, and
//     rejects a negative step the spec permits. Both callers share intrange's
//     Range type but use different entry points (Parse here, ParseWithPolicy
//     there) because they disagree about what to accept — see the intrange
//     package doc for the full comparison.
//
//   - Test coverage. The OpenJD conformance suite's EXPR/job_templates group
//     is scored by make test-conformance. The protected-fixture tests
//     (TestConformance_*Fixtures in test/conformance/suite_test.go)
//     additionally assert by name that specific .invalid fixtures are
//     rejected: a
//     construct this comment describes as rejected, such as the
//     comprehension shadowing check or a parser/lexer rejection of a form
//     Python allows; the conversion and math functions REJECTING an invalid
//     argument (an empty list or path into bool(), an unrecognized string
//     into bool(), an empty list to min/max, an unrepresentable
//     float-to-int, NaN and infinity into float(), an empty string or list
//     into range_expr()); the string functions REJECTING an invalid argument
//     (an empty substring into count/find/replace, an empty separator into
//     split/rsplit, a missing substring into index/rindex); the regex scanner
//     and re_sub replacement check REJECTING an unsupported construct (an
//     empty pattern, \z, lookahead, lookbehind, a named backreference, and
//     re_sub's four group-reference spellings); and the path functions —
//     relative_to's own errNotRelative, with_number's own errPaddingTooWide,
//     section 1.2.4's receiver restriction refusing to coerce a path receiver
//     into a string method, and bool()'s path row. Each must be rejected by
//     real validation rather than by an accidental "unknown function", so
//     that one fixture starting to pass for the wrong reason while another
//     regresses cannot hide inside an unchanged aggregate score. Such a test
//     asserts the fixture is still
//     REJECTED, not which mechanism rejected it, so an entry pins a specific
//     check only where no second mechanism rejects the same input.
//     The differential oracle (make test-expr-oracle) compares VALUES against
//     the reference implementation over test/oracle/corpus.txt, and, on every
//     case whose value already agrees, OPERATION COUNTS (EvalWithMetrics
//     against the reference's evaluate_with_metrics; a case with a value
//     divergence is reported once and not double-counted). Value divergences
//     are adjudicated in test/oracle/baseline.txt, count divergences in
//     test/oracle/baseline-ops.txt (see the BOUNDED EVALUATION bullet's
//     COUNTING RULINGS paragraph). The path corpus is large and its
//     divergences are the reference's, argued family by family in
//     test/oracle/baseline.txt (its non-normalizing "/" and "+", its
//     flavor-blind as_posix, its pre-current-pathlib stem/suffix split, its
//     with_number hash-swallow, its inconsistent trailing-slash consumption
//     on a relative_to base, and its treatment of a bare "scheme://" as a
//     wildcard authority). The baselined value entries include the
//     range_expr(string) canonicalization difference described above and one
//     round() case: sqi returns int for round(x, ndigits) when ndigits <= 0,
//     per RFC 0006's own signature table, while the reference returns a
//     FLOAT — round(1234.5, -1) is "1230 : int" here and "1230.0 : float"
//     there. The specification outranks the reference (it is Beta, 0.x,
//     breaking changes permitted in minor bumps), so this is adjudicated as
//     the reference's bug and recorded in test/oracle/baseline.txt, not fixed
//     here to match it. Also included are two of the string-function rulings
//     argued above — isalnum's composition (isalnum('٣')) and title's
//     ASCII-only word boundary (title('²x y')) — while capitalize's ligature
//     expansion is NOT baselined, since the reference agrees with it
//     outright; and three of the regex and repr_* rulings argued above — the
//     intersection-rule refusals (\p{Nd}, (?<n>a), [[:alpha:]]), the
//     [^\W...] set-subtraction gap, and repr_sh/repr_py's textual differences
//     from the reference — while \d/\w/\s's Unicode semantics are NOT
//     baselined, since they are what the oracle is checking agreement on, and
//     repr_json's non-ASCII escaping is NOT baselined either, since the
//     reference matches it exactly. The path rulings above are baselined
//     where they diverge, while case-SENSITIVE path comparison is NOT — the
//     reference agrees with sqi there, and it is CPython's PureWindowsPath
//     that disagrees with both, which the oracle does not see because it runs
//     the reference under POSIX only. apply_path_mapping has no oracle case
//     (see the PATH MAPPING bullet above). Most baselined divergences are the
//     reference's own bugs, adjudicated against the spec text and recorded
//     one by one — those files, not this comment, are the place to check any
//     individual ruling.
//
// Anything unimplemented FAILS rather than silently misbehaving, with one
// deliberate exception: the escape sequences named above pass through
// verbatim instead of erroring or being decoded. The grammar is EXPR's own
// rather than borrowed from a Python parser so that the failure runs in that
// direction: a borrowed parser would accept syntax this package cannot
// evaluate.
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
//     Shape entries; a candidate list with no admissible match is reported
//     as "unsupported operand types".
//
// The specification is the OpenJD wiki page "Expression Language [Extension:
// EXPR]", pinned in the third_party/openjd-specifications submodule. Section
// numbers cited in this package's comments refer to it.
package expr
