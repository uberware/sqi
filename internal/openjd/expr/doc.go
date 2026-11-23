// SPDX-License-Identifier: AGPL-3.0-or-later

// Package expr reads and evaluates the OpenJD EXPR extension's expression
// language.
//
// The package is a leaf with respect to internal/openjd: its only
// intra-openjd import is internal/openjd/intrange, a leaf shared between the
// two (see the intrange bullet below). internal/openjd and
// internal/worker/fmtres import expr, so expr can be tested with no template
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
// table; section 1.2.1 itself lists twelve rows and omits any, noreturn and
// the type variables), automatic coercion between them (section 1.2.3), and
// static type checking against placeholders for values that do not exist yet
// (section 1.3.1's unresolved[T]). "Param.Frame + 1" type-checks to
// unresolved[int] before any parameter has a value, and "Param.Name + 5" is
// rejected as a type error the same way, with Param.Name declared but still
// unbound. Every literal form, list literals included, dotted names and the
// full ten-level operator grammar are implemented on top of that type system.
// Several behaviors look like bugs when tried by hand and are intended:
//
//   - "1 + 2.5" evaluates to 3.5, and "1 < 2.5" to true: section 2.1.1's
//     int-to-float promotion is implemented, and an operator is selected by
//     trying an exact-type match first and a promoting one second. Promotion
//     only uses a conversion that cannot fail on any value (int-to-float,
//     path-to-string, range_expr-to-string, range_expr-to-list[int]), never
//     one that can, such as string-to-int. That is why "'a' + 1" is an error
//     rather than "a1": section 1.2.3's single-scalar catch-all conversion
//     applies when a context (a target type, a coercion) demands a specific
//     type and is prepared for the value not to fit, not when the language
//     is choosing which operator overload to run. "true + true" is an error
//     for the same reason: no shape accepts (bool, bool), and bool has no
//     promoting route into one that does.
//
//   - Equality and ordering differ in how far they reach across types.
//     Section 1.2.5 defines equality for every pair of types, so "5 == 5.0"
//     is true and "'5' == 5" is false; equality never fails. A list or
//     range_expr value follows the same cross-type rule: "[1, 2] == [1.0,
//     2.0]" is true, and a range_expr compares equal to the list of integers
//     it expands to. Section 2.1.4 permits ordering to cross only two named
//     compatible pairs, int/float and string/path; every other cross-type
//     comparison is an error, so "5 < 'a'" fails with "unsupported operand
//     types" while "5 == 'a'" evaluates to false. List ordering
//     (lexicographic, section 1.2.5) reaches across element types by those
//     same two pairs, applied elementwise: "[1] < [1.0]" is false, and so is
//     "[[1]] < [[1.0]]" one level down, while "['a'] < [1]" stays an
//     "unsupported operand types" error because string/int is not one of the
//     pairs. That composes section 1.2.5 (list ordering is elementwise and
//     says nothing about the elements' types) with section 2.1.4 (an ordering
//     operator's operands "may differ for compatible pairs"). The unification
//     happens during shape matching (shape.go's orderingUnified), so both
//     operands are coerced to the common element type before any element is
//     compared. Ordering also reaches the empty list, in either operand
//     position and at any nesting depth: section 1.2.6 rule 6 makes
//     list[nulltype] convertible to list[T] for any T, so "[] < [1]" is true,
//     "[1] < []" is false, and "[[]] < [[1]]" and "[[1]] < [[]]" answer the
//     same way one level down. The empty literal's binding of the shared
//     element-type variable is provisional (shape.go's emptyListBinding), and
//     the exception applies in both directions (an empty binding meeting a
//     real argument, and a real binding meeting an empty argument), which is
//     what makes the orders agree.
//
//   - List literals, subscripts and slices are implemented, and list[T] has
//     real values. A literal with no target type infers its element type
//     from its own elements per section 1.2.6's unification rules; a list[T]
//     target instead coerces every element to T directly. "x[0]" and
//     "x[1:3]" work on a list, a string or a range_expr (sections 2.1.7 and
//     2.1.8); a subscript is bounds-checked and errors out of range, while a
//     slice clamps like Python. Both also work on a union of those, which
//     this package produces itself: slicing a range_expr whose length is not
//     yet known is typed "range_expr | list[int]", and section 1.3.1's
//     unknown-condition rule types a conditional as the union of both
//     branches. Every member is indexed and the results combined, so
//     "Param.Range[:][0]" is an int and "([1, 2] if Param.Flag else
//     [3.0])[0]" a float; the operation is rejected only when some member
//     cannot be indexed. list[T] also has its own operators: concatenation
//     ("+"), repetition ("*" by an int, section 2.1.3) and membership
//     ("in"/"not in"), plus list comprehensions (section 1.3.7), described in
//     the next bullet.
//
//   - List comprehensions (section 1.3.7) parse and evaluate:
//     "[x for x in [1, 2, 3] if x > 1]". The loop variable follows the
//     section's shadowing rule ("a loop variable that shadows an existing
//     binding is an error"), checked against the caller's symbol table
//     (comp.go's evalListComp), on top of the parser's <UserIdentifier>
//     casing rule that already keeps a loop variable from colliding with a
//     spec-defined symbol like Param. This package carries no scope
//     information of its own, so the shadowing check is only as precise as
//     the table it is handed: a caller that binds a name not in scope at this
//     expression produces a false rejection. internal/openjd's
//     checkTemplateExpressions hands it a per-position table built from
//     section 3.6.2's scope model, so a step's own "let" is visible in its
//     own script but not in a sibling step's
//     (internal/openjd/exprcheck_let_test.go). The iterable and the filter
//     are both parsed at the or level (parser.go's parseOr), not as the full
//     <ConditionalExpr> section 1.1.2's grammar names for them, because that
//     grammar is ambiguous there: parseConditional consumes a bare "if"
//     expecting an "else", so "[x for x in y if c]" would fail looking for
//     one. A conditional in either position therefore needs parentheses
//     ("[x for x in (a if b else c)]"), which is how Python resolves the same
//     ambiguity (comp_for and comp_if both take an or_test). A string is not
//     iterable: the spec never defines iterating one, and section 2.1.2 gives
//     "in" over a string a different meaning, a substring test, so "[c for c
//     in 'abc']" is a "cannot be iterated" error. A union iterable follows
//     the rule a subscript and a slice apply to a union receiver: it is
//     iterable only when every member is, with the members' element types
//     unified per section 1.2.6. So "range_expr | list[int]" is iterable as
//     int, while "list[int] | nulltype" and "range_expr | list[string]" are
//     rejected. An empty iterable produces "[]" with the element expression
//     never evaluated, and therefore never type-checked: "[Bogus for i in
//     Param.Empty]" is "[]", not an unknown-symbol error. The spec requires
//     no static check of a body that never runs, and the reference
//     implementation answers the same way. Each rejected with its own parser
//     error: a second "for" clause and a second "if" filter (Python's
//     multi-generator and multi-filter forms) and a generator expression
//     ("(x for x in y)"). A dict or set comprehension ("{k: k for k in y}",
//     "{x for x in y}") is rejected in the lexer: "{" is not a token this
//     grammar defines. Nesting one comprehension inside another's element
//     expression multiplies the inner work by the outer iteration count at
//     every level, the same growth "[[0]*10000000]*10" has.
//     checkElementCount (limits.go's maxElements) bounds one comprehension's
//     result, not the cumulative work of producing every nested level;
//     section 1.3.10's operation limit bounds that: runComp (comp.go) charges
//     rule 2 for every element the comprehension iterates, at every nesting
//     level, so the multiplied work spends against the same operation budget
//     as the rest of the expression and the limit fires before it runs away
//     (TestOperationLimit_CatchesANestedComprehension). See the bounded
//     evaluation bullet below for the limits themselves.
//
//   - Function, method and property calls parse and resolve ("len([1])",
//     "[1,2].upper()" and "Param.Name.stem") through the registry
//     functionShapes (funcs.go, assembled by mergeFuncs), which holds 80
//     entries: general conversions (len, bool, string, int, float, list,
//     range_expr; funcsconv.go); validation (fail, in the same file but its
//     own RFC 0006 category); math (abs, min, max, sum, floor, ceil, round;
//     funcsmath.go); list functions (range, flatten, sorted, reversed,
//     unique, any, all; funcslist.go); the string library, across four files:
//     case transforms and classification (upper, lower, capitalize, title,
//     isdigit, isalpha, isalnum, isspace, isupper, islower, isascii;
//     funcsstrcase.go), trim, affix and search/replace (strip, lstrip,
//     rstrip, removeprefix, removesuffix, startswith, endswith, count, find,
//     rfind, index, rindex, replace; funcsstrfind.go), split, rsplit and join
//     (funcsstrsplit.go) and padding (ljust, rjust, center, zfill;
//     funcsstrpad.go); regular expressions (re_match, re_search, re_findall,
//     re_sub, re_escape, re_split; funcsre.go, over its own translated
//     dialect in repattern.go, see the regex rulings below); repr_* (repr_sh,
//     repr_cmd, repr_pwsh in funcsreprshell.go, and repr_py, repr_json in
//     funcsreprdata.go, see the repr_* rulings below); the path engine, all
//     of it in funcspath.go over the three-flavor pure-path engine in
//     pathval.go: construction and predicates (path, as_posix, is_absolute),
//     the six properties, registered under their __property_*__ spellings and
//     so six of the 80 (__property_name__, __property_stem__,
//     __property_suffix__, __property_suffixes__, __property_parent__,
//     __property_parts__), the with_* family and the relative pair
//     (with_name, with_stem, with_suffix, is_relative_to, relative_to), and
//     with_number, whose substitution scanner lives in pathnumber.go; and
//     apply_path_mapping, registered from its own pathMappingFuncs group
//     beside the engine it wraps (pathmapping.go) rather than from pathFuncs,
//     because it is the one function in the registry that reads session
//     state, the path-mapping rules WithPathMapping threads through evalCtx
//     (see the path mapping bullet below). Section 2.1.5's "/" and "+" path
//     operators are operator-table rows (ops.go), not registry entries, so
//     they are not among the 80.
//
//     Section 2.2's library of roughly 100 functions is entirely registered.
//     An unknown call fails at runtime with "unknown function", not a parse
//     error: "no_such_function('/mnt/share')" reports `unknown function
//     "no_such_function"`. (The example names something that is not a
//     library function; a real library name would demonstrate a different
//     mechanism. "with_suffix('/foo/bar.txt', '.png')" reports `no signature
//     of "with_suffix" accepts (string, string)` (overload selection), and
//     "apply_path_mapping('/mnt/share')" returns the path "/mnt/share".) Not
//     every call reaches the registry: a dunder callee like "__add__(1, 2)"
//     is rejected before lookup with "is a specification naming convention
//     and is not directly callable", and a callee whose whole dotted name
//     resolves to a symbol ("Param.Name()" when Param.Name is bound) fails
//     "Param.Name is not a function".
//
//     The six classification predicates other than isascii, and title()
//     and capitalize(), have the semantics of the Python str methods of the
//     same name over the Unicode Character Database (RFC 0006 §2.2.4's two
//     notes). Three consequences are easy to mistake for bugs. isdigit('٣')
//     and isdigit('²') are true (Numeric_Type Decimal or Digit) even though
//     int() accepts neither, so isdigit is not a guard for int(). isalnum is
//     broader than isalpha || isdigit: it admits any Numeric_Type, so
//     isalnum('½') is true while isalpha('½') and isdigit('½') are both
//     false. And title() starts a word after any uncased character, so
//     title('a1b c') is "A1B C" and title('1st') is "1St", and both title()
//     and capitalize() titlecase rather than uppercase a word's first
//     character: capitalize('ﬁne day') is "Fine day". The Go standard
//     library has no Numeric_Type or Case_Ignorable table; funcsstrcase.go
//     carries the data it needs. Results can differ from CPython 3.14 on code
//     points whose Unicode 16 data postdates Go's Unicode 15 tables, which the
//     specification permits.
//
//     Uniform function call syntax (section 1.3.3) makes "x.f(a)" resolve
//     through the same callFunction as "f(x, a)", with the receiver
//     prepended to the argument list, except that section 1.2.4 suppresses
//     implicit coercion on a method receiver (callFunction's methodStyle).
//     The registry shows it: round's single-argument row is declared over
//     TFloat alone, so "round(3)" resolves in function position by promoting
//     the int argument to float (returning the int 3, since that row returns
//     int), while "Param.N.round()" with Param.N bound to the same int fails
//     with "no signature of \"round\" accepts (int)", because the receiver
//     never gets the promotion the argument position receives.
//     TestReceiverCoercionRestriction (call_internal_test.go) uses the spec's
//     own worked example, startswith(path, string), against startswith in
//     funcsstrfind.go: a path receiver must fail to coerce into its (string,
//     string) signature. [].len() and len([]) both resolve and both return 0
//     (TestLen_EmptyListReceiver) because there is nothing for the
//     restriction to restrict: binding list[T]'s unbound element variable T
//     to nulltype (the empty literal's own type) converts no value, and
//     section 1.2.4 suppresses a coercion on the receiver, which binding a
//     type variable is not. The restriction applies to a property receiver as
//     well (resolve.go's evalProperty passes methodStyle): section 1.3.3
//     defines "x.p" as method syntax over "__property_p__", and section 1.2.4
//     suppresses coercion on the receiver of a method call, so a property's
//     receiver is not coerced either. For the path engine's properties this
//     makes "Param.S.stem" on a string parameter an error instead of a silent
//     string-to-path conversion; the reference implementation agrees
//     ("'/a/b.txt'.stem" reports "'stem' property is not available for
//     string. Available for: path"). TestPropertyReceiverCoercionRestriction
//     pins it with a path receiver against a "(string)" property signature
//     registered locally by the test, the only receiver type where the flag
//     is observable: every other type that reaches a string parameter does so
//     by a conversion overload selection would refuse anyway. Property syntax
//     is sugar for a call to __property_p__, dispatched through the same
//     registry, so a property access to a name nothing has registered fails
//     through the same unknown-function mechanism, reworded to "unknown
//     property" (resolve.go's evalProperty) so that a runtime error inside a
//     property that does exist is never relabeled as a missing one.
//     "Param.Name.nosuchprop" (Param.Name bound to a string) reports `unknown
//     property "nosuchprop" on string`, not `unknown function
//     "__property_nosuchprop__"`. "Param.Name.stem" on that same string
//     receiver takes the other branch: __property_stem__ exists, so it
//     reports `no signature of "__property_stem__" accepts (string)`, the
//     receiver-restriction message. A dotted name that reaches a method or
//     property call is split by longest-prefix resolution against the
//     caller's symbol table (resolve.go's resolveName): "Param.Name.upper()"
//     tries "Param.Name.upper" as a bound symbol first, then "Param.Name",
//     stopping at the first prefix the caller's table binds. This package
//     hardcodes no namespaces of its own (the caller's table is
//     authoritative, as it is for the comprehension shadowing check above),
//     so a caller that binds both "a.b" and "a.b.c" gets "a.b.c" resolved to
//     the symbol rather than to a property "c" of "a.b". The type-variable
//     codes (CodeVarT and friends) and the matcher's binding of them live in
//     shape.go; the list[varT] rows (len(), bool(), string()'s list row, and
//     flatten(), sorted(), reversed() and unique() from the list-function
//     group) bind that variable for polymorphic signatures.
//
//     Five regular-expression and repr_* rulings look like bugs when tried by
//     hand and are intended. RFC 0006 defines none of them precisely enough
//     to settle by reading alone; each is compared with Go's regexp package,
//     Python's re module and the reference implementation, and recorded in
//     test/oracle/baseline.txt where it diverges from the reference.
//
//     The accepted regex dialect is narrower than the reference's. RFC 0006
//     defines the dialect as "the intersection of Python's re module and
//     Rust's regex crate", and sqi enforces that stated rule rather than
//     only the spec's explicit list of rejected constructs, so
//     re_search('3', r'\p{Nd}'), re_search('ab', r'(?<n>a)b') and
//     re_search('a', r'[[:alpha:]]') are all rejected, although the reference
//     accepts all three. Python's re module rejects \p{Nd} ("bad escape \p")
//     and (?<n>a) ("unknown extension ?<n") outright, as sqi does, but it
//     does not reject [[:alpha:]]: it reads the bracket expression literally,
//     as a class holding '[', ':', 'a', 'l', 'p', 'h' followed by a literal
//     ']', so "a" alone does not match it and "a]" does. sqi refuses to
//     compile the pattern; Python compiles it into something that matches
//     almost nothing anyone intended. That disagreement between the two
//     engines is why RFC 0006 excludes POSIX classes from the intersection.
//
//     \W and \S inside a negated character class are rejected, a stated
//     compliance gap: re_search('a!', r'[^\Wa]') errors with "\W inside a
//     negated character class needs set subtraction, which Go's engine
//     cannot express", because "not a word character, minus 'a'" is set
//     subtraction and RE2 (which backs Go's regexp, and the reference's own
//     engine) has no operator for it. The reference does not reject this
//     pattern but returns null (no match) for every input, which is not a
//     well-formed reading of the dialect either; see test/oracle/baseline.txt.
//     Inside a positive class the same shorthand works, by rewriting the
//     class into an alternation rather than emitting it in place
//     (re_search('a!', r'[\Wa]') matches), and a class may hold any number of
//     \W/\S occurrences this way, each distinct shorthand becoming its own
//     alternation branch (scanClass, classNeedsAlternation).
//
//     \d, \w and \s are Unicode-aware, unlike Go's ASCII-only defaults: every
//     pattern is translated by translatePattern (repattern.go) before it
//     reaches Go's engine, and that translation is what makes them Unicode.
//     re_search('٣', '\d') matches the Arabic-Indic digit ٣, which Go's own
//     \d does not.
//
//     repr_sh and repr_py follow the Python functions RFC 0006 names them
//     after, shlex.quote and repr, which produce different text from the
//     reference implementation for the same input: repr_sh("it's") is
//     'it'"'"'s' here (shlex.quote's close-quote/literal-quote/reopen-quote
//     splice) and "it's" there (a different, shell-safe, quoting strategy);
//     repr_py("it's") is "it's" here (Python repr()'s quote-switching rule:
//     prefer a single quote, switch to double when the string holds a single
//     quote and no double) and 'it\'s' there (always single-quote, escape
//     instead of switching). repr_sh's difference is textual only: both forms
//     are shell-equivalent and safe, which is why funcsreprshell.go lives
//     apart from the serialization functions in funcsreprdata.go (see its own
//     comment). Both divergences are baselined in test/oracle/baseline.txt as
//     the reference's bug: RFC 0006 names the Python function, and sqi's
//     output is what that function produces.
//
//     repr_json does not share writeJSONValue, string()'s list-rendering
//     helper (funcsconv.go): string(list) and repr_json differ on non-ASCII
//     input, in the reference too. string(['café']) is ["café"] (non-ASCII
//     left literal, as the reference does) while repr_json('café') is
//     "caf\u00e9" (ASCII-only output with a four-hex-digit escape, Python's
//     json.dumps default ensure_ascii behavior, which the reference also
//     follows). RFC 0006 calls string()'s list row "the JSON string
//     representation" with no ensure_ascii qualification and separately names
//     repr_json after json.dumps, whose default is ensure_ascii=True: two
//     specified behaviors in two functions.
//
//   - flatten()'s row order matters; listFuncs (funcslist.go) is the first
//     table in the package where it does. "flatten([[1],[2]])" matches both
//     its nested row (list[list[T]] -> list[T]) and its flat row (list[T] ->
//     list[T]) at cost 0, since [[1],[2]] is a list[list[int]] (T = int) and
//     also a list[T] with T = list[int]; matchShapesExactFirst breaks an
//     exact-cost tie to the earliest registered shape. The nested row is
//     listed first for that reason: reversing the two would make flatten()
//     the identity on every argument, with no type error to catch it.
//
//   - path and range_expr exist as types: TPath and TRangeExpr participate
//     in Type, coercion and cross-type equality, and a declared parameter can
//     be typed as either. path has no literal syntax of its own, but a value
//     is produced by evaluation: string -> path is section 1.2.3's own
//     coercion rule, so Eval(src, syms, expr.TPath) returns a path value for
//     any expression that evaluates to a string. path has semantics of its
//     own on top of that (its three flavors, its URI awareness, its
//     properties and functions, and section 2.1.5's "/" and "+"), described
//     in the path engine bullet below; apply_path_mapping has the path
//     mapping bullet to itself. range_expr has no operator of its own beyond
//     what coercion gives it. The range_expr -> list[int] conversion (section
//     1.2.3) is implemented (coercing a range_expr value to a list[int]
//     target expands it), and so is section 1.2.5's list/range_expr
//     cross-type equality rule. What coercion gives a range_expr operand is
//     narrow, by ruling: section 1.2.3's range_expr -> string rule reaches a
//     string parameter only for the operators whose own tables name a
//     range_expr row, which is "+" alone. Sections 2.1.2 and 2.1.3 write
//     those rows out explicitly, and would not need to if the coercion fired
//     on its own during overload selection. So "Param.Range + '!'"
//     concatenates as text, while "Param.Range * 2" and "'1' in Param.Range"
//     are "unsupported operand types" errors, not the string repetition
//     "1-101-10" or a substring test over "1-10". The reference
//     implementation rejects both as well.
//
//   - range_expr has real values, and range_expr() (funcsconv.go) constructs
//     one. Its value is its list of values, and its string form is the
//     canonical form of Template Schemas §3.4.1.1.1 (openjd-specifications
//     #202): runs of a constant difference taken greedily from the left, so
//     range_expr("1-10:2") is "1-9:2", range_expr("1,3,5,7") is "1-7:2", and
//     every path that writes one as text (interpolation, string(), "+", the
//     repr_* functions) agrees. When the elements' spans are disjoint,
//     intrange.Canonical computes it from their arithmetic progressions
//     without listing the values, so a billion-value range costs what a
//     three-value one does. Overlapping elements are listed up to
//     canonicalOverlapBound values; past that the text keeps its written form
//     (rangeexpr.go). A CHUNK[INT] task parameter's value also keeps the text
//     it was given (TaskParamType types it range_expr): RFC 0005 §1.2.2 gives
//     a chunk its own string form, "<N>-<M>" for a contiguous chunk even with
//     one or two values, so ValueFromText binds it as written, while a
//     range_expr computed from it (a slice, range_expr(string(...))) is
//     canonical.
//
//   - The path engine implements three flavors, every path property and
//     function RFC 0006 defines, and section 2.1.5's "/" and "+" operators.
//     apply_path_mapping has the path mapping bullet below to itself, because
//     its source matching is not this engine's path equality. A path value
//     here is pure (no filesystem is touched, nothing is resolved or
//     stat'ed) and normalized by construction, because Value.Path re-parses
//     its text. That is what makes RFC 0006 line 767's guarantee, path(p.parts)
//     == p, hold for every shape, and it is why "/" and "+" re-normalize their
//     results where the reference implementation stores the joined text raw.
//
//     path_format is an evaluation option (WithPathFormat, eval.go), not a
//     property of the host, and its default here is POSIX, which differs from
//     the specification's default of host-native. sqi parses templates on the
//     server, so a host-derived default would let one template expand into
//     different tasks depending on which machine submitted it; the spec names
//     POSIX as what template scope wants, "to ensure consistent behavior
//     regardless of the submission machine's OS", and template parsing is
//     that scope. PathNative resolves to the running GOOS;
//     internal/worker/fmtres selects it for host-context evaluation on the
//     worker. With no option, path('C:/a') renders "C:/a" and its
//     is_absolute() is false (a drive letter means nothing to POSIX), while
//     WithPathFormat(PathWindows) gives "C:\a" and true.
//
//     A URI is not a flavor. It is detected from the text under every
//     path_format, by the spec's scheme grammar, and a URI normalizes
//     nothing: consecutive slashes, "." segments and a trailing slash all
//     survive, because a URI path component is an opaque identifier and "a//b"
//     and "a/b" may name different objects in a store.
//     path('s3://bucket/a//b/./c') renders back verbatim under both filesystem
//     flavors, and its parts are ["s3://bucket", "a", "", "b", ".", "c"]: the
//     empty component and the "." are real components, where a POSIX or
//     Windows path would collapse both.
//
//     "+" on a path returns a path. RFC 0006's section 2.1.5 table declares
//     __add__(path, string) with a path return, so "path('/a/b') + 'c'" is
//     the path "/a/bc". path + path reaches that same row through section
//     1.2.3's path -> string coercion at cost 1, beating the (string, string)
//     row at cost 2, so it is typed path too; the reference types it string,
//     and the divergence is baselined. A caller that wants a string result
//     must ask for one (a string target, or string()).
//
//     The rulings below look like bugs when tried by hand and are intended.
//     Each is adjudicated against third_party/openjd-specifications/ and,
//     where it diverges from the reference, recorded case by case in
//     test/oracle/baseline.txt, which is the place to check an individual
//     row.
//
//     Path comparison is byte-exact and case-sensitive for every flavor,
//     Windows included. path('C:/A') == path('c:/a') is false here under
//     WithPathFormat(PathWindows); CPython's PureWindowsPath casefolds and
//     answers True. The specification is silent, and the reference agrees
//     with sqi. The reason is the same as for the POSIX path_format default:
//     an expression evaluated on the server must not answer differently
//     because of a host convention, and a case-folding equality would make
//     two templates that differ only in the case of a path literal expand
//     into the same tasks on one flavor and different tasks on another.
//
//     as_posix is flavor-aware: it replaces the flavor's own separator with
//     "/", so it is the identity under POSIX and on a URI, and a "\" -> "/"
//     rewrite only under Windows. path('/renders/shot_a\b.exr').as_posix() is
//     therefore unchanged. An unconditional backslash rewrite would
//     contradict this engine's own values: under POSIX a backslash is an
//     ordinary filename character, so that path's parts are ["/", "renders",
//     "shot_a\b.exr"] and its name is "shot_a\b.exr", while such a rewrite
//     answers "/renders/shot_a/b.exr", a different path with different parts.
//     It would also rewrite a URI object key ("s3://bucket/a\b" to the
//     different key "s3://bucket/a/b"), contradicting the rule that a URI
//     normalizes nothing. CPython settles the direction:
//     PurePath.as_posix() is str(self).replace(self.parser.sep, '/'), and
//     RFC 0006 line 857 names as_posix among the functions that "match
//     Python's pathlib API", the same clause that settles stem/suffix below.
//     The reference does the unconditional rewrite and has the same
//     contradiction (its parts keep the backslash too), so this is a
//     baselined divergence, argued case by case in test/oracle/baseline.txt.
//
//     with_number replaces the last match, per RFC 0006 line 822: "searches
//     the filename stem from the end for these patterns and replaces the last
//     match". A match is one bounded run of one placeholder kind (a printf
//     specifier, a '#' run, a digit run), not everything from where a run
//     starts to the end of the stem, so "##a3" holds two candidates and only
//     the digits are replaced: with_number('##a3', 7) is "##a7". The
//     reference lets a '#' run swallow the remainder of the stem and answers
//     "07", destroying the literal "a" the caller wrote, which contradicts its
//     own specification's wording. Do not change sqi to match it; corpus.txt
//     also carries the rows where the two agree, so an overcorrection fails
//     there.
//
//     stem, suffix and suffixes follow current CPython pathlib, which strips
//     the whole leading dot run before splitting (and therefore treats a
//     trailing dot as a suffix); the reference implements the older rule,
//     which guarded only a single leading dot. So path('a.').stem is "a" with
//     suffix "." and suffixes ["."], and path('/a/..b').stem is "..b" with no
//     suffix. The same splitStemSuffix backs with_stem, with_suffix and
//     with_number, so the rule reaches those too. RFC 0006 says "matches Python
//     pathlib.PurePath behavior" and links the current documentation, which
//     settles it in favor of the current rule.
//
//     A trailing empty component on the base of relative_to/is_relative_to is
//     consumed. Section 2.1.5 rules that "a trailing slash on the left operand
//     is consumed by the join (matching pathlib behavior)", and the base of
//     these two functions is an operand of that kind, so an operator-written
//     prefix like "s3://renders/" must match the objects under it. Parsing
//     preserves an empty component verbatim (Expression-Language lines
//     754-755), the URI rule above. Together the two rules produce a result
//     that looks like a bug: path('s3://b//') == path('s3://b') is false
//     (their parts differ: ["s3://b", "", ""] against ["s3://b"]), and yet
//     each is relative to the other and relative_to returns "." in both
//     directions. Equality compares the parsed values; relative_to consumes a
//     boundary run on the base before comparing. The reference consumes the
//     same run everywhere the receiver has something after the base, and then
//     does not consume it in the self case, which makes its answers
//     internally inconsistent.
//
//     The oracle evaluates the reference under POSIX only
//     (scripts/expr-oracle.py pins PathFormat.POSIX), so it does not
//     adjudicate the Windows flavor: under POSIX the reference accepts a
//     backslash as a replacement name, and never switches on a drive even for
//     a drive-looking receiver like "C:/a/b". Every Windows expectation in
//     this engine is therefore pinned by this package's own unit tests
//     against CPython's PureWindowsPath, not by make test-expr-oracle, and
//     the corpus carries no Windows-flavor case. URI behavior is covered by
//     the oracle, because a URI is detected under POSIX too.
//
//     Three smaller divergences, each recorded beside the code that produces
//     it:
//
//     The "\\?\UNC\" extended-length prefix is not parsed. pathlib gives that
//     one literal prefix its own start-at-offset-8 parsing, splitting a
//     further server+share out of what follows, so "\\?\UNC\srv\share\x" is
//     one opaque root plus "x" to Python; here it runs through the ordinary
//     offset-2 UNC algorithm and comes out with root "\\?\UNC\" and "srv",
//     "share", "x" as ordinary components. That is a different split, not
//     only a different rendering, and the only extended-length or device
//     shape known to diverge (a plain "\\?\a\b" or "\\.\a\b" agrees with
//     CPython). Reserved device names ("NUL", "CON") are not special-cased
//     either, and neither are they by pathlib's pure paths. See parseWindows.
//
//     path('//srv/share/x') / '//srv/share' anchors here, giving
//     "\\srv\share\", where CPython keeps the parent's "x" and answers
//     "\\srv\share\x". sqi follows section 2.1.5, which says an absolute right
//     operand replaces the left entirely, and "//srv/share" is_absolute() is
//     true in both engines. The mechanical cause is that splitRootWindows
//     also ports pathlib's root-synthesis heuristic, which ntpath.splitroot
//     does not have, so a complete server+share pair carries a root here and
//     the join's anchor arm fires. See pathJoin.
//
//     A URI scheme is at least two characters, per the specification's
//     ^[a-zA-Z][a-zA-Z0-9+.-]+:// , so "C://scenes/a.ma" is a filesystem path
//     (a Windows drive with a doubled separator) under every path_format,
//     never a URI with scheme "c". See splitURI.
//
//   - Path mapping. apply_path_mapping runs over a prefix-mapping engine
//     local to this package (pathmapping.go's mapPath). It is registered
//     flat, resolvable in every scope, from its own pathMappingFuncs group
//     rather than from pathFuncs, because it is the only function in the
//     registry that reads session state: the rules WithPathMapping (eval.go)
//     threads through evalCtx.
//
//     With no rules it passes its input through, normalized as a path value
//     in the evaluation's flavor: apply_path_mapping('/mnt/share') evaluates
//     with no option to the path "/mnt/share". "Passthrough" names the
//     mapping outcome (no rule rewrote the text), not a verbatim string
//     return: the result is always re-parsed by boundedPath in
//     ec.pathFormat, so apply_path_mapping('/a//b/') is the path "/a/b" and
//     apply_path_mapping('/a/b') under WithPathFormat(PathWindows) is "\a\b".
//     That normalization is required: the vendored fixture
//     expr2.3.2--apply-path-mapping's output_windows line for its
//     POSIX_UNMAPPED case expects a slash-shaped input that matched no rule
//     to come back with Windows separators, which happens only because the
//     passthrough is re-parsed. internal/worker/fmtres supplies the session's
//     rules through WithPathMapping. Rules are tried in order of decreasing
//     SourcePath length and the first match wins, so the more specific rule
//     wins whatever order the caller listed them in: with "/a" -> "/short"
//     listed before "/a/b" -> "/long", apply_path_mapping('/a/b/c') is
//     "/long/c".
//
//     A WINDOWS source matches case-insensitively and
//     separator-insensitively, unlike the path engine's equality. Path
//     equality (==) and relative_to/is_relative_to compare components
//     byte-exactly and case-sensitively on every flavor including Windows
//     (the path engine bullet above explains why). The contrast is with those
//     operations only: the / operator folds case, because pathJoin
//     (pathval.go) compares the two operands' drives with strings.EqualFold,
//     as ntpath.join does, so path('C:/a') / 'c:b' keeps the parent and gives
//     "c:\a\b", where path('C:/a') / 'd:b' discards it and gives "d:b" (both
//     under WithPathFormat(PathWindows)). That drive test is the only
//     case-insensitive comparison the path engine makes, and the package's
//     only strings.EqualFold outside pathmapping.go. Source matching is a
//     different operation from path equality, and the two answers disagree on
//     the same pair of paths: a WINDOWS rule whose SourcePath is `C:\studio`
//     maps 'c:/STUDIO/project/scene.ma', while path('C:/Studio') ==
//     path('c:/studio') is false and is_relative_to(path('C:/Studio/a'),
//     path('c:/studio')) is false, both under WithPathFormat(PathWindows).
//     The insensitivity is confined to the rule comparator (applyFileRule
//     passes strings.EqualFold to relativeParts, otherwise the same function
//     is_relative_to uses); a POSIX rule does not casefold, so a "/Projects"
//     rule leaves '/projects/a.exr' untouched.
//
//     Its parameter is a string and its result is a path, which makes the
//     section 1.2.4 receiver restriction visible: the result chains, so
//     apply_path_mapping('/projects/shot01/out.exr').stem is "out", but a path
//     receiver is refused (path('/projects/a').apply_path_mapping() reports
//     `no signature of "apply_path_mapping" accepts (path)`), because a method
//     receiver is not coerced. Both are pinned by
//     TestApplyPathMapping_ResultChainsButAPathReceiverIsRefused. Two calls
//     therefore do not compose directly; ask for a string in between.
//
//     It has no oracle coverage: the reference's evaluation entry points that
//     scripts/expr-oracle.py calls accept no host context, so there is no
//     channel for session mapping rules, and no case in
//     test/oracle/corpus.txt names this function. Everything above is pinned
//     by unit tests in this package, as the path engine's Windows semantics
//     are.
//
//     Some of those unit tests carry the specification's expectations rather
//     than sqi's own reading. The conformance suite ships three fixtures
//     written against apply_path_mapping:
//     EXPR/jobs/expr2.3.2--apply-path-mapping.test.yaml and its
//     --uri-source-path-mapping-posix and --uri-source-path-mapping-windows
//     siblings, carrying 31 expected strings across their
//     expected.output/output_posix/output_windows blocks, rules included. The
//     scored harness does not read them: collectTemplateFixtures
//     (test/conformance/suite_test.go) keeps only the job_templates and
//     env_templates kinds and skips job-execution fixtures, which need a live
//     session runtime. They are transcribed into
//     TestApplyPathMapping_VendoredFixtureExpectations (35 assertions; the
//     four flavor-independent ones run under both formats), the only external
//     check this function has.
//
//     sqi has two path-mapping implementations. internal/worker/pathmap does
//     a strings.ReplaceAll substring swap anywhere in a command string; this
//     engine matches a prefix on component boundaries and rebuilds the
//     remainder in the destination flavor. They serve different jobs, and
//     this leaf package cannot import that one.
//
//     Host-context enforcement is not here. RFC 0006 makes
//     apply_path_mapping valid only in host-context (@fmtstring[host]) scopes
//     and an error anywhere else; this package has no scope model to enforce
//     that with, and internal/openjd's checker does (exprcheck.go's
//     hostOnlyFunctions).
//
//   - Bounded evaluation implements the specification's two configurable
//     resource limits, section 1.3.9's memory-bounded evaluation and section
//     1.3.10's operation-bounded evaluation, both through meter.go.
//
//     Two options, on by default, with no unlimited mode.
//     WithMemoryLimit(bytes) bounds the live memory, in bytes, that one
//     evaluation's values hold at once; it defaults to 100,000,000.
//     WithOperationLimit(ops) bounds the number of section 1.3.10 operations
//     one evaluation may perform; it defaults to 10,000,000 (meter.go). Both
//     apply with no option passed, so a caller that forgets the option is
//     not left unbounded. A non-positive value to either option is a caller
//     error, reported by Eval: WithMemoryLimit(0) and WithOperationLimit(-1)
//     both fail with "... must be positive, got ...".
//
//     The floor. limits.go's five hard, non-configurable bounds
//     (maxElements, maxStringBytes, maxParseDepth, maxEvalDepth and
//     maxSourceBytes, described in the hard-bound bullet below) sit
//     underneath these two configurable ones and are not raised by them:
//     raising WithMemoryLimit does not raise what one operation may allocate.
//     Evaluating "'a' * 20000000" under WithMemoryLimit(1_000_000_000) still
//     fails, with the same error as with no option: "col 5: the result is too
//     large: repeating 1 elements 20000000 times exceeds the limit of
//     10000000", with errors.Is(err, errTooLarge) true and errors.Is(err,
//     errMemoryLimit) false. checkRepeat's maxStringBytes ceiling (limits.go)
//     is checked before any WithMemoryLimit accounting sees the string.
//
//     Limits are per Eval, not per template. Each call to Expression.Eval
//     (and Eval, EvalWithMetrics, EvalForBalanceCheck, which route through
//     it) starts a fresh meter with its own budget; nothing here accumulates
//     a total across the expressions evaluated for one template. The
//     cumulative, template-wide budget is internal/openjd's (exprcheck.go's
//     templateBudget).
//
//     Unresolved operands. A dispatched call whose argument is still
//     unresolved (the type-check path taken before a parameter is bound to a
//     value) charges section 1.3.10 rule 1 only, the single operation for
//     the call itself (call.go's callFunction, plus three mirroring sites in
//     ops.go: binary, unary and the fast-path equality check; four
//     unresolved-operand charge sites in all). Rules 2 and 3 do not apply:
//     the call was dispatched, but no list was iterated and no string was
//     processed, because nothing downstream of the unresolved operand ran.
//     The reference implementation has no comparable unresolved-operand
//     evaluation path to compare against.
//
//     Rule 2's enumeration is open. The two specification documents disagree
//     about it, and it is settled here once so that the package cites one
//     reading:
//
//     wiki/2026-02-Expression-Language.md lines 1080-1087 introduces rule
//     2's function list with "such as", an open enumeration.
//     rfcs/0005-expression-language.md lines 1173-1182 introduces the same
//     list with "This applies to:", read literally a closed one.
//
//     The ruling is open, on four grounds. (1) The wiki page is the
//     specification for this package (see the closing note at the bottom of
//     this file); RFC 0005 is the proposal that produced it, and where the
//     two disagree the specification governs. (2) Both documents state the
//     same operative sentence first ("When a function or the evaluator
//     iterates through every element of a list, the number of elements is
//     added to the operation count"), and both introduce rule 3's parallel
//     list with "and similar", the RFC included. A closed rule 2 beside an
//     open rule 3, on two rules of identical construction, is not a reading
//     either document argues for. (3) A closed reading makes rule 2's words
//     "or the evaluator" inoperative for anything unnamed: no
//     evaluator-performed iteration except the list comprehension is in the
//     enumeration, and the comprehension is listed separately because the
//     evaluator, not a function, performs it, so the general clause must
//     reach further than the list or it says nothing. (4) A closed reading
//     defeats the rule's stated purpose: five constructs that do O(receiver)
//     work would be charged a flat 1, so an expression like
//     "len([range_expr('1-1000000')[0] for x in range(500)])" would do
//     millions of elements of work for about 2,000 counted operations.
//
//     What the open reading does not license. It is not a mandate to charge
//     everything: each row still answers whether the work is
//     element-count-dominated or byte-scan-dominated, and whether the
//     implementation provably does that work. split() stays uncharged for
//     rule 2 because its work is byte-dominated; "not", unary minus and
//     unary plus stay uncharged because they touch no list and no string;
//     min/max's fixed-arity 2- and 3-argument overloads stay uncharged
//     because they take no list-typed parameter. Being unnamed by the
//     enumeration is not a reason to charge nothing, and being named is not
//     required to charge.
//
//     Five constructs do O(receiver) work and are charged for it. Each
//     expands or decodes its whole receiver inside a single call, and each
//     is charged as the slice form of the same expression is:
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
//     A subscript of a list stays at a flat 1: AsList returns the backing
//     slice with no copy (value.go), so a list subscript touches one element
//     and iterates nothing. The receiver-kind split is the same one
//     slice.go's sliceValue makes, matching the asymmetry the implementations
//     have.
//
//     List ordering charges its walk, like list equality. "[...] < [...]"
//     and "[...] == [...]" run the same compareLists/valuesEqual walk over
//     the same operands. Rule 2 names "list/range equality comparisons" by
//     token and does not name ordering, but under the open reading the
//     general sentence covers both, and charging only equality would let a
//     large list compared many times under "<" cost a small fraction of the
//     identical "==" expression. orderingShapes' list/list row declares
//     Cost{ArgElements: {0}}, charging the left operand as applyBinary's
//     equality path does. The four scalar ordering rows charge rule 1 only;
//     they iterate nothing.
//
//     Counting rulings. Where sqi's operation counter and the reference's
//     diverge on a corpus case, the case is adjudicated and recorded in
//     test/oracle/baseline-ops.txt, grouped under root-cause families (its
//     "# ── " section headers enumerate them; that file is where to check one
//     entry). Most are the reference over-counting by a flat +1 on operators
//     RFC 0005's AST-transformation table routes around section 1.3.10 rule
//     1 rather than through a function call (ordering, equality, "not",
//     "and"/"or", the ternary, "in"/"not in"), adjudicated as the
//     reference's bug. A second class is the two pricing the same call
//     differently (string(list), the repr_* family, unique()'s
//     per-comparison charge, see below), or sqi charging nothing where the
//     reference charges (round(x, ndigits>0), min/max's fixed 2- and 3-arg
//     rows), each argued from the RFC 0006 text in that file's section for
//     it.
//
//     The section 1.3.10 worked example's arithmetic is wrong. The
//     specification states 'a' * 100000 costs 393 operations; the correct
//     total is 392: ceil(100000/256) is 391 (256*390 = 99840, leaving 160),
//     plus 1 for the repetition call. EvalWithMetrics("'a' * 100000", nil,
//     TString) reports 392, as the reference implementation does. The rule
//     (meter.go's ceilDiv256) is right; only the specification's addition is
//     off, and meter.go records the correction beside the function.
//
//     Value.String() and string(list) render a list identically, quoting and
//     escaping included. openjd-specifications#176 requires format-string
//     interpolation to use "this same conversion" as string() and the result
//     to "parse as JSON"; Go's spelling of a control character (\x01, and
//     \a/\v for two JSON does not name) is not JSON. Both renderers call
//     funcsconv.go's jsonQuoteElement, so they agree for every input.
//     TestValueString_VersusStringFunction checks thirteen cases (an embedded
//     double quote, non-ASCII, angle brackets and an ampersand, a newline, a
//     backslash, tab, backspace, formfeed, carriage return, an emoji,
//     U+2028/U+2029, and the empty string), and
//     TestValueString_ListQuotingIsJSONEverywhere pins six more where
//     strconv.Quote and encoding/json disagree (the C0 controls, vertical
//     tab, DEL and invalid UTF-8). The separator differs from internal/openjd's
//     canonical storage form ("," there, ", " here), and paramjson.go states
//     that difference where it lives.
//
//     unique() charges per comparison, not per element. A registry-level
//     Cost{ArgElements: {0}} charge prices only the input length, while
//     unique's work is quadratic: unique(range(20000)) runs on the order of
//     4*10^8 valuesEqual comparisons. uniqueList (funcslist.go) is an FnCtx
//     function charging one operation per valuesEqual comparison, and the
//     charge fires before the comparison it prices, so the limit stops the
//     quadratic work while it is in progress
//     (TestUnique_IsBoundedByTheOperationLimit).
//
//     The top-level coercion is metered. coerceTop (eval.go), which every
//     public entry point (Expression.Eval, Eval, EvalWithMetrics,
//     EvalForBalanceCheck) runs on the whole expression's result, releases
//     the pre-coercion value and allocates the coerced result with
//     WithMemoryLimit's bound checked. Coercion (coerce.go) can turn a small
//     live range_expr into an arbitrarily large list[int]; unmetered, that
//     materialization would be bounded only by the fixed maxElements floor
//     (10,000,000 elements, roughly 640MB at 64 bytes a value).
//
//     Untrusted expressions. With both limits on by default
//     (TestOperationLimit_*, TestMemoryLimit_* and
//     TestUnique_IsBoundedByTheOperationLimit), this package may be handed
//     untrusted expressions, with three residuals: the fixed floor
//     (limits.go) still bounds what a single operation may allocate,
//     unchanged by either configurable limit; maxParseDepth and
//     maxSourceBytes guard parsing, a phase no evaluation-time budget can
//     reach, so what a hostile expression may spend before the first
//     operation charge is whatever those two hard bounds allow; and limits
//     are per Eval, so a template with many expressions gets one independent
//     budget per expression evaluated (the cumulative per-template budget is
//     internal/openjd's).
//
//     None of it bounds parsing. Every limit named in this bullet (the two
//     configurable ones, the floor beneath them, the template-wide budget
//     internal/openjd layers on top and the deadline below) is consulted
//     from two places, meter.charge and meter.reserve, and both run during
//     evaluation. Parse runs first: tokenize builds the entire token slice
//     and the parser the entire tree before any budget exists to be spent.
//     maxParseDepth does not help, because it bounds recursive-descent stack
//     frames and a flat left-associative chain is read in a loop; a
//     multi-megabyte "1+1+1+…" expression would parse into hundreds of
//     megabytes of tokens and nodes, and an expired deadline would not stop
//     it, since nothing reads the clock during the parse. maxSourceBytes
//     (limits.go, 10,000 bytes, checked at the top of Parse before tokenize)
//     closes it: it is the one bound in this package that applies before
//     evaluation begins. It is a 4xx verdict, not a 503: a source that is too
//     long is a deterministic property of the template, so it reaches a
//     caller as an ordinary parse error (a ValidationError through
//     internal/openjd) rather than through ErrDeadlineExceeded.
//
//     The wall-clock deadline (WithDeadline) is a bound of a different
//     shape. Everything above bounds work: bytes held, operations performed,
//     elements built, nesting depth. None of them bounds time, and they
//     cannot: section 1.3.10 prices 256 bytes at one operation, so
//     '("x" * 900000).title()' spends about 7,000 of a 10,000 operation
//     budget while doing far more CPU work than that count suggests, and a
//     request that stays inside every budget above can still run for
//     minutes. WithDeadline(t) sets an absolute wall-clock instant after which
//     evaluation stops. The zero time, the default, means no deadline and
//     reads no clock.
//
//     It differs from every other bound here in three ways:
//
//     (1) It is wall-clock, and therefore non-deterministic. The same
//     template on the same input breaches it on a loaded host and not on an
//     idle one. Every other bound in this section is a function of the
//     expression alone.
//
//     (2) It is per request, not per Eval. The option takes an absolute time
//     rather than a duration so that one deadline spans the many evaluations
//     a template walk performs; a per-evaluation duration would let each of
//     hundreds of positions spend the whole allowance. That is also why
//     meter.charge checks it on the first charge of every evaluation and not
//     only on the interval: a template built entirely from cheap expressions
//     must still stop, and each of those expressions gets a fresh meter.
//
//     (3) A breach means the server gave up, not that the template is
//     invalid. Every other bound here is a verdict about the expression, and
//     a caller is right to reject the submission (422). ErrDeadlineExceeded
//     is exported, and distinct from every budget error, so that a caller can
//     tell the two apart with errors.Is rather than by reading a message,
//     and report it as a transient server condition (503).
//
//     Precedence, when a single charge breaches both: the operation limit
//     wins. meter.charge checks it first: the deterministic verdict is the
//     more useful one, and reporting an over-budget template as a transient
//     server condition would invite a client to retry something that can
//     never succeed (TestDeadline_OperationLimitBeatsTheDeadline).
//     meter.reserve orders the two the same way.
//
//     It is a backstop, not a budget, and its granularity is coarse. The
//     clock is sampled, on the first charge and every deadlineCheckInterval
//     (1024) charges thereafter, because charge is the hot path of every
//     expression and a time.Now() on each call taxes evaluations that are
//     nowhere near a deadline. So the guarantee is "the deadline plus at most
//     one evaluation's remaining operation budget": up to 1023 further charge
//     calls may run, each of them potentially a bulk operation, and the call
//     count is held to the operation budget only because a charge that
//     prices bulk work adds at least one operation to it. meter.reserve
//     narrows the worst of that window by checking unconditionally, with the
//     count in hand, immediately before each large materialization, so the
//     single biggest construction inside the window never starts. The rest of
//     the window is not closed here.
//
//   - A union target that names a value's own type exactly admits it
//     unchanged, list types included: Eval("[1.0, 2.0]", nil,
//     expr.UnionOf(expr.ListOf(expr.TFloat), expr.ListOf(expr.TInt)))
//     returns the list rather than failing with "list[float] cannot be
//     coerced to list[float] | list[int]". coerce() (coerce.go) checks
//     satisfaction first (a value whose type the target already admits is
//     returned unchanged), comparing whole types rather than type codes,
//     since a code-only test cannot tell a list[int] from a list[string]
//     inside a union. Do not conflate that with coercible(), which answers a
//     different question: whether an implicit conversion rule carries a type
//     to the target while a function call is being resolved.
//     coercibleToTarget answers the target-type question under RFC 0005's
//     coercion rules, so the two disagree on purpose (coerce.go).
//
//     nulltype is the one case where satisfaction and coercible() coincide:
//     coercible(nulltype, to) reports true whenever `to` admits null
//     (coerce.go), because there is no separate null-to-something conversion
//     rule to ask about, only whether the target's union names null.
//     coercible(nulltype, "string?") is true because the target admits it;
//     TestCoercible's nulltype rows pin this, alongside the negative boundary
//     ("nulltype does not reach a union that does not name null") showing
//     the rule still checks target membership.
//
//   - A hard, fixed bound (limits.go's maxElements and maxStringBytes, both
//     10,000,000) applies to any list, string or range_expr this package
//     produces, whether by repetition ("'x' * 3", "[0] * 3") or by
//     concatenation ("'a' + 'b'", "[1] + [2]"). Both producers are checked
//     on both types; without the string-concatenation check, a chain of
//     individually legal repetitions can grow many times past the bound.
//     A third bound, maxParseDepth (500), applies before any value exists:
//     the parser's recursive descent is depth-limited, because exhausting the
//     Go stack is a runtime.throw that recover() cannot catch, and a
//     200,000-deep list literal would kill the process rather than return an
//     error. Every recursion cycle in the descent is counted, not only the
//     entry production: parsePower reads its exponent through parseUnary,
//     and parseUnary falls through to parsePower, so an unguarded cycle would
//     let "2**2**2**…" a million operators long kill the process (parser.go's
//     enter carries the full enumeration). A fourth bound, maxEvalDepth
//     (10,000), does the same for evaluation, which the parse guard cannot
//     cover: a left-associative run like "true or true or …" or "1 + 1 + …"
//     is built by a loop, so it costs the parser no recursion and passes
//     maxParseDepth however long it is, while the left-deep tree it produces
//     is then walked recursively by evalNode, which would overflow the stack
//     at a few hundred thousand operators without the bound.
//     A fifth bound, maxSourceBytes (10,000 bytes), applies earlier than any
//     of them, at the top of Parse before tokenize reads a byte, because the
//     four above leave the parse unmetered: a flat chain passes
//     maxParseDepth for the reason the fourth bound's sentence gives, and
//     reaches maxEvalDepth only after every byte has been tokenized and
//     turned into nodes. It bounds source length rather than node count; see
//     limits.go for why, and for the headroom over real templates (the
//     largest expression in any vendored fixture, sample or preset is 99
//     bytes).
//     None of the five is the spec's configurable memory and operation
//     limits (sections 1.3.9 and 1.3.10), which are a separate mechanism,
//     covered in the bounded evaluation bullet above. The five bounds here
//     stop one operation from allocating unbounded memory, stop the parser
//     and the evaluator from overflowing the stack, and stop one
//     expression's source from being large enough that reading it costs
//     hundreds of megabytes, a narrower job than the configurable limits,
//     which stop a pathological expression from doing unbounded total work
//     across many operations. The one remaining walk over a parsed tree,
//     ast.go's walk, which Expression.Names uses, needs no bound: it is
//     iterative, with an explicit stack, so its Go stack depth is constant
//     however deep the tree is. Names returns []string with no error
//     channel, and a tree that parsed cannot fail to be walked.
//
//   - A float literal in expression source does not preserve the text it was
//     parsed from: "1.100" evaluates to a value that renders as "1.1", not
//     "1.100". Value.String is a rendering for diagnostics and tests, not
//     section 1.3.4's pass-through, and no producer sets the literal path's
//     carry. Value has an optional rendered form (value.go's fs field,
//     exported construction via FloatText), set by round(x, ndigits) for a
//     positive ndigits because RFC 0006 requires round(3.5, 2) to render
//     "3.50", a form no float64 carries on its own
//     (TestRound_CarryIsRoundOnlyAndDoesNotPropagate), and by ValueFromText
//     (paramtypes.go) for the other half of section 1.3.4: a FLOAT job
//     parameter's submitted text, which sqi has verbatim because parameters
//     are stored as map[string]string. internal/openjd's phase-2
//     concreteJobParamValue (exprcheck.go) and the worker both bind through
//     it, so Param.<name> for a submitted "3.500" carries that text and
//     Value.String reports it. internal/worker/fmtres renders phase-3 values
//     onto a real command line, and pins that a submitted "3.500" arrives as
//     3.500 through both plain substitution and repr_py. Value.String quotes
//     a list's string elements as string()'s JSON row (funcsconv.go) does;
//     the bounded evaluation bullet above states how far the two renderings
//     agree.
//
//   - Nothing here touches a job template. Parsing a template, binding its
//     parameters, and interpolating an expression's result back into template
//     text are all outside this package; it evaluates expression text handed
//     to it directly.
//
//   - Section 1.1.5's escape table is narrower than its opening claim that
//     "all Python escape sequences are supported": \a, \b, \f, \v, \0, octal
//     escapes and a backslash-newline line continuation are absent from the
//     table, so this package keeps them verbatim, backslash included, rather
//     than decoding them: "'\a'" evaluates to the two characters "\a". This
//     follows the table over the prose.
//
// A target type propagates into a sub-expression from exactly four node
// kinds, and nowhere else. The rule is not visible from outside the package,
// so it is spelled out here:
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
// The four "forwards" rows compute a value that is one of the
// sub-expression's values, so the caller's target applies to it directly.
// ListComp's element expression joins ListLit's for the same reason: each of
// a comprehension's produced elements is the element expression's value, one
// per iteration, as each of a literal's elements is. Everything else computes
// a new value from its operands, so forwarding the target would leak context
// across an operator or call boundary (a string target reaching into
// "Param.Count + 1" would concatenate its operands into "11" rather than add
// them), and a subscript's index or a slice's bound is fixed at TInt because
// that position must be an int whatever the whole expression is being
// coerced to. A call's return type is fixed by the signature callFunction
// selects, not by the caller's target, so a target reaching into an argument
// would let the caller's context change which overload the callee resolves
// to.
//
//   - Compiled regular expressions are cached for the lifetime of one
//     evaluation, and for that lifetime only (recache.go). Without the cache,
//     every re_match/re_search/re_findall/re_sub/re_split invocation runs
//     translatePattern's full byte scan and regexp.Compile again, so a
//     pattern inside a comprehension is compiled once per element while
//     section 1.3.10 charges it roughly one operation plus ceil(len/256),
//     under-priced by about three orders of magnitude, and on the worker
//     there is no wall-clock deadline to backstop it.
//
//     It changes no result and no count. Section 1.3.10's charges are applied
//     by callShape (ops.go) around the whole call, before Fn or FnCtx runs, so
//     a cached compile is charged exactly what an uncached one is. A failing
//     pattern is cached too, and its error value is returned unchanged on
//     every hit, so an invalid pattern in a comprehension is rejected with
//     the same message each time.
//
//     It is capped at maxCachedPatterns entries. A pattern is an arbitrary
//     string from a submitted template, not necessarily a literal, so
//     "[re_match(s, s) for s in Param.Files]" presents one distinct pattern
//     per element; an uncapped map would retain one compiled program per
//     element and turn a comprehension into a memory amplifier. Past the cap
//     the cache stops storing (nothing is evicted and the map does not grow),
//     so what it retains is a constant, not a function of the input. Being
//     per-evaluation is what makes that bound hold across requests, and is
//     also why the cache needs no eviction policy and no lock.
//
//   - This package imports internal/openjd/intrange for range_expr's
//     grammar, section 3.4.1.1.1's <IntRangeExpr>. internal/openjd expands
//     the same grammar for a different purpose, a step's task parameter
//     space, with its own divergences from both this package and the spec:
//     it orders values first-seen rather than increasing, rejects a range
//     whose start exceeds its end where the spec yields a single value, and
//     rejects a negative step the spec permits. Both callers share intrange's
//     Range type but use different entry points (Parse here, ParseWithPolicy
//     there) because they disagree about what to accept; see the intrange
//     package doc for the full comparison.
//
//   - Test coverage. The OpenJD conformance suite's EXPR/job_templates group
//     is scored by make test-conformance. The protected-fixture tests
//     (TestConformance_*Fixtures in test/conformance/suite_test.go) also
//     assert by name that specific .invalid fixtures are rejected: a
//     construct this comment describes as rejected, such as the
//     comprehension shadowing check or a parser/lexer rejection of a form
//     Python allows; the conversion and math functions rejecting an invalid
//     argument (an empty list or path into bool(), an unrecognized string
//     into bool(), an empty list to min/max, an unrepresentable
//     float-to-int, NaN and infinity into float(), an empty string or list
//     into range_expr()); the string functions rejecting an invalid argument
//     (an empty substring into count/find/replace, an empty separator into
//     split/rsplit, a missing substring into index/rindex); the regex scanner
//     and re_sub replacement check rejecting an unsupported construct (an
//     empty pattern, \z, lookahead, lookbehind, a named backreference, and
//     re_sub's four group-reference spellings); and the path functions
//     (relative_to's errNotRelative, with_number's errPaddingTooWide,
//     section 1.2.4's receiver restriction refusing to coerce a path receiver
//     into a string method, and bool()'s path row). Each must be rejected by
//     real validation rather than by an accidental "unknown function", so
//     that one fixture starting to pass for the wrong reason while another
//     regresses cannot hide inside an unchanged aggregate score. Such a test
//     asserts the fixture is still rejected, not which mechanism rejected it,
//     so an entry pins a specific check only where no second mechanism
//     rejects the same input.
//     The differential oracle (make test-expr-oracle) compares values against
//     the reference implementation over test/oracle/corpus.txt, and, on every
//     case whose value agrees, operation counts (EvalWithMetrics against the
//     reference's evaluate_with_metrics; a case with a value divergence is
//     reported once and not double-counted). Value divergences are
//     adjudicated in test/oracle/baseline.txt, count divergences in
//     test/oracle/baseline-ops.txt (see the counting rulings paragraph of
//     the bounded evaluation bullet). The path corpus is large and its
//     divergences are the reference's, argued family by family in
//     test/oracle/baseline.txt (its non-normalizing "/" and "+", its
//     flavor-blind as_posix, its older pathlib stem/suffix split, its
//     with_number hash-swallow, its inconsistent trailing-slash consumption
//     on a relative_to base, and its treatment of a bare "scheme://" as a
//     wildcard authority). The baselined value entries include several
//     round() cases. One is the return type: sqi returns int for round(x,
//     ndigits) when ndigits <= 0, per RFC 0006's signature table, while the
//     reference returns a float (round(1234.5, -1) is "1230 : int" here and
//     "1230.0 : float" there). The specification outranks the reference (it
//     is Beta, 0.x, with breaking changes permitted in minor releases), so
//     this is adjudicated as the reference's bug and recorded in
//     test/oracle/baseline.txt. Also baselined are two of the regex and
//     repr_* rulings argued above, the [^\W...] set-subtraction gap and
//     repr_sh's textual difference from the reference; \d/\w/\s's Unicode
//     semantics are not, since the oracle checks agreement on them, and
//     neither is repr_json's non-ASCII escaping, since the reference matches
//     it. The path rulings above are baselined where they diverge;
//     case-sensitive path comparison is not, because the reference agrees
//     with sqi there and CPython's PureWindowsPath disagrees with both, which
//     the oracle does not see because it runs the reference under POSIX
//     only. apply_path_mapping has no oracle case (see the path mapping
//     bullet above). Most baselined divergences are the reference's bugs,
//     adjudicated against the spec text and recorded one by one; those
//     files are the place to check any individual ruling.
//
// Anything unimplemented fails rather than misbehaving, with one exception:
// the escape sequences named above pass through verbatim instead of erroring
// or being decoded. The grammar is EXPR's own rather than borrowed from a
// Python parser so that failure runs in that direction: a borrowed parser
// would accept syntax this package cannot evaluate.
//
// # Extending it
//
// Two shapes carry the weight and should not be reshaped:
//
//   - Type is a code plus its type parameters (Params), built through
//     normalizing constructors (UnionOf, ListOf, UnresolvedOf and the rest)
//     rather than as struct literals, so that two types meaning the same
//     thing always have the same shape and Equal is sufficient everywhere
//     downstream. Value carries a Type rather than inferring one from which
//     payload field is set; an unresolved value carries no payload at all,
//     which is what makes a typed-but-valueless result possible.
//   - Operator behavior is an ordered list of Shapes per operator (shape.go,
//     ops.go), not a switch or a map keyed on operand types; a Type
//     containing a slice cannot be a map key. Each Shape declares the types
//     it takes and the type it returns, which is what lets a missing operand
//     value still produce a typed result. New signatures are new Shape
//     entries; a candidate list with no admissible match is reported as
//     "unsupported operand types".
//
// The specification is the OpenJD wiki page "Expression Language [Extension:
// EXPR]", pinned in the third_party/openjd-specifications submodule. Section
// numbers cited in this package's comments refer to it.
package expr
