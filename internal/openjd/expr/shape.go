// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

// Shape is one accepted signature of an operator or function: the types it
// takes, the type it gives back, and the code that computes it.
//
// Sub-project A dispatched on a map keyed by operand kind. Two facts retired
// that. A Type contains a slice, so it cannot be a Go map key at all. And a map
// entry does not record what it returns, which makes it impossible to answer
// "what would this produce?" for an operand that has no value — the question at
// the center of the spec's static type checking. A declared Ret answers it
// without executing anything.
//
// Params and Ret may contain type variables (CodeVarT and friends), bound at
// match time and substituted into Ret afterward, so that a signature like
// __getitem__(list[T], int) -> T returns the element type. Sub-project C
// registers its function library into this same structure.
type Shape struct {
	// Params are the declared parameter types, one per argument.
	Params []Type
	// Ret is the declared result type. It may be a union: the spec's own
	// __pow__(int, int) -> float | int is one.
	Ret Type
	// RetOf, when set, computes the declared result from the match's variable
	// bindings INSTEAD of substituting them into Ret.
	//
	// It exists for a result type that is a UNIFICATION of the operands rather
	// than a copy of one of them. List concatenation is the case: its two
	// element variables are independent (they must be, or "[1] + [2.0]" would
	// not match at all), so no substitution into Ret can say what section
	// 2.1.3's "common type" of the two is — "[] + list[int]" reads the LEFT
	// variable and answers list[nulltype], and "list[int] + []" answers
	// list[int], for the same concatenation. concatLists computes the real
	// answer from the VALUES, which is no help on the path where there are no
	// values; this is that same computation over types.
	RetOf func(b bindings) Type
	// Fn computes the result. It is called only with arguments that matched
	// Params, so it may use the payload accessors without checking.
	Fn func(args []Value) (Value, error)
	// FnCtx is Fn for the few implementations that need the evaluation's
	// settings rather than only their arguments.
	//
	// The rule for which rows use it is not a tally, it is a question: does
	// this row have to CHOOSE a flavor at construction time? All three of
	// path()'s rows do — path(string), path(list[string]) and
	// path(list[nulltype]) — because constructing a path is the only
	// operation that ever has to decide one; every other path operation reads
	// the flavor off its receiver. Adding a parameter to Fn instead would have
	// changed every OTHER row, which does not need it; RetOf is the existing
	// precedent for an optional alternative field.
	//
	// A Shape sets Fn or FnCtx, never both.
	FnCtx func(ec evalCtx, args []Value) (Value, error)
	// Promote narrows which conversions this shape's parameters will accept to
	// admit an argument of a different type. The zero value accepts every
	// lossless conversion coerce.go allows, which is right for most operators
	// and wrong for ordering — see promotion.
	Promote promotion
	// Cost declares this signature's section 1.3.10 charges beyond the single
	// per-call operation. See Cost.
	Cost Cost
}

// Cost declares a Shape's section 1.3.10 rule-2 and rule-3 charges.
//
// The single per-call operation of rule 1 is charged by callShape for EVERY
// shape and is deliberately not declared here: making it declarative would let
// a forgotten entry silently un-count a call, which is the one charge that can
// never legitimately be zero.
//
// The zero value charges nothing beyond that call, which is correct for the
// functions section 1.3.10 explicitly exempts -- "simple lookups like len()
// that do not process the string content".
type Cost struct {
	// ArgElements are parameter indices charged by element count (rule 2:
	// "when a function or the evaluator iterates through every element of a
	// list, the number of elements is added").
	ArgElements []int
	// ArgBytes are parameter indices charged by ceil(len/256) (rule 3: "when a
	// function processes a string or path value, the length of the value
	// divided by 256, rounded up, is added").
	ArgBytes []int
	// ResultElements charges the element count of the value PRODUCED. Rule 2
	// covers generators as well as consumers: range(1000) iterates a list it
	// created rather than one it was given.
	ResultElements bool
	// ResultBytes charges the byte length of the value PRODUCED, for a function
	// whose work scales with what it built rather than with what it read.
	ResultBytes bool
}

