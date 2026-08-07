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
//     genuinely cannot be indexed. list[T] also has its own operators:
//     concatenation ("+"), repetition ("*" by an int, section 2.1.3) and
//     membership ("in"/"not in") — and, as of this wave, list comprehensions
//     (section 1.3.7) too; see the comprehension bullet below for what that
//     does and does not cover. Subscript and slice have NO differential
//     coverage against the OpenJD reference implementation (make test-expr-oracle),
//     though: verified directly against it under every target tried (int,
//     list[int], float, string, and even a matching type), the reference
//     mis-infers a subscript's or slice's own static result type whenever
//     ANY target type is supplied at all, and is correct only with none — a
//     bug in the reference, not here (see the root-cause-B entries in
//     test/oracle/baseline.txt, which baseline all 22 such corpus cases as a
//     known reference defect). So subscript and slice are checked by this
//     package's own tests and the conformance fixtures only.
//
//   - List comprehensions (section 1.3.7) are complete, parse and eval:
//     "[x for x in [1, 2, 3] if x > 1]" evaluates. The loop variable's binding
//     follows the section's own shadowing rule — "a loop variable that shadows
//     an existing binding is an error" — checked against the CALLER's symbol
//     table (comp.go's evalListComp), on top of the parser's own
//     <UserIdentifier> casing rule that already keeps a loop variable from
//     colliding with a spec-defined symbol like Param. The shadowing check is
//     only as precise as the caller's table: this package carries no scope
//     information of its own, so a caller that binds a name which is not
//     actually IN SCOPE at this particular expression — a "let" bound
//     somewhere else in the template, say — will produce a false rejection.
//     Real scope tracking is sub-project E's, where template integration
//     threads an actual scope through. The iterable and the filter are both
//     parsed at the OR level (parser.go's parseOr), not the full
//     <ConditionalExpr> section 1.1.2's own grammar names for them, because
//     that grammar is ambiguous there: parseConditional consumes a bare "if"
//     expecting an "else" to follow it, so "[x for x in y if c]" would fail
//     hunting for one. A conditional in either position therefore needs
//     parentheses — "[x for x in (a if b else c)]" — which is exactly how
//     Python resolves the same ambiguity (comp_for and comp_if both take an
//     or_test, not a full conditional). A STRING IS DELIBERATELY NOT
//     ITERABLE: the spec never defines iterating one, and section 2.1.2
//     already gives "in" over a string a different meaning, a substring test,
//     so "[c for c in 'abc']" is a "cannot be iterated" error rather than a
//     list of characters. A UNION iterable follows the same every-member rule
//     a subscript and a slice already apply to a union receiver: it is
//     iterable only when EVERY member is, with the members' element types
//     unified per section 1.2.6. That keeps "range_expr | list[int]" — the
//     union this package manufactures itself for a slice of a range_expr whose
//     length is not yet known — iterable as int, while "list[int] | nulltype"
//     and "range_expr | list[string]" are both rejected rather than silently
//     typed as their list member. An EMPTY iterable produces "[]" with the
//     element expression never evaluated, and therefore never type-checked:
//     "[Bogus for i in Param.Empty]" is "[]", not an unknown-symbol error.
//     That is deliberate rather than a gap — the spec requires no static check
//     of a body that never runs, and the reference implementation answers
//     identically. Rejected outright, each with its own parser error:
//     a second "for" clause and a second "if" filter (Python's
//     multi-generator and multi-filter forms) and a generator expression
//     ("(x for x in y)"). A dict or set comprehension ("{k: k for k in y}",
//     "{x for x in y}") is rejected earlier still, IN THE LEXER — "{" is not
//     a token this grammar defines at all, so there is no parser production
//     to reject it in; it never reaches one. The comprehension is a SECOND
//     way to do unbounded total work in a single expression, alongside B2's
//     nested-repetition growth: nesting one comprehension inside another's
//     element expression multiplies the inner work by the outer iteration
//     count at every level, the same shape of growth "[[0]*10000000]*10"
//     already had. checkElementCount (limits.go's maxElements) bounds one
//     comprehension's own RESULT, not the cumulative work of producing every
//     nested level along the way — the configurable limits that would
//     (sections 1.3.9 and 1.3.10) are still sub-project E's, unchanged from
//     every earlier wave.
//
//   - Function, method and property calls now PARSE AND RESOLVE —
//     "len([1])", "[1,2].upper()" and "Param.Name.stem" no longer fail as
//     unsupported syntax — and the registry they resolve through,
//     functionShapes (funcs.go, assembled by its own mergeFuncs), now holds
//     80 entries: sub-project C1's four groups — general conversions (len,
//     bool, string, int, float, list, range_expr — funcsconv.go), validation
//     (fail, the same file, its own RFC 0006 category despite living beside
//     the conversions in one Go source file), math (abs, min, max, sum,
//     floor, ceil, round — funcsmath.go), and list functions (range, flatten,
//     sorted, reversed, unique, any, all — funcslist.go) — sub-project C2's
//     string library, across four files: case transforms and classification
//     (upper, lower, capitalize, title, isdigit, isalpha, isalnum, isspace,
//     isupper, islower, isascii — funcsstrcase.go), trim, affix and
//     search/replace (strip, lstrip, rstrip, removeprefix, removesuffix,
//     startswith, endswith, count, find, rfind, index, rindex, replace —
//     funcsstrfind.go), split, rsplit and join (funcsstrsplit.go) and padding
//     (ljust, rjust, center, zfill — funcsstrpad.go) — sub-project C3's two
//     hard-grammar families across four more files:
//     regular expressions (re_match, re_search, re_findall, re_sub,
//     re_escape, re_split — funcsre.go, behind its own translated dialect in
//     repattern.go — see the regex bullet below) and repr_* (repr_sh,
//     repr_cmd, repr_pwsh — funcsreprshell.go — and repr_py, repr_json —
//     funcsreprdata.go — see the repr_* bullet below) — and sub-project
//     C4's path engine, all of it in funcspath.go over the
//     three-flavor pure-path engine in pathval.go: construction and predicates
//     (path, as_posix, is_absolute), the six properties, which are registered
//     under their __property_*__ spellings and so are six of the 80
//     (__property_name__, __property_stem__, __property_suffix__,
//     __property_suffixes__, __property_parent__, __property_parts__), the
//     with_* family and the relative pair (with_name, with_stem, with_suffix,
//     is_relative_to, relative_to), and with_number, whose substitution
//     scanner lives in pathnumber.go. Section 2.1.5's "/" and "+" path
//     operators ship in the same wave but are operator-table rows (ops.go),
//     not registry entries, so they are not among the 80 — see the path
//     bullet below for all of it. The eightieth entry is sub-project D's
//     apply_path_mapping, which is registered from its own pathMappingFuncs
//     group beside the engine it wraps (pathmapping.go) rather than from C4's
//     pathFuncs, because it is the one function in the registry that reads
//     SESSION state — the path-mapping rules WithPathMapping threads through
//     evalCtx — see the path-mapping bullet below.
//
//     WITH IT, SECTION 2.2's ROUGHLY 100-FUNCTION LIBRARY IS ENTIRELY
//     REGISTERED: no library name is left unregistered, in any sub-project.
//     An unknown call still fails at RUNTIME with "unknown function", a real
//     diagnostic rather than a parse error — "no_such_function('/mnt/share')"
//     reports `unknown function "no_such_function"`, not a syntax error,
//     verified directly, exactly as every library name did before C1.
//
//     THAT WORKED EXAMPLE DELIBERATELY NAMES SOMETHING THAT IS NOT, AND WILL
//     NEVER BE, A LIBRARY FUNCTION. Every earlier revision pointed it at a
//     real name from a sub-project that had not shipped yet, and every one
//     went stale when that sub-project shipped — FOUR times: re_match until
//     C2, repr_* until C3, with_suffix until C4, and apply_path_mapping until
//     this wave. Each time the example began demonstrating a DIFFERENT
//     mechanism under the same prose: "with_suffix('/foo/bar.txt', '.png')"
//     reports `no signature of "with_suffix" accepts (string, string)`, and
//     "apply_path_mapping('/mnt/share')" now returns the path "/mnt/share"
//     outright (both verified directly) — overload selection and a clean
//     evaluation, not the unknown-function lookup the paragraph is about,
//     which is exactly the misreading a stale example invites. There is no
//     unregistered library name left to re-point it at a fifth time, and it
//     must not be re-pointed at a real one: a name nothing registers is the
//     only spelling of this example that cannot go stale.
//     (Not every call reaches the registry at all: a dunder
//     callee like "__add__(1, 2)" is rejected before lookup with "is a
//     specification naming convention and is not directly callable", and a
//     callee whose whole dotted name resolves to a symbol — "Param.Name()"
//     when Param.Name is bound — fails "Param.Name is not a function".)
//
//     THREE of C2's rulings look like bugs when tried by hand, and are not.
//     RFC 0006 defines none of the three, so each is adjudicated here and
//     recorded in test/oracle/baseline.txt where it diverges from the
//     reference.
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
//     property this package does implement (callFunction's methodStyle), and
//     C1's own registry is now the first place a caller can observe it
//     directly with no test-only function involved: round's single-argument
//     row is declared over TFloat
//     alone, so "round(3)" resolves in FUNCTION position by promoting the int
//     argument to float (returning the int 3, since that row's own return
//     type is int), while "Param.N.round()" with Param.N bound to that same
//     int FAILS — "no signature of \"round\" accepts (int)" — because the
//     receiver never gets the promotion the argument position received,
//     verified directly against both call forms. TestReceiverCoercionRestriction
//     (call_internal_test.go) now targets the SHIPPED startswith from
//     funcsstrfind.go rather than a synthetic function it registers itself —
//     the spec's own worked example is startswith(path, string), which
//     exercises the restriction by COERCION: the receiver must fail to
//     coerce into a (string, string) signature for the restriction to be
//     observable at all. C1 had to register one because it shipped no
//     (string, string) overload for a path receiver to be refused by; C2's
//     string library supplies one, so a path receiver now genuinely fails to
//     match it. One further
//     exception already exists in the shipped registry: [].len() and len([])
//     both resolve and both return 0 (TestLen_EmptyListReceiver) — not
//     because the receiver restriction was relaxed, but because there is
//     nothing for it to restrict. list[T]'s receiver-position binding of the
//     unbound element variable T to nulltype (the empty literal's own type)
//     converts no value at all; section 1.2.4 suppresses a COERCION on the
//     receiver, and binding a type variable is not one. That restriction is applied to a
//     PROPERTY receiver as well (resolve.go's evalProperty passes
//     methodStyle), which is an adjudication and not an inherited detail:
//     section 1.3.3 defines "x.p" as method syntax over "__property_p__", and
//     section 1.2.4 suppresses coercion on the receiver of a method call, so a
//     property's receiver is not coerced either. It is consequential for the
//     path engine's properties (C4's) rather than academic — it decides, for a
//     "__property_stem__(path)", that "Param.S.stem" on a STRING parameter is
//     an error instead of a silent string-to-path conversion. The reference
//     implementation agrees, checked directly: "'/a/b.txt'.stem" reports
//     "'stem' property is not available for string. Available for: path".
//     TestPropertyReceiverCoercionRestriction pins it with a path receiver
//     against a "(string)" property signature registered locally by the test
//     — C1 registers no properties at all — which is the only receiver type
//     where the flag is observable in the first place: every other type that
//     reaches a string parameter does so by a conversion overload selection
//     would refuse anyway, so the test would pass with the restriction
//     switched off. Property syntax (section 1.3.3) is sugar for a call to
//     __property_p__, dispatched through the same registry, so a property
//     access to a name NOTHING has registered fails through the identical
//     unknown-function mechanism — but NOT with the identical message: it is
//     deliberately reworded to "unknown property" (resolve.go's
//     evalProperty) so that a genuine runtime error inside a property that
//     DOES exist is never relabeled as a missing one. "Param.Name.nosuchprop"
//     (Param.Name bound to a string) reports `unknown property "nosuchprop"
//     on string`, not `unknown function "__property_nosuchprop__"`.
//     "Param.Name.stem" on that same string receiver is the OTHER branch, and
//     went stale exactly the way the with_suffix worked example above warned
//     a path-family example would: it named the "unknown property" wording
//     until C4's Task 6 registered __property_stem__ for real, and now
//     verified directly, it instead reports `no signature of
//     "__property_stem__" accepts (string)` — the receiver-restriction
//     message two paragraphs up, not this one, because the property is no
//     longer unknown. A dotted name that reaches a method or property
//     call is split by LONGEST-PREFIX resolution against the caller's own
//     symbol table (resolve.go's
//     resolveName): "Param.Name.upper()" tries "Param.Name.upper" as a bound
//     symbol first, then "Param.Name", stopping at the first prefix the
//     caller's table actually binds. This package hardcodes no namespaces of
//     its own — the caller's table is authoritative, exactly as it is for the
//     comprehension shadowing check above — so a caller that binds both
//     "a.b" and "a.b.c" gets "a.b.c" resolved to the SYMBOL rather than to a
//     property "c" of "a.b". Nothing of the ~100-function library is left
//     unregistered, sub-project D's apply_path_mapping having been the last
//     of it; the type-variable codes (CodeVarT and friends) and the
//     matcher's binding of them already live here in shape.go, and C1's own
//     list[varT] rows — len(), bool(), string()'s list row, and flatten(),
//     sorted(), reversed() and unique() from the list-function group — are
//     the first functions to bind that variable for a genuinely polymorphic
//     signature.
//
//     FIVE of C3's rulings look like bugs when tried by hand, and are not.
//     RFC 0006 defines none of them precisely enough to settle by reading
//     alone, so each was verified directly against Go's regexp package,
//     Python's re module and the reference implementation, and is recorded
//     here and in test/oracle/baseline.txt where it diverges from the
//     reference.
//
//     The accepted regex dialect is NARROWER than the reference's. RFC 0006
//     defines the dialect as "the intersection of Python's re module and
//     Rust's regex crate", and sqi enforces that STATED RULE rather than
//     merely the spec's own explicit list of rejected constructs, so
//     re_search('3', r'\p{Nd}'), re_search('ab', r'(?<n>a)b') and
//     re_search('a', r'[[:alpha:]]') are all rejected — verified directly —
//     even though the reference accepts all three. Checked against real
//     Python's re module directly, not just against the spec text: it
//     rejects \p{Nd} ("bad escape \p") and (?<n>a) ("unknown extension ?<n")
//     outright, matching sqi, but it does not reject [[:alpha:]] at all — it
//     reads the whole bracket expression LITERALLY, as a class holding the
//     characters '[', ':', 'a', 'l', 'p', 'h' followed by a literal ']', so
//     "a" alone does not match it and "a]" does. That is a different
//     rejection from sqi's (sqi refuses to compile the pattern at all; Python
//     compiles it into something that matches almost nothing anyone
//     intended), which is exactly why RFC 0006 excludes POSIX classes from
//     the intersection rather than leaving them to either engine's own
//     reading.
//
//     \W and \S inside a NEGATED character class are rejected — a STATED
//     COMPLIANCE GAP, not an oversight: re_search('a!', r'[^\Wa]') errors
//     with "\W inside a negated character class needs set subtraction, which
//     Go's engine cannot express", verified directly, because "not a word
//     character, minus 'a'" is set subtraction and RE2 (which backs Go's
//     regexp, and the reference's own engine) has no operator for it — the
//     reference itself does not reject this pattern, but silently returns
//     null (no match) for every input, which is not a well-formed reading of
//     the dialect either; see test/oracle/baseline.txt. Inside a POSITIVE
//     class the same shorthand works, by REWRITING the class into an
//     alternation rather than emitting it in place — re_search('a!',
//     r'[\Wa]') matches, verified directly — and a class may hold any number
//     of \W/\S occurrences this way, each distinct shorthand becoming its own
//     alternation branch (scanClass, classNeedsAlternation).
//
//     \d, \w and \s are UNICODE, not Go's ASCII-only defaults — every pattern
//     is translated by translatePattern (repattern.go) before it ever reaches
//     Go's engine, and that translation is what makes them Unicode: verified
//     directly, re_search('٣', '\d') matches the Arabic-Indic digit ٣, which
//     Go's own \d does not.
//
//     repr_sh and repr_py follow the PYTHON FUNCTIONS RFC 0006 names them
//     after — shlex.quote and repr respectively — which produce different
//     TEXT from the reference implementation for the same input, verified
//     directly: repr_sh("it's") is 'it'"'"'s' here (shlex.quote's
//     close-quote/literal-quote/reopen-quote splice) and "it's" there (a
//     different, but shell-safe, quoting strategy); repr_py("it's") is
//     "it's" here (Python repr()'s own quote-switching rule: prefer a single
//     quote, switch to double when the string holds a single quote and no
//     double) and 'it\'s' there (always single-quote, escape instead of
//     switching). repr_sh's difference is TEXTUAL ONLY — both forms are
//     shell-equivalent and safe, unlike a hypothetical quoting bug, which is
//     why this file lives apart from the serialization functions next door
//     (funcsreprshell.go's own comment). Both divergences are baselined in
//     test/oracle/baseline.txt as the reference's own bug: RFC 0006 names the
//     Python function, and sqi's output is what that function actually
//     produces.
//
//     repr_json does NOT share writeJSONValue, string()'s list-rendering
//     helper (funcsconv.go) — string(list) and repr_json genuinely differ on
//     non-ASCII input, in the reference too, verified directly against both:
//     string(['café']) is ["café"] (non-ASCII left literal, matching the
//     reference exactly) while repr_json('café') is "caf\u00e9" (ASCII-only
//     output with a generic four-hex-digit escape, matching Python's
//     json.dumps default ensure_ascii behavior, which the reference also
//     matches). RFC 0006 calls string()'s list row "the JSON string
//     representation" with no ensure_ascii qualification and separately names
//     repr_json after json.dumps, whose DEFAULT is ensure_ascii=True — two
//     different specified behaviors landing in two different functions on
//     purpose, not a missed opportunity to share code.
//
//   - flatten()'s row ORDER is load-bearing — listFuncs (funcslist.go) is the
//     first table in the package where it is. "flatten([[1],[2]])" matches
//     BOTH its nested row (list[list[T]] -> list[T]) and its flat row
//     (list[T] -> list[T]) at cost 0, since [[1],[2]] is simultaneously a
//     list[list[int]] (T = int) and a list[list[int]] read as list[T] with
//     T = list[int]; matchShapesExactFirst breaks an exact-cost tie to the
//     EARLIEST registered shape, not by any property of the argument. The
//     nested row is listed first for exactly that reason: reversing the two
//     would make flatten() the identity on every argument, silently, with no
//     type error to catch it.
//
//   - path and range_expr exist as types — TPath and TRangeExpr participate
//     in Type, coercion and cross-type equality, and a declared parameter can
//     be typed as either. path has no literal syntax of its own, but a value
//     IS produced by evaluation: string -> path is section 1.2.3's own
//     coercion rule, so Eval(src, syms, expr.TPath) returns a path value for
//     any expression that evaluates to a string. path now has semantics of its
//     own on top of that — its three flavors, its URI awareness, its properties
//     and functions, and section 2.1.5's "/" and "+" — all delivered by
//     sub-project C4 and described in the path-engine bullet below. RFC 0006
//     puts the whole family in the function library, which is why C4 owns it
//     and D does not; D is narrower than that split once suggested:
//     apply_path_mapping, the Param/RawParam split, and mapping into typed
//     parameter values — the first of those has now shipped, and has the
//     path-mapping bullet below to itself. range_expr, by contrast, still has no operator of its
//     own beyond what coercion gives it for free. The
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
//   - range_expr has real values, and sub-project C1's range_expr() function
//     (funcsconv.go) is now the language's own way to construct one — the
//     first since "nothing coerces to it, and nothing constructs it" was
//     written. range_expr(string) parses the spec's <IntRangeExpr> grammar
//     (via internal/openjd/intrange, the same leaf internal/openjd itself
//     uses) and keeps the source text verbatim rather than canonicalizing it;
//     range_expr(list[int]) sorts and de-duplicates its input, then builds
//     the canonical string through canonicalRange (rangeexpr.go) — the same
//     function this package already used internally to derive a range_expr
//     from a SLICE of one (slice.go's sliceRangeExpr). Sub-project E's
//     CHUNK[INT] symbol remains a second,
//     still-unimplemented way to get one. Because a bare expression can now
//     construct a range_expr with no symbol table, it also has DIFFERENTIAL
//     coverage for the first time (make test-expr-oracle, corpus.txt's
//     range_expr(...) cases) — which is what surfaced a canonicalization gap
//     rather than closing the topic outright: range_expr(string) keeps
//     "1-10:2" as entered, while the reference reports the canonical
//     "1-9:2" (the end corrected to the last value the step actually
//     produces), and the same gap resurfaces wherever that string is read
//     back out — string(range_expr(...)) and range_expr as an operand of
//     "+". range_expr(list[int]) itself is NOT part of the gap: canonicalRange
//     agrees with the reference on every list-constructed case in the corpus.
//     All of it is baselined in test/oracle/baseline.txt as a known gap,
//     deferred to sub-project E along with the rest of section 1.3.4's
//     pass-through requirements.
//
//   - THE PATH ENGINE (sub-project C4) is complete: three flavors, every path
//     property and function RFC 0006 defines, and
//     section 2.1.5's "/" and "+" operators. apply_path_mapping was the one
//     name it left out, and sub-project D has since registered it — it has the
//     PATH MAPPING bullet below to itself, because its source matching is
//     deliberately NOT this engine's path equality. A path value here is PURE — no
//     filesystem is touched, nothing is resolved or stat'ed — and it is
//     NORMALIZED BY CONSTRUCTION, because Value.Path re-parses its text. That
//     is what makes RFC 0006 line 767's own guarantee, path(p.parts) == p,
//     hold for every shape, and it is why "/" and "+" re-normalize their
//     results where the reference implementation stores the joined text raw.
//
//     path_format is an EVALUATION option (WithPathFormat, eval.go), not a
//     property of the host, and its default here is POSIX — which differs from
//     the specification's own default of host-native, deliberately. sqi parses
//     templates SERVER-SIDE, so a host-derived default would let one template
//     expand into different tasks depending on which machine submitted it; the
//     spec itself names POSIX as what TEMPLATE scope wants, "to ensure
//     consistent behavior regardless of the submission machine's OS", and
//     template parsing is exactly that scope. PathNative exists and resolves to
//     the running GOOS, but nothing in sqi selects it yet — sub-project E does,
//     for host contexts. Verified with no option at all: path('C:/a') renders
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
//     "+" NOW RETURNS A PATH where it used to return a string. Before this
//     wave a path operand simply coerced to string and the (string, string)
//     row answered; RFC 0006's section 2.1.5 table declares __add__(path, string)
//     with a path return, so "path('/a/b') + 'c'" is the path "/a/bc",
//     verified. path + path reaches that same row through section 1.2.3's
//     path -> string coercion at cost 1, beating the (string, string) row at
//     cost 2, so it is typed path too — the reference types it string, and the
//     divergence is baselined. A caller that wanted the old string result must
//     ask for one (a string target, or string()).
//
//     THE RULINGS BELOW look like bugs when tried by hand and are not. (The
//     heading used to give a count, and no reading of what follows ever
//     produced it: named rulings and a "smaller divergences" block are two
//     different lists, and the number could only ever be right for one of
//     them.) Each was
//     adjudicated against third_party/openjd-specifications/ and, where it
//     diverges from the reference, recorded case by case in
//     test/oracle/baseline.txt — that file, not this comment, is the place to
//     check an individual row.
//
//     PATH COMPARISON IS BYTE-EXACT AND CASE-SENSITIVE FOR EVERY FLAVOR,
//     WINDOWS INCLUDED. path('C:/A') == path('c:/a') is FALSE here under
//     WithPathFormat(PathWindows); CPython's PureWindowsPath casefolds and
//     answers True (measured). The specification is silent, and the reference
//     agrees with sqi. This was raised twice during the sub-project and kept
//     both times, for the same reason path_format defaults to POSIX: an
//     expression evaluated server-side must not answer differently because of
//     a host convention, and a case-folding equality would make two templates
//     that differ only in the case of a path literal expand into the same
//     tasks on one flavor and different tasks on another. Determinism and
//     host-independence outrank imitating one platform's filesystem.
//
//     as_posix IS FLAVOR-AWARE: it replaces the FLAVOR'S OWN separator with
//     "/", so it is the IDENTITY under POSIX and on a URI, and a "\" -> "/"
//     rewrite only under Windows. path('/renders/shot_a\b.exr').as_posix() is
//     therefore unchanged, verified. The unconditional backslash rewrite it
//     replaced was refuted by this engine's own values rather than by a
//     preference between readings: under POSIX a backslash is an ordinary
//     filename character, so that same path's parts are ["/", "renders",
//     "shot_a\b.exr"] and its name is "shot_a\b.exr", while as_posix answered
//     "/renders/shot_a/b.exr" — a different path with different parts. It also
//     rewrote a URI object key ("s3://bucket/a\b" to the DIFFERENT key
//     "s3://bucket/a/b"), contradicting the "a URI NORMALIZES NOTHING" rule
//     three paragraphs up. CPython settles the direction: PurePath.as_posix()
//     is str(self).replace(self.parser.sep, '/'), and RFC 0006 line 857 names
//     as_posix among the functions that "match Python's pathlib API" — the same
//     clause that settles stem/suffix below. The reference agrees with the OLD
//     behavior and has the identical contradiction (its parts keep the
//     backslash too), so this is a baselined divergence, argued case by case in
//     test/oracle/baseline.txt.
//
//     with_number REPLACES THE LAST MATCH, per RFC 0006 line 822 — "searches
//     the filename stem from the end for these patterns and replaces the last
//     match". A match is one bounded run of ONE placeholder kind (a printf
//     specifier, a '#' run, a digit run), not everything from where a run
//     starts to the end of the stem, so "##a3" holds TWO candidates and only
//     the digits are replaced: with_number('##a3', 7) is "##a7", verified. The
//     reference lets a '#' run swallow the remainder of the stem and answers
//     "07", DESTROYING the literal "a" the caller wrote — a defect against its
//     own specification's wording, not a second reading of it. Do not change
//     sqi to match it; corpus.txt deliberately also carries the rows where the
//     two agree, so an overcorrection fails there rather than trading one set
//     of answers for another silently.
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
//     python3.12 and python3.14 side by side; 3.13 was not installed and was
//     NOT tested, so the boundary is somewhere in (3.12, 3.14] and this
//     comment does not name a single version. RFC 0006 says "matches Python
//     pathlib.PurePath behavior" and links the CURRENT documentation, which is
//     what settles it in favor of the current rule rather than the reference's.
//
//     A TRAILING EMPTY COMPONENT ON THE BASE of relative_to/is_relative_to is
//     CONSUMED. Section 2.1.5 rules that "a trailing slash on the left operand
//     is consumed by the join (matching pathlib behavior)", and the base of
//     these two functions is an operand of exactly that kind, so an
//     operator-written prefix like "s3://renders/" must match the objects
//     under it. Parsing, meanwhile, PRESERVES an empty component verbatim
//     (Expression-Language lines 754-755) — the URI rule above. Those two
//     rules are both right and together they produce a COHERENCE ARTIFACT
//     worth stating outright, because it looks like a bug: path('s3://b//') ==
//     path('s3://b') is FALSE (their parts differ: ["s3://b", "", ""] against
//     ["s3://b"]), and yet each IS relative to the other and relative_to
//     returns "." in BOTH directions — all four verified. Equality compares
//     the parsed values; relative_to consumes a boundary run on the base
//     before comparing. The reference consumes the same run everywhere the
//     receiver has something after the base, and then does not consume it in
//     the self case, which makes its answers internally inconsistent rather
//     than differently ruled.
//
//     THE REFERENCE'S PATH FAMILY IS POSIX-ONLY, so it cannot adjudicate the
//     Windows flavor at all: it accepts a backslash as a replacement name, and
//     never switches on a drive even for a drive-looking receiver like
//     "C:/a/b" (both measured). Every Windows expectation in this engine is
//     therefore pinned by this package's own unit tests against CPython's
//     PureWindowsPath, NOT by make test-expr-oracle, and the corpus carries no
//     Windows-flavor case at all. URI behavior IS oracle-measurable, because a
//     URI is detected under the reference's POSIX format too, and the corpus
//     covers it heavily.
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
//   - PATH MAPPING (sub-project D) ships apply_path_mapping, the last name
//     the RFC 0006 library was missing, over a prefix-mapping engine local to
//     this package (pathmapping.go's mapPath). It is registered FLAT — always
//     resolvable, in every scope — from its own pathMappingFuncs group rather
//     than from C4's pathFuncs, because it is the only function in the
//     registry that reads session state: the rules WithPathMapping (eval.go)
//     threads through evalCtx.
//
//     WITH NO RULES IT PASSES ITS INPUT THROUGH, as a path:
//     apply_path_mapping('/mnt/share') evaluates with no option at all to the
//     path "/mnt/share" (verified directly). That is not a placeholder — it is
//     the specified behavior when nothing matches — and it is what every
//     current caller gets, because nothing in sqi passes WithPathMapping yet.
//     Sub-project E is what will. Rules are tried in order of DECREASING
//     SourcePath length and the first match wins, so the more specific rule
//     wins whatever order the caller listed them in: with "/a" -> "/short"
//     listed BEFORE "/a/b" -> "/long", apply_path_mapping('/a/b/c') is
//     "/long/c" (verified).
//
//     A WINDOWS SOURCE MATCHES CASE-INSENSITIVELY AND
//     SEPARATOR-INSENSITIVELY, which is DELIBERATELY NOT C4's path equality.
//     Everything else in this package compares path components byte-exactly
//     and case-SENSITIVELY on every flavor including Windows (the path bullet
//     above explains why: determinism for a server-side expansion). Source
//     MATCHING is a different operation from path EQUALITY, and the two
//     answers genuinely disagree on the same pair of paths: a WINDOWS rule
//     whose SourcePath is `C:\studio` maps 'c:/STUDIO/project/scene.ma',
//     while path('C:/Studio') == path('c:/studio') is FALSE and
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
//     coerced. Both verified directly. Two calls therefore do not compose
//     directly; ask for a string in between.
//
//     IT HAS NO ORACLE COVERAGE, and cannot get any through the current
//     harness: scripts/expr-oracle.py calls the reference's
//     evaluate_expression(src, target_type=...), whose only two knobs are the
//     source text and the target type, so there is no channel for session
//     mapping rules and none of test/oracle/corpus.txt's cases name this
//     function. Everything above is pinned by unit tests in this package
//     alone, the same position C4's Windows semantics are in.
//
//     SQI NOW CARRIES TWO PATH-MAPPING IMPLEMENTATIONS, and that is by
//     design rather than an oversight left to be found later.
//     internal/worker/pathmap does a strings.ReplaceAll SUBSTRING swap
//     anywhere in a command string; this engine matches a PREFIX on component
//     boundaries and rebuilds the remainder in the destination flavor. They
//     serve different jobs, and this leaf package cannot import that one
//     anyway. Reconciling them is possible future work, not D's.
//
//     HOST-CONTEXT ENFORCEMENT IS NOT HERE. RFC 0006 makes
//     apply_path_mapping valid only in host-context (@fmtstring[host]) scopes
//     and an error anywhere else, and this package has no scope model to
//     enforce that with — sub-project E does. The cost is recorded rather
//     than hidden: registering the function turned
//     EXPR/job_templates/7.3--apply-path-mapping-in-job-name.invalid.yaml and
//     its -in-timeout sibling from passing to failing (conformance 145 ->
//     143), because both were only ever rejected by "unknown function" and
//     not by the placement rule they actually violate. Both are baselined in
//     test/conformance/baseline-expr.txt with that reason, and burn down when
//     E can reject them for the right one.
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
//     pass-through. Sub-project C1 gives Value a NARROWER, related mechanism
//     that must not be confused with it: an optional rendered form (value.go's
//     fs field), set only by round(x, ndigits) for a positive ndigits, because
//     RFC 0006 requires round(3.5, 2) to render "3.50" — a form no float64
//     carries on its own (TestRound_CarryIsRoundOnlyAndDoesNotPropagate). It is
//     NOT section 1.3.4's pass-through: that needs the original submitted
//     string, which only template integration has, so a parsed float parameter
//     still renders through ordinary formatting, digit-for-digit fidelity and
//     all, remains unimplemented, and extending the carry to cover it is
//     sub-project E's, unchanged from sub-project A. The same underlying
//     Value.String gap now shows up for a list too: Value.String renders a
//     list's string elements unquoted ("[a, b]"), while the reference
//     implementation's to_string() JSON-quotes them ("[\"a\", \"b\"]") —
//     confirmed directly against the reference, not just inferred. Sub-project
//     C1's own string() function (funcsconv.go) does NOT share this gap for a
//     list argument: RFC 0006 calls that conversion's list row "the JSON string
//     representation" rather than a diagnostic rendering, so it quotes string
//     elements — string(["a", "b"]) is "[\"a\", \"b\"]" — while Value.String()
//     of that same list stays unquoted. Two renderings, two functions, on
//     purpose (TestString), and only Value.String()'s form is still
//     sub-project E's to fix.
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
// per iteration, exactly as each of a literal's elements is. Everything else
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
//     EXPR/job_templates group is 143/209 passing, 66 fixtures baselined
//     (make test-conformance). It went DOWN in sub-project D, from 145/209
//     with 64 baselined, and that is the one direction this number is allowed
//     to move without being a defect: registering apply_path_mapping made
//     7.3--apply-path-mapping-in-job-name.invalid.yaml and its -in-timeout
//     sibling evaluate cleanly, and both were only ever rejected by "unknown
//     function" rather than by the placement rule they actually violate. Both
//     are baselined until sub-project E can reject them for the right reason —
//     see the PATH MAPPING bullet above, which states the same 145 -> 143 and
//     must keep stating it. D therefore has NO protected-fixtures test of the
//     kind every wave below has, because it is the mirror image of one: those
//     tests exist where registering a family turned accidental
//     "unknown function" passes into real rejections that must not silently
//     become acceptances, whereas here registration turned two accidental
//     passes into acceptances outright, which no assertion can protect and
//     only scope-aware evaluation can fix. The score includes the ten .invalid
//     fixtures TestConformance_B3ProtectedFixtures asserts by name — a
//     construct this comment describes as rejected, such as the comprehension
//     shadowing check or a parser/lexer rejection of a form Python allows,
//     genuinely does reject it — the sixteen TestConformance_C1ProtectedFixtures
//     protects: sub-project C1's own conversion and math functions correctly
//     REJECTING an invalid argument (an empty list or path into bool(), an
//     unrecognized string into bool(), an empty list to min/max, an
//     unrepresentable float-to-int, NaN and infinity into float(), an empty
//     string or list into range_expr()) — the seven
//     TestConformance_C2ProtectedFixtures protects: sub-project C2's own
//     string functions correctly REJECTING an invalid argument (an empty
//     substring into count/find/replace, an empty separator into
//     split/rsplit, a missing substring into index/rindex), now that
//     registering the 31 string functions removed the accidental
//     "unknown function" rejection those same fixtures used to pass under —
//     each must be rejected by REAL argument validation instead, so that
//     swap (one starting to pass for the wrong reason while another silently
//     regresses) cannot hide inside an unchanged aggregate score either — and
//     the sixteen TestConformance_C3ProtectedFixtures protects: sub-project
//     C3's own regex scanner and re_sub replacement check correctly REJECTING
//     an unsupported construct (an empty pattern, \z, lookahead, lookbehind, a
//     named backreference, and re_sub's four group-reference spellings) for
//     the RIGHT reason now — the pattern scanner or the replacement check,
//     not "unknown function" — rather than merely failing to parse as before
//     C3 — and the four TestConformance_C4ProtectedFixtures protects, all
//     .invalid and all of which passed before C4 for the wrong reason
//     ("unknown function", because path() itself was unregistered):
//     relative_to's own errNotRelative, with_number's own errPaddingTooWide,
//     section 1.2.4's receiver restriction refusing to coerce a path receiver
//     into a string method, and C1's bool() path row. Note what that test can
//     and cannot promise — it asserts the fixture is still REJECTED, not which
//     mechanism rejected it, so an entry pins a specific check only where no
//     second mechanism rejects the same input; its own docstring says so, and
//     all four were verified to have none. C3 also CLEARED one fixture
//     outright, 3.6--let-bindings.yaml, which needed only functions already
//     registered by that point (len, sum, string, repr_py). Its sibling
//     3.6--let-host-context-symbols.yaml is STILL baselined, but no longer for
//     the reason an earlier revision of this comment gave: it does not fail
//     for want of the "/" path operator any more (C4 shipped that), it fails
//     because this scoring path binds a "let" name untyped, so
//     "repr_sh(out)" reports `no signature of "repr_sh" accepts (unresolved)`
//     — measured. That is the scope-blindness class the baseline file's own
//     header describes, and it burns down in sub-project E, not here. The
//     differential oracle test has 891/1052 cases agreeing with the reference
//     implementation, 161 baselined divergences (make test-expr-oracle) — up
//     from B3's 135/169, then C1's 279/321 now that C1's 22 functions had
//     their own corpus cases, C2's 407/469 once its 31 string functions had
//     theirs, C3's 473/551 with its regex and repr_* functions, and C4's
//     path family (test/oracle/corpus.txt), which is where most of the growth
//     in BOTH numbers comes from. Sub-project D moved neither number and is
//     the first wave that could not: the corpus contains no apply_path_mapping
//     case and cannot contain one, because the harness passes the reference
//     only a source string and a target type, with no channel for session
//     mapping rules — see the PATH MAPPING bullet above. The path corpus is
//     large and its divergences are the reference's, argued family by family
//     in the C4 section of test/oracle/baseline.txt (its non-normalizing "/"
//     and "+", its
//     flavor-blind as_posix, its
//     pre-current-pathlib stem/suffix split, its with_number hash-swallow, its
//     inconsistent trailing-slash consumption on a relative_to base, and its
//     treatment of a bare "scheme://" as a wildcard authority). The baselined
//     entries include the range_expr(string) canonicalization gap described
//     above and one round() case: sqi returns int for round(x, ndigits) when
//     ndigits <= 0, per RFC 0006's own signature table, while the reference
//     returns a FLOAT — round(1234.5, -1) is "1230 : int" here and "1230.0 :
//     float" there. The specification outranks the reference (it is Beta,
//     0.x, breaking changes permitted in minor bumps), so this is adjudicated
//     as the reference's bug and recorded in test/oracle/baseline.txt, not
//     fixed here to match it. Also included are the list-of-strings rendering
//     gap (Value.String, not string()) recurring through flatten, sorted and
//     unique, split and rsplit, C3's own regex functions (re_search, re_match,
//     re_findall, re_split all return list[string] or list[list[string]]) and
//     now C4's list-valued path properties (parts, suffixes) — the corpus
//     wraps those in join() wherever it means to compare the VALUES rather
//     than that rendering; two of C2's own rulings argued above — isalnum's
//     composition (isalnum('٣')) and title's ASCII-only word boundary
//     (title('²x y')) — while capitalize's ligature expansion is NOT
//     baselined, since the reference agrees with it outright; and three of
//     C3's own rulings argued above — the intersection-rule refusals
//     (\p{Nd}, (?<n>a), [[:alpha:]]), the [^\W...] set-subtraction gap, and
//     repr_sh/repr_py's textual differences from the reference — while
//     \d/\w/\s's Unicode semantics are NOT baselined, since they are exactly
//     what the oracle is checking agreement on, and repr_json's non-ASCII
//     escaping is NOT baselined either, since the reference matches it
//     exactly. C4 adds the same shape: its rulings above are baselined where
//     they diverge, while case-SENSITIVE path comparison is NOT — the
//     reference agrees with sqi there, and it is CPython's PureWindowsPath
//     that disagrees with both, which the oracle cannot see at all because the
//     reference's path family is POSIX-only. Most baselined divergences are the reference's own bugs,
//     adjudicated against the spec text and recorded one by one in
//     test/oracle/baseline.txt — that file, not this comment, is the place to
//     check any individual ruling.
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