// specNamedIteratingFunctions returns section 1.3.10 rule 2's own list of
// named REGISTRY functions -- deliberately not verbatim against the wiki's
// token list, see the CORRECTION below -- in the wiki's order (2026-02
// Expression-Language.md, section 1.3.10). It exists so the per-group coverage
// tests in sub-projects Tasks 5-8 assert against the SPEC TEXT rather than
// against a hand-copied list that could silently drift from it.
//
// Rule 2 also names three things that are not registry entries at all: list
// concatenation ("+"), list repetition ("*"), and list/range equality
// comparisons. Those are binaryShapes rows, not functionShapes ones, so this
// function -- which names FUNCTIONS -- omits them; the operator charges are
// each Task's own to declare on the row that implements them.
//
// CORRECTION (review finding, Task 5): "contains()" is omitted here too, and
// for the same reason as the three operator-shaped items above, not by
// oversight. RFC 0005's dunder-transform table (lines 1445-1446) is explicit:
// "In -> __contains__, NotIn -> __not_contains__ ... x in y becomes
// __contains__(y, x)". Rule 2's "contains()" names that dunder -- the "in"
// operator itself, written by its transform name rather than its surface
// syntax -- not a functionShapes registry entry; there is no function named
// "contains" anywhere in the spec or in sqi's registry. An earlier revision
// of this list included it, which would have made Task 8's coverage test
// (every name here must resolve to a functionShapes entry) permanently and
// correctly fail, since "contains" will never be registered as a function.
// The charge itself is declared on OpIn/OpNotIn's rows in ops.go, not here.
func specNamedIteratingFunctions() []string {
	return []string{
		"sum", "min", "max", "any", "all", "sorted", "reversed", "flatten",
		"join", "range",
		"repr_sh", "repr_py", "repr_json", "repr_pwsh", "repr_cmd",
	}
}

// specNamedStringFunctions returns section 1.3.10 rule 3's own list of named
// functions, verbatim and in the wiki's order. See specNamedIteratingFunctions
// on why this mirrors the spec text rather than a hand-copied list.
//
// "join" and "repr_sh" appear in BOTH this list and
// specNamedIteratingFunctions's: the spec names them under both rules because
// each does work proportional to element count (rule 2) AND to the string
// content it reads or writes (rule 3) -- a duplication in the return values
// here, not a mistake.
//
// Rule 3 also names "regex functions" and "and similar" without spelling out
// which registry entries those are, and names string concatenation ("+") and
// string repetition ("*"), which -- like their list counterparts above -- are
// binaryShapes rows rather than functionShapes entries. None of those four are
// returned here for the same reason: this function names things the SPEC TEXT
// itself names, and leaves resolving "regex functions" to the concrete
// re_-prefixed registry entries, and the two operators to their own shape
// rows, to the tasks that declare Cost on them.
//
// Checked for specNamedIteratingFunctions's "contains()" mistake (review
// finding, Task 5) and found clean: rule 3's own text names no dunder-style
// operator entry under a function-looking name the way rule 2's "contains()"
// did -- "in"/"not in" against a string ARE charged (OpIn/OpNotIn's
// TString,TString rows in ops.go), but rule 3 never names them by any token,
// dunder or otherwise, so there was nothing here to remove.
func specNamedStringFunctions() []string {
	return []string{
		"upper", "lower", "replace", "split", "join", "strip",
		"repr_sh",
	}
}

// promotion selects the set of conversions a shape's parameters accept.
//
// It exists because admissibility is NOT uniform across operators, which
// sub-project B1 assumed and documented as a divergence when it turned out
// false. Section 1.2.3's range_expr -> string coercion is real, and section
// 2.1.2 has an explicit string + range_expr row that depends on it; but section
// 2.1.4 restricts ORDERING to int, float, string, path and bool, crossing type
// only between int/float and string/path. With one global predicate, "r < 'a'"
// was accepted.
type promotion int

const (
	// promoteDefault accepts every lossless conversion, which is what section
	// 1.2.3 permits generally.
	promoteDefault promotion = iota
	// promoteOrdering accepts only section 2.1.4's two named compatible pairs,
	// int/float and string/path, plus the elementwise form of each for lists.
	promoteOrdering
	// promoteNoRangeText accepts every lossless conversion EXCEPT
	// range_expr -> string, which section 1.2.3 defines but which the operator
	// tables grant to ONE operator only. See the OpMul table's comment on why
	// that is a reading of the spec rather than a restriction invented here.
	promoteNoRangeText
)

// promotableUnder is promotable, restricted by a shape's promotion set.
func promotableUnder(pr promotion, from, to Type) bool {
	if !promotable(from, to) {
		return false
	}
	if pr == promoteNoRangeText {
		return unwrapUnresolved(from).Code != CodeRangeExpr
	}
	if pr != promoteOrdering {
		return true
	}
	f, t := unwrapUnresolved(from), unwrapUnresolved(to)
	// Section 2.1.4's compatible pairs, and only these. Both are pairs of
	// SCALARS, which is all this function ever has to answer for: a list
	// argument against a list parameter never reaches here at all, because
	// argCost intercepts that pair first and descends elementwise through
	// argCostList. The elementwise form of these pairs — section 1.2.5's
	// "[1] < [1.0]" — is therefore applied there, by typeVarCost's ordering
	// unification, not here.
	//
	// An earlier revision carried a list branch of its own for that case,
	// unreachable on every path and flagged as such; it was removed once the
	// elementwise rule landed for real, rather than left as an untestable
	// second implementation of it. A list argument reaching a SCALAR parameter
	// is the only list-shaped call this function sees, and promotable() above
	// has already answered it false.
	switch {
	case f.Code == CodeInt && t.Code == CodeFloat:
		return true
	case f.Code == CodePath && t.Code == CodeString:
		return true
	}
	return false
}

// numTypeVars is how many type-variable codes the language has. They are
// contiguous, CodeVarT through CodeVarT3, which is what lets a Code index a
// bindings slot directly.
const numTypeVars = int(CodeVarT3-CodeVarT) + 1

// bindings records what each type variable in a signature bound to.
//
// It is a fixed-slot VALUE rather than a map, and that is a hot-path decision
// rather than a style one. Every binary operator, unary operator, function call
// and property access funnels through matchShapesExactFirst, which scores each
// candidate row against its own fresh bindings — binaryShapes[OpAdd] alone has
// six rows, of which at most one survives — and a union parameter took two more
// per member for its scratch copies. As a map every one of those was a heap
// allocation, on an evaluator whose meter has counted 1.66M operations for a
// single large submission. Four slots and a set-mask answer the same three
// questions (bound?, to what, bind it) with no allocation at all: a scratch
// copy becomes a struct assignment and the whole match runs in the caller's
// frame.
//
// The mutating half of the matcher therefore takes *bindings; everything that
// only READS one (substitute, unresolvedResult, Shape.RetOf) still takes it by
// value, which is also what makes the returned match impossible to alias — the
// caller gets a copy, not a buffer the matcher may reuse.
//
// A FIFTH type-variable code means a fifth slot here. varIndex is the one place
// a Code becomes a slot and isTypeVar is defined in terms of it, so a code
// without one is simply not a type variable: it stops binding, visibly, rather
// than silently sharing another variable's slot.
type bindings struct {
	// set is a bit per slot, marking which variables are bound. A zero Type is
	// a legitimate binding (nulltype), so the slot alone cannot say.
	set  uint8
	vars [numTypeVars]Type
}

// varIndex is c's slot in a bindings, or -1 when c is not a type variable.
func varIndex(c Code) int {
	if c < CodeVarT || c > CodeVarT3 {
		return -1
	}
	return int(c - CodeVarT)
}

// get reports what the type variable c is bound to, and whether it is bound at
// all — the map index expression this replaced, with the same two results.
//
// The receiver is a pointer only to match bind's, which must be one; get
// mutates nothing. Every caller's bindings is addressable, including the
// by-value parameters of substitute and Shape.RetOf.
func (b *bindings) get(c Code) (Type, bool) {
	i := varIndex(c)
	if i < 0 || b.set&(1<<i) == 0 {
		return Type{}, false
	}
	return b.vars[i], true
}

// bind records c's binding, replacing any previous one. A code that is not a
// type variable is ignored: every caller has already established that it is one
// (isTypeVar), and there is no slot to record it in.
func (b *bindings) bind(c Code, t Type) {
	i := varIndex(c)
	if i < 0 {
		return
	}
	b.set |= 1 << i
	b.vars[i] = t
}

// Argument costs. A candidate's cost is the sum over its parameters, and the
// cheapest admissible candidate wins.
const (
	// costExact is an argument that already has the declared parameter type.
	costExact = 0
	// costWiden is an argument reaching its parameter by a conversion that
	// cannot fail on any value.
	costWiden = 1
)

// matchShapes selects the shape for a call with the given argument types, and
// returns the variable bindings the match produced. See the doc comment on
// argCost for how selection ranks candidates.
func matchShapes(shapes []Shape, args []Type) (Shape, bindings, bool) {
	return matchShapesExactFirst(shapes, args, false)
}

// matchShapesExactFirst is matchShapes with specification section 1.2.4's
// method-receiver restriction: when exactFirst is set, argument 0 must already
// have its parameter's type rather than reaching it by coercion.
//
// The restriction belongs to the CALL SITE, not the signature — the same
// function accepts a coerced first argument in function position and refuses one
// in method position — which is why Shape.Promote cannot express it.
//
// Selection is by COST, not by position. Each argument scores costExact when it
// already has the declared type, costWiden when it gets there by a conversion
// that can never fail, and is INADMISSIBLE when the only route is a conversion
// that can fail on some value. A candidate with any inadmissible argument is not
// a candidate at all; among the rest, the lowest total cost wins, and an exact
// tie breaks to the earliest shape.
//
// Two things fall out of that, and both are the point:
//
// An all-costExact candidate always beats a converting one, so "1 + 1" can never
// land on the (float, float) shape however the list is ordered — shape authors
// never have to reason about position.
//
// A conversion that can fail is never chosen to SELECT an overload. Section
// 1.2.3 does permit float -> int and string -> int, but only where a context
// demanded that type and is prepared for the value not to fit. Letting the
// matcher use them would make "1 + 2.5" pick (int, int) by discarding the .5,
// and would let "'a' + 'b'" match (int, int) by parsing both strings. Section
// 2.1.1 says the opposite happens: "the int is promoted to float and the float
// overload is used". Ranking with an inadmissible tier is what produces that,
// and it is not a separate promotion rule.
func matchShapesExactFirst(shapes []Shape, args []Type, exactFirst bool) (Shape, bindings, bool) {
	best := -1
	var bestShape Shape
	var bestBindings bindings
	for _, s := range shapes {
		if len(s.Params) != len(args) {
			continue
		}
		// A fresh zero value per candidate, in this frame: a losing candidate's
		// bindings must not be visible to the next one, and bestBindings takes a
		// COPY, so nothing the loop reuses can reach the caller.
		var b bindings
		cost, ok := shapeCostExactFirst(s, args, &b, exactFirst)
		if !ok {
			continue
		}
		// Strictly less, so an exact tie keeps the earliest shape.
		if best < 0 || cost < best {
			best, bestShape, bestBindings = cost, s, b
		}
	}
	if best < 0 {
		return Shape{}, bindings{}, false
	}
	return bestShape, bestBindings, true
}

// shapeCostExactFirst is shapeCost with the receiver restriction applied to
// argument 0.
//
// costExact is the test rather than "no coercion happened", and that is what
// makes a type-variable parameter still usable in method position: binding a
// variable scores costExact, so identity(x) works on any receiver, while a
// (string, string) signature refuses a path receiver because path -> string
// scores costWiden.
func shapeCostExactFirst(s Shape, args []Type, b *bindings, exactFirst bool) (int, bool) {
	total := 0
	for i := range s.Params {
		cost, ok := argCost(s.Params[i], args[i], b, s.Promote)
		if !ok {
			return 0, false
		}
		if i == 0 && exactFirst && cost != costExact {
			return 0, false
		}
		total += cost
	}
	return total, true
}

// argCost reports what it costs to pass an argument of type arg to a parameter
// declared as param, and whether it is admissible at all. pr narrows which
// conversions the enclosing shape accepts — see promotion.
func argCost(param, arg Type, b *bindings, pr promotion) (int, bool) {
	// A placeholder is matched on its constraint. Its lack of a value is
	// irrelevant to selecting a signature — which is what lets an expression be
	// type-checked before any parameter value exists.
	if c, ok := unresolvedConstraint(arg); ok {
		return argCost(param, c, b, pr)
	}
	if isTypeVar(param.Code) {
		return typeVarCost(param.Code, arg, b, pr)
	}
	// "any" accepts anything, but costs more than an exact match so that a
	// specific shape wins over a generic one when both fit.
	if param.Code == CodeAny {
		return costWiden, true
	}
	// Descend into a parameterized parameter so a variable inside it can bind —
	// list[T] against list[int] must bind T to int.
	if param.Code == CodeList && arg.Code == CodeList &&
		len(param.Params) == 1 && len(arg.Params) == 1 {
		return argCostList(param, arg, b, pr)
	}
	// A union parameter is scored member-wise, before the exact-match fallback
	// below: the union as a whole is never Equal to one of its own members, so
	// an exact member match (int against int | float | string) would otherwise
	// be missed entirely. This is also more precise than testing membership by
	// type code, which could not tell list[int] from list[string] inside the
	// union — argCost's own list-descent handles that correctly per member.
	if param.Code == CodeUnion {
		return unionArgCost(param, arg, b, pr)
	}
	// A union ARGUMENT is the dual case: it is SOME ONE of its members,
	// decided at runtime, so it is admissible only where EVERY member would
	// be — the caller must be prepared to pay for whichever member actually
	// shows up, so the cost is the worst of them rather than the best.
	if arg.Code == CodeUnion {
		return unionArgValueCost(param, arg, b, pr)
	}
	if param.Equal(arg) {
		return costExact, true
	}
	if promotableUnder(pr, arg, param) {
		return costWiden, true
	}
	return 0, false
}

// typeVarCost binds a type variable to an argument, or scores the argument
// against the type the variable already bound to.
//
// A variable binds once: seeing it a second time requires the same type…
// …with one exception, and it is the empty list literal's. Section 1.2.6 rule 6
// makes list[nulltype] "implicitly convertible to list[T] for any T", so a
// binding that came from an empty list names no type the caller actually chose
// — it is a placeholder for whatever the OTHER occurrence turns out to be.
// Holding it fixed made the shared-variable ordering shape (list[T], list[T])
// ORDER-DEPENDENT: "[1] < []" matched, because T was already int when
// argCostList's own empty-list branch declined to rebind it, while "[] < [1]"
// did not, because T had been pinned to nulltype first and int then mismatched.
// Section 1.2.5 says the shorter list is less, so both are answerable, and the
// reference implementation agrees ("[] < [1]" is true, "[1] < []" false).
//
// The exception has to be SYMMETRIC, and was not. One layer down, where the
// empty literal reaches this function as a plain argument rather than through
// argCostList's own branch, "[[]] < [[1]]" bound T to list[nulltype] and then
// replaced it with list[int], while "[[1]] < [[]]" bound T to list[int] and met
// list[nulltype] — the same pair, the same rule, and an "unsupported operand
// types" error for want of the second direction. The reference returns false
// for it, queried directly.
//
// There is a SECOND exception, and it belongs to ordering alone (pr,
// promoteOrdering): section 2.1.4's compatible pairs, applied elementwise. See
// orderingUnified below for the adjudication; the mechanism is that the two
// occurrences are unified rather than required equal, so the variable ends up
// naming the type BOTH operands reach — list[float] for "[1] < [1.0]" — and
// callShape then coerces each operand to it before compareLists runs. Nothing
// downstream has to learn about cross-type element comparison, because by the
// time it runs there is no longer a cross-type pair.
func typeVarCost(code Code, arg Type, b *bindings, pr promotion) (int, bool) {
	bound, ok := b.get(code)
	if !ok {
		b.bind(code, arg)
		return costExact, true
	}
	if bound.Equal(arg) {
		return costExact, true
	}
	// The empty side may be EITHER of the two, and the fix that made "[] < [1]"
	// work handled only the first of these. When the BINDING is the empty one it
	// is replaced, so the variable ends up naming the type the caller actually
	// chose. When the INCOMING ARGUMENT is the empty one the binding already
	// names that type, so it stands and the argument is merely admitted:
	// rebinding to it would throw the real element type away and pin the
	// variable to nulltype, which is what the first branch exists to undo.
	if emptyListBinding(bound, arg) {
		b.bind(code, arg)
		return costWiden, true
	}
	if emptyListBinding(arg, bound) {
		return costWiden, true
	}
	if unified, ok := orderingUnified(pr, bound, arg); ok {
		b.bind(code, unified)
		return costWiden, true
	}
	return 0, false
}

// orderingUnified answers section 2.1.4's compatible pairs applied ELEMENTWISE,
// for the shared element variable of the list ordering shape: given the type one
// operand's elements have and the type the other's have, the type both reach, or
// false when there is none.
//
// The adjudication, since this reverses a documented behavior. Section 1.2.5
// defines list ordering as "elements are compared pairwise from the start, and
// the first unequal pair determines the result" — it constrains the two lists'
// LENGTHS and nothing about their element types. Section 2.1.4 then says an
// ordering operator's operands "may differ for compatible pairs (int/float and
// string/path)". Composing the two, an elementwise comparison of an int against
// a float is an ordering comparison of a compatible pair, which section 2.1.4
// permits; so "[1] < [1.0]" is false, not an error. It was an error here, and
// that error was the artifact of an implementation choice rather than a reading
// of the spec: one shared type variable across both parameters, which requires
// EXACT equality by construction.
//
// unifyElemPair (list.go) is reused rather than restated because it already
// computes exactly this set — section 1.2.6's int/float and path/string rules,
// applied through nested lists — and section 1.2.6's compatible pairs are
// section 2.1.4's, named twice in the same spec. Anything outside it stays
// inadmissible, which is what keeps "['a'] < [1]" and "[1] < [[]]" reported as
// unsupported operand types rather than failing later inside coerce().
//
// The reference implementation agrees on the result ("[1] < [1.0]" is false
// there, "[1.0] < [1]" false, "[1] <= [1.0]" true), which is corroboration and
// not the reason; it disagrees with sqi in the other direction on the adjacent
// membership question ("2.0 in [1, 2, 3]" errors there and is true here, see
// test/oracle/baseline.txt), and the spec is what decides both.
func orderingUnified(pr promotion, bound, arg Type) (Type, bool) {
	if pr != promoteOrdering {
		return Type{}, false
	}
	return unifyElemPair(bound, arg)
}

// emptyListBinding reports whether a type variable's existing binding came from
// an empty list literal and may therefore be replaced by arg.
//
// The test is not simply "is the binding nulltype": a nested empty literal
// binds the variable to list[nulltype] ("[[]] < [[1]]" binds T to
// list[nulltype], then meets list[int]), so matching list layers are peeled off
// both sides until the bound side bottoms out at nulltype. Requiring arg to
// have a list layer wherever bound has one is what keeps this from being a
// license to rebind anything: "[[]] < [1]" leaves bound at list[nulltype] with
// arg at int, which is NOT convertible and stays inadmissible — otherwise the
// match would succeed and then fail in coerce(), turning a clean "unsupported
// operand types" into a coercion error.
func emptyListBinding(bound, arg Type) bool {
	if bound.Code == CodeNull {
		return true
	}
	boundElem, boundIsList := listElem(bound)
	argElem, argIsList := listElem(arg)
	if !boundIsList || !argIsList {
		return false
	}
	return emptyListBinding(boundElem, argElem)
}

// argCostList scores a list[T]-shaped argument against a list[T]-shaped
// parameter, both already confirmed single-parameter list types by argCost's
// caller. Split out to keep argCost itself under the repo's complexity cap.
func argCostList(param, arg Type, b *bindings, pr promotion) (int, bool) {
	// list[nulltype] is the empty-list literal's type (section 1.2.3) and is
	// compatible with any list type — promotable's own list branch already
	// says so — but descending elementwise below would instead ask whether
	// nulltype itself reaches the element type, which it never does
	// (isScalarCode excludes it deliberately: null coerces to nothing). That
	// seam is inert while B1 has no list shapes to match against, but the
	// first list[T] parameter B2 registers would reject "[]" outright
	// without this.
	if arg.Params[0].Code == CodeNull {
		// list[nulltype] vs list[nulltype] is an exact match that argCost's own
		// param.Equal(arg) would have caught had the list-descent above not
		// intercepted the pair first — argCost never reaches its own
		// param.Equal check for a list/list pair, because THIS function
		// decides list arguments unconditionally. Catching it here, before the
		// type-variable carve-out below, is what makes the dedicated
		// list[nulltype] row for min([])/max([]) actually win the tie against
		// list[int]/list[float] (both of which only reach nulltype's argument
		// by the costWiden fallthrough further down) rather than losing it to
		// whichever concrete-element row happens to be registered first.
		if param.Params[0].Code == CodeNull {
			return costExact, true
		}
		// When the parameter's element is itself a type variable (B2's
		// concatLists/repeatList shapes), it must still bind — otherwise
		// callShape later substitutes the UNBOUND variable into the param
		// type and coerces the empty-list argument to a bogus "list[T]"
		// value instead of leaving it list[nulltype], corrupting whatever
		// the shape's Fn inspects. Binding to nulltype is exactly what a
		// normal argCost(varT, nulltype, b) call would have done, and for
		// an UNBOUND variable the cost now matches too — see below. The
		// override is only for the other two paths this branch decides
		// without ever calling argCost: a variable already bound by another
		// occurrence is left alone here (the empty list is compatible with
		// whatever that binding resolved to, per the same empty-list rule),
		// and a concrete element type reaches this branch at all — both
		// score costWiden below rather than through a recursive call.
		if elem := param.Params[0]; isTypeVar(elem.Code) {
			if _, bound := b.get(elem.Code); !bound {
				b.bind(elem.Code, TNull)
				// costExact, not costWiden, and the difference is only
				// observable in METHOD position. matchShapesExactFirst
				// implements section 1.2.4 by requiring argument 0 to score
				// costExact, so a widening receiver is refused — and binding a
				// variable that has no binding yet CONVERTS NOTHING. There is
				// no implicit coercion here for section 1.2.4 to suppress, so
				// scoring it as one made "[].len()" fail while "len([])"
				// succeeded: one call, two syntaxes, two answers. The
				// reference implementation returns 0 for both.
				//
				// The carve-out is exactly "the variable is unbound". A
				// variable another argument already bound to a concrete type
				// is NOT being bound here — the empty list is reaching that
				// concrete type through section 1.2.6 rule 6, which is a real
				// conversion — so that case falls through to costWiden below,
				// as does a concrete element type.
				return costExact, true
			}
		}
		return costWiden, true
	}
	return argCost(param.Params[0], arg.Params[0], b, pr)
}

// unionArgValueCost scores a union-typed ARGUMENT against a (non-union)
// parameter: the dual of unionArgCost just above, which scores a union
// PARAMETER. Every member is scored, not just the cheapest, because the
// union's actual runtime value could be any one of them; the argument is
// admissible only if all of them are, at the cost of the most expensive.
//
// Each member is scored against its own scratch copy of the bindings
// accumulated so far, exactly as unionArgCost's members are. But unlike
// unionArgCost, every member's bindings must survive, not just a winner's:
// if two members would bind the same type variable to different types there
// is no single binding that is correct regardless of which member shows up
// at runtime, so the whole argument is inadmissible rather than picking one
// arbitrarily. Bindings the members agree on are merged back into b.
func unionArgValueCost(param, arg Type, b *bindings, pr promotion) (int, bool) {
	worst := 0
	merged := *b
	for _, member := range arg.Params {
		scratch := *b
		cost, ok := argCost(param, member, &scratch, pr)
		if !ok {
			return 0, false
		}
		if cost > worst {
			worst = cost
		}
		for i := range numTypeVars {
			code := CodeVarT + Code(i)
			t, bound := scratch.get(code)
			if !bound {
				continue
			}
			if existing, seen := merged.get(code); seen {
				if !existing.Equal(t) {
					return 0, false
				}
				continue
			}
			merged.bind(code, t)
		}
	}
	*b = merged
	return worst, true
}

// unionArgCost scores arg against every member of a union parameter and keeps
// the lowest-cost admissible member; the argument is inadmissible only if no
// member is.
//
// Each member is tried against its own scratch copy of the bindings
// accumulated so far, not the caller's b directly: trying member two must not
// see a variable binding left behind by a failed or losing attempt at member
// one. Only the winning member's bindings are folded back into b, so a type
// variable never ends up bound to a type from a member that did not win.
func unionArgCost(param, arg Type, b *bindings, pr promotion) (int, bool) {
	best := -1
	var bestBindings bindings
	for _, member := range param.Params {
		scratch := *b
		cost, ok := argCost(member, arg, &scratch, pr)
		if !ok {
			continue
		}
		if best < 0 || cost < best {
			best, bestBindings = cost, scratch
		}
	}
	if best < 0 {
		return 0, false
	}
	*b = bestBindings
	return best, true
}

// isTypeVar reports whether c is one of the type-variable codes, which appear
// only in signatures and never as the type of a value.
func isTypeVar(c Code) bool { return varIndex(c) >= 0 }

// substitute replaces every bound type variable in t with the type it bound to,
// rebuilding through the constructors so the result stays normalized. An unbound
// variable is left as it is.
func substitute(t Type, b bindings) Type {
	if isTypeVar(t.Code) {
		if bound, ok := b.get(t.Code); ok {
			return bound
		}
		return t
	}
	if len(t.Params) == 0 {
		return t
	}
	params := make([]Type, len(t.Params))
	for i, p := range t.Params {
		params[i] = substitute(p, b)
	}
	switch t.Code {
	case CodeList:
		return ListOf(params[0])
	case CodeUnion:
		return UnionOf(params...)
	case CodeUnresolved:
		return UnresolvedOf(params[0])
	}
	return Type{Code: t.Code, Params: params}
}
