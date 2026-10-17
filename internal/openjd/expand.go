// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
)

// ExpandParameterSpace materializes a step's parameter space into the concrete
// set of task parameter maps that the scheduler will use to create individual
// tasks.
//
// When ps is nil the step has a zero-dimensional parameter space: a single task
// is produced with an empty [TaskParams] map.
//
// The combination expression (if provided) controls how parameter ranges are
// joined:
//   - Identifiers separated by * form a Cartesian product (outer join).
//   - Identifiers grouped in parentheses (A,B,C) are zipped (inner join / association).
//     All parameters in a group must have the same range length.
//
// If no combination expression is provided, all parameters are joined with *.
//
// This entry point charges no job-wide budget ([maxTasksPerJob]): it expands
// one step, which [maxTasksPerStep] already bounds at the same number. The
// submission pipeline calls [expandParameterSpace] with a per-job budget,
// because only a caller that walks a whole template can see the sum.
func ExpandParameterSpace(ps *StepParameterSpace) ([]TaskParams, error) {
	return expandParameterSpace(ps, nil)
}

// expandParameterSpace is [ExpandParameterSpace] with an optional job-wide
// task budget. jobBudget may be nil, which means "this step is not part of a
// job being expanded" and skips the [maxTasksPerJob] check entirely.
//
// Both caps are charged before materialization, from countCombNode's
// arithmetic, so an over-cap step never allocates its rows.
func expandParameterSpace(ps *StepParameterSpace, jobBudget *jobTaskBudget) ([]TaskParams, error) {
	if ps == nil {
		if err := jobBudget.charge(1); err != nil {
			return nil, err
		}
		return []TaskParams{{}}, nil
	}

	// Build per-parameter value lists.
	paramValues := make(map[string][]string, len(ps.TaskParameterDefinitions))
	for _, tp := range ps.TaskParameterDefinitions {
		vals, err := expandTaskParam(tp)
		if err != nil {
			return nil, fmt.Errorf("openjd: parameter %q: %w", tp.Name, err)
		}
		paramValues[tp.Name] = vals
	}

	// Parse or construct the combination expression tree.
	var expr string
	if ps.Combination != nil {
		expr = *ps.Combination
	} else {
		// Default: all parameters joined with *.
		names := make([]string, 0, len(ps.TaskParameterDefinitions))
		for _, tp := range ps.TaskParameterDefinitions {
			names = append(names, tp.Name)
		}
		expr = strings.Join(names, "*")
	}

	// Handle the degenerate case of a single parameter with no operator.
	if expr == "" {
		if err := jobBudget.charge(1); err != nil {
			return nil, err
		}
		return []TaskParams{{}}, nil
	}

	tree, err := parseCombinationExpr(expr)
	if err != nil {
		return nil, fmt.Errorf("openjd: combination expression: %w", err)
	}

	// Resource-exhaustion guard: compute the task count arithmetically from the
	// tree before materializing the Cartesian product. This bounds both the
	// result allocation and the number of per-task DB inserts one submission can
	// trigger. Like the per-range maxRangeValues cap it always applies,
	// independent of EnforceLimits.
	count, ok := countCombNode(tree, paramValues)
	if !ok || count > maxTasksPerStep {
		return nil, fmt.Errorf(
			"openjd: parameter space expands to too many tasks (limit %d)", maxTasksPerStep,
		)
	}
	// The same count, charged against the whole job's allowance, also before
	// materialization: a step that fits on its own but not in what the job has
	// left must not build its rows either.
	if err := jobBudget.charge(count); err != nil {
		return nil, err
	}

	rows, err := evalCombNode(tree, paramValues)
	if err != nil {
		return nil, fmt.Errorf("openjd: combination expression: %w", err)
	}
	return rows, nil
}

// maxTasksPerStep is a hard resource-exhaustion bound on the number of tasks a
// single step's parameter space may expand to. Like [maxRangeValues] it always
// applies, even when EnforceLimits is false, because it guards against
// unbounded materialization and an unbounded number of per-task DB inserts
// reachable from POST /api/v1/jobs (e.g. two 1024-value parameters joined by a
// Cartesian product already yield ~1M tasks). It is deliberately generous; tune
// it if legitimate jobs need larger steps.
const maxTasksPerStep = 1_000_000

// maxTasksPerJob caps the tasks a whole job may produce, summed across its
// steps. Without it [maxSteps] (100) multiplies through [maxTasksPerStep], so
// one POST /api/v1/jobs could create 100 x 1,000,000 = 10^8 task rows with
// every other limit respected.
//
// It equals maxTasksPerStep: a job may not produce more tasks than a single
// step may. It therefore rejects some jobs the per-step cap alone would
// accept, such as two 600,000-task steps; tune it if legitimate jobs need
// larger totals.
//
// It always applies, even when EnforceLimits is false. With EnforceLimits
// false maxSteps is not enforced either, so without this cap the total has no
// ceiling at all.
const maxTasksPerJob = maxTasksPerStep

// jobTaskBudget is the running total of tasks one job's steps have produced so
// far, so [maxTasksPerJob] can be enforced across them. [Submitter.Submit]
// creates one per submission and threads it through every step's expansion.
//
// A nil *jobTaskBudget is valid and charges nothing. The exported
// [ExpandParameterSpace] passes nil, since a lone step has no job to be summed
// into.
type jobTaskBudget struct {
	// total is the number of tasks charged so far by this job's steps.
	total int
}

// charge adds count to the running total, reporting an error once the job
// passes [maxTasksPerJob]. Callers charge before materializing rows.
//
// The total cannot overflow: every caller has already rejected a count above
// maxTasksPerStep, and charge returns an error as soon as the total passes
// maxTasksPerJob, so total never exceeds 2 x maxTasksPerStep.
func (b *jobTaskBudget) charge(count int) error {
	if b == nil {
		return nil
	}
	b.total += count
	if b.total > maxTasksPerJob {
		return fmt.Errorf(
			"openjd: job expands to too many tasks across all steps (%d, limit %d)",
			b.total, maxTasksPerJob,
		)
	}
	return nil
}

// countCombNode computes, arithmetically, the number of [TaskParams] rows that
// [evalCombNode] would produce for tree, without materializing them. ok is false
// if the running product overflows, so callers can reject before allocating. A
// product node multiplies its children; an association (zip) node takes its
// first child's count, matching [zipRows] (a length mismatch is reported later
// by zipRows itself). An identifier not present in values counts as 0, leaving
// the real "undeclared parameter" error to [evalIdent].
func countCombNode(node combNode, values map[string][]string) (count int, ok bool) {
	switch n := node.(type) {
	case combIdent:
		return len(values[n.Name]), true
	case combProduct:
		total := 1
		for _, child := range n.Children {
			c, cok := countCombNode(child, values)
			if !cok {
				return 0, false
			}
			total, ok = mulSaturate(total, c)
			if !ok {
				return 0, false
			}
			if total > maxTasksPerStep {
				return total, true // already over the cap; no need to keep multiplying
			}
		}
		return total, true
	case combAssoc:
		if len(n.Children) == 0 {
			return 0, true
		}
		return countCombNode(n.Children[0], values)
	default:
		return 0, false
	}
}

// mulSaturate multiplies a and b, returning ok=false on int overflow.
func mulSaturate(a, b int) (product int, ok bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	product = a * b
	if product/b != a {
		return 0, false // overflowed
	}
	return product, true
}

// expandTaskParam expands a single task parameter definition into a flat list
// of string values, one per task.
//
// For CHUNK[INT] parameters each returned string is a "<start>-<end>" range
// (CONTIGUOUS) or a compact <IntRangeExpr> (NONCONTIGUOUS) encoding the group
// of integers that a single task will process.
func expandTaskParam(tp TaskParamDefinition) ([]string, error) {
	switch tp.Type {
	case TaskParamTypeInt:
		if tp.RangeExpr != nil {
			ints, err := parseIntRangeExpr(*tp.RangeExpr)
			if err != nil {
				return nil, err
			}
			out := make([]string, len(ints))
			for i, v := range ints {
				out[i] = strconv.Itoa(v)
			}
			return out, nil
		}
		return validateIntList(tp.RangeList)

	case TaskParamTypeChunkInt:
		return expandChunkInt(tp)

	case TaskParamTypeFloat:
		return validateFloatList(tp.RangeList)

	case TaskParamTypeString, TaskParamTypePath:
		if len(tp.RangeList) == 0 {
			return nil, errors.New("range list is empty")
		}
		return tp.RangeList, nil

	default:
		return nil, fmt.Errorf("unknown type %q", tp.Type)
	}
}

// expandChunkInt expands a CHUNK[INT] parameter into one string per chunk.
// Each string is either a "<start>-<end>" range (CONTIGUOUS) or a compact
// <IntRangeExpr> (NONCONTIGUOUS), matching the value a worker will receive as
// its task parameter.
func expandChunkInt(tp TaskParamDefinition) ([]string, error) {
	if tp.Chunks != nil && tp.Chunks.DefaultTaskCountExpr != nil {
		// Only ResolveParameterSpaceParams turns this into a count; chunking
		// without it would silently ignore the requested chunk size.
		return nil, fmt.Errorf("chunks.defaultTaskCount %q was not resolved before expansion",
			*tp.Chunks.DefaultTaskCountExpr)
	}
	// Resolve the full ordered integer list.
	var all []int
	if tp.RangeExpr != nil {
		var err error
		all, err = parseIntRangeExpr(*tp.RangeExpr)
		if err != nil {
			return nil, err
		}
	} else {
		for _, s := range tp.RangeList {
			n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid integer %q in range list", s)
			}
			all = append(all, int(n))
		}
	}

	if len(all) == 0 {
		return nil, errors.New("range produces no values")
	}

	chunkSize := 1
	contiguous := true
	if tp.Chunks != nil {
		if tp.Chunks.DefaultTaskCount > 0 {
			chunkSize = tp.Chunks.DefaultTaskCount
		}
		contiguous = tp.Chunks.RangeConstraint != "NONCONTIGUOUS"
	}
	return chunkValues(all, chunkSize, contiguous), nil
}

// chunkValues groups all into chunks of at most chunkSize values, in order.
//
// A CONTIGUOUS chunk is "always a contiguous range of integers" (TASK_CHUNKING,
// Template Schemas), so all is first split into runs of consecutive integers
// and each run is chunked on its own. A NONCONTIGUOUS chunk may be any set, so
// the flat list is chunked directly and each chunk spelled by
// formatNoncontiguousChunk.
//
// Each run is split into the fewest chunks that fit, with its values spread
// evenly across them, as the reference implementation does and the
// TASK_CHUNKING fixture contiguous-uneven.test.yaml requires: 1-12 at 5 is
// 1-4, 5-8, 9-12. RFC 0001's example, "1,10-12,18-50" at 10, is therefore
// "1-1", "10-12", "18-26", "27-34", "35-42", "43-50"; the RFC's own
// illustration, which does not balance, ends 18-27, ..., 48-50.
func chunkValues(all []int, chunkSize int, contiguous bool) []string {
	runs, format := [][]int{all}, formatNoncontiguousChunk
	if contiguous {
		runs, format = consecutiveRuns(all), formatContiguousChunk
	}
	var out []string
	for _, run := range runs {
		// k = ceil(n/chunkSize) chunks, chunk i ending at index
		// ceil((i+1)*n/k). k is computed as (n-1)/chunkSize+1 because
		// defaultTaskCount has no upper bound, and n+chunkSize-1 would wrap
		// at the largest INT and give zero chunks. n >= 1 always.
		n := len(run)
		k := (n-1)/chunkSize + 1
		start := 0
		for i := range k {
			end := ((i+1)*n + k - 1) / k
			out = append(out, format(run[start:end]))
			start = end
		}
	}
	return out
}

// consecutiveRuns splits vals, keeping their order, into maximal runs in which
// every value is the previous one plus one. vals must be non-empty.
func consecutiveRuns(vals []int) [][]int {
	var runs [][]int
	start := 0
	for i := 1; i <= len(vals); i++ {
		if i == len(vals) || vals[i] != vals[i-1]+1 {
			runs = append(runs, vals[start:i])
			start = i
		}
	}
	return runs
}

// formatContiguousChunk spells a run of consecutive integers as "<start>-<end>",
// including a single value ("5-5"), as RFC 0001 specifies. vals is always a
// run from consecutiveRuns, never a stepped or gapped set.
func formatContiguousChunk(vals []int) string {
	return strconv.Itoa(vals[0]) + "-" + strconv.Itoa(vals[len(vals)-1])
}

// formatNoncontiguousChunk spells a chunk as the compact <IntRangeExpr> the
// reference implementation produces, as the TASK_CHUNKING conformance fixtures
// require ("1-8" at 3 is "1-3", "4-6", "7,8"). Scanning left to right, the
// longest run with one constant positive step is written as a range when it
// holds at least three values ("1-5", "1-9:2"); a shorter run is written as its
// first two values, and the scan carries on after them. So a run of two stays
// a list ("7,8"), and "1,3,5,6,7,8" is "1-5:2,6-8".
//
// The values keep the order expansion gave them, which for a range such as
// "10-15:2,1-5" is first-seen rather than increasing. A step that is not
// positive never forms a range, because internal/openjd's own parser rejects
// "a-b" with a > b, so a descending or repeated pair is written value by value.
func formatNoncontiguousChunk(vals []int) string {
	var parts []string
	for i := 0; i < len(vals); {
		if i+1 == len(vals) {
			parts = append(parts, strconv.Itoa(vals[i]))
			break
		}
		step := vals[i+1] - vals[i]
		if step <= 0 {
			parts = append(parts, strconv.Itoa(vals[i]))
			i++
			continue
		}
		j := i + 1
		for j+1 < len(vals) && vals[j+1]-vals[j] == step {
			j++
		}
		switch {
		case j-i+1 < 3:
			parts = append(parts, strconv.Itoa(vals[i]), strconv.Itoa(vals[i+1]))
			i += 2
			continue
		case step == 1:
			parts = append(parts, strconv.Itoa(vals[i])+"-"+strconv.Itoa(vals[j]))
		default:
			parts = append(parts, strconv.Itoa(vals[i])+"-"+strconv.Itoa(vals[j])+":"+strconv.Itoa(step))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// DeriveChunkBounds adds "<name>.Start" and "<name>.End" keys to every row in
// rows for each CHUNK[INT] parameter declared in ps. Each chunk value is a
// contiguous integer range encoding (e.g. "1-10", "5-5"; a NONCONTIGUOUS-style
// list is also tolerated); Start is the smallest integer in the chunk and End
// the largest. It is a no-op when ps is nil or declares no CHUNK[INT]
// parameter, and the submission pipeline calls it only when the template
// enables the SQI_CHUNK_BOUNDS extension. A value that fails to parse is left
// without derived keys, since expansion has already validated the range.
func DeriveChunkBounds(rows []TaskParams, ps *StepParameterSpace) {
	if ps == nil {
		return
	}
	for _, tp := range ps.TaskParameterDefinitions {
		if tp.Type != TaskParamTypeChunkInt {
			continue
		}
		for _, row := range rows {
			v, ok := row[tp.Name]
			if !ok {
				continue
			}
			ints, err := parseIntRangeExpr(v)
			if err != nil || len(ints) == 0 {
				continue
			}
			lo, hi := ints[0], ints[0]
			for _, n := range ints[1:] {
				if n < lo {
					lo = n
				}
				if n > hi {
					hi = n
				}
			}
			row[tp.Name+".Start"] = strconv.Itoa(lo)
			row[tp.Name+".End"] = strconv.Itoa(hi)
		}
	}
}

// validateIntList checks that every element parses as an integer and returns
// the canonical string representation.
func validateIntList(items []string) ([]string, error) {
	if len(items) == 0 {
		return nil, errors.New("range list is empty")
	}
	out := make([]string, len(items))
	for i, s := range items {
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid integer %q", s)
		}
		out[i] = strconv.FormatInt(n, 10)
	}
	return out, nil
}

// validateFloatList checks that every element parses as a float and returns
// the canonical string representation.
func validateFloatList(items []string) ([]string, error) {
	if len(items) == 0 {
		return nil, errors.New("range list is empty")
	}
	out := make([]string, len(items))
	for i, s := range items {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid float %q", s)
		}
		out[i] = strconv.FormatFloat(f, 'g', -1, 64)
	}
	return out, nil
}

// ─── combination expression ───────────────────────────────────────────────────

// combNode is a node in the parsed combination expression tree.
type combNode interface {
	isCombNode()
}

// combIdent is a leaf node representing a single parameter name.
type combIdent struct{ Name string }

func (combIdent) isCombNode() {}

// combProduct is a product (Cartesian) node: children joined with *.
type combProduct struct{ Children []combNode }

func (combProduct) isCombNode() {}

// combAssoc is an association (zip) node: children grouped in parentheses.
type combAssoc struct{ Children []combNode }

func (combAssoc) isCombNode() {}

// parseCombinationExpr tokenizes and parses a combination expression.
//
// Grammar (OpenJD jobtemplate-2023-09, recursive):
//
//	<CombinationExpr> ::= <Element> | <CombinationExpr> '*' <Element>
//	<Element>         ::= <Identifier> | '(' <ExprList> ')'
//	<ExprList>        ::= <CombinationExpr> | <ExprList> ',' <CombinationExpr>
//	<Identifier>      ::= [A-Za-z_][A-Za-z0-9_]*
//
// i.e. a product is a sequence of Elements joined by '*'; an Element is either
// an identifier or a parenthesised, comma-separated list of full
// sub-expressions (an association group). An association group must contain at
// least two comma-separated expressions.
func parseCombinationExpr(expr string) (combNode, error) {
	p := &combParser{tokens: tokenizeComb(expr)}
	node, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if !p.done() {
		return nil, fmt.Errorf("unexpected token %q (token index %d)", p.peek(), p.pos)
	}
	return node, nil
}

// combinationIdentifiers parses an expression and returns the identifier list
// and the parsed tree, both for use by the validator. The list keeps
// duplicates; validateCombination reports them. The returned tree lets callers
// avoid a second parse.
func combinationIdentifiers(expr string) ([]string, combNode, error) {
	tree, err := parseCombinationExpr(expr)
	if err != nil {
		return nil, nil, err
	}
	return collectIdents(tree), tree, nil
}

func collectIdents(node combNode) []string {
	switch n := node.(type) {
	case combIdent:
		return []string{n.Name}
	case combProduct:
		var out []string
		for _, child := range n.Children {
			out = append(out, collectIdents(child)...)
		}
		return out
	case combAssoc:
		var out []string
		for _, child := range n.Children {
			out = append(out, collectIdents(child)...)
		}
		return out
	}
	return nil
}

// combParser is a simple recursive descent parser for combination expressions.
type combParser struct {
	tokens []string
	pos    int
	depth  int
}

func (p *combParser) done() bool { return p.pos >= len(p.tokens) }

func (p *combParser) peek() string {
	if p.done() {
		return ""
	}
	return p.tokens[p.pos]
}

func (p *combParser) consume() string {
	t := p.tokens[p.pos]
	p.pos++
	return t
}

// parseExpr parses a CombinationExpr: element ('*' element)*.
func (p *combParser) parseExpr() (combNode, error) {
	left, err := p.parseElement()
	if err != nil {
		return nil, err
	}

	if p.done() || p.peek() != "*" {
		return left, nil
	}

	children := []combNode{left}
	for !p.done() && p.peek() == "*" {
		p.consume() // consume '*'
		right, err := p.parseElement()
		if err != nil {
			return nil, err
		}
		children = append(children, right)
	}
	return combProduct{Children: children}, nil
}

// parseElement parses an Element: ident | '(' <ExprList> ')'.
func (p *combParser) parseElement() (combNode, error) {
	if p.done() {
		return nil, errors.New("unexpected end of expression")
	}
	if p.peek() == "(" {
		return p.parseAssocGroup()
	}
	// Plain identifier.
	name := p.consume()
	if !isIdentifier(name) {
		return nil, fmt.Errorf("expected parameter name, got %q", name)
	}
	return combIdent{Name: name}, nil
}

// parseAssocGroup parses '(' <ExprList> ')' into a combAssoc node, where
// <ExprList> is a comma-separated list of full CombinationExprs. An association
// group must contain at least two comma-separated expressions.
//
// A depth counter guards against adversarial deeply-nested input: nesting
// beyond 64 levels is rejected with an error rather than growing the stack.
func (p *combParser) parseAssocGroup() (combNode, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > 64 {
		return nil, errors.New("combination expression nested too deeply (max 64 levels)")
	}
	p.consume() // consume '('
	var children []combNode
	for {
		if p.done() {
			return nil, errors.New("unclosed parenthesis in combination expression")
		}
		if p.peek() == ")" || p.peek() == "," {
			return nil, fmt.Errorf("expected expression in group, got %q", p.peek())
		}
		child, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		children = append(children, child)

		if p.done() {
			return nil, errors.New("unclosed parenthesis in combination expression")
		}
		next := p.consume()
		if next == ")" {
			break
		}
		if next != "," {
			return nil, fmt.Errorf("expected ',' or ')' in group, got %q", next)
		}
	}
	if len(children) < 2 {
		return nil, errors.New("association group must contain at least two comma-separated expressions")
	}
	return combAssoc{Children: children}, nil
}

// tokenizeComb splits a combination expression into tokens:
// identifiers, '*', '(', ')', ','.  Whitespace is discarded.
func tokenizeComb(expr string) []string {
	var tokens []string
	i := 0
	for i < len(expr) {
		c := expr[i]
		switch c {
		case ' ', '\t':
			i++
		case '*', '(', ')', ',':
			tokens = append(tokens, string(c))
			i++
		default:
			// Identifier: [A-Za-z_][A-Za-z0-9_]*
			j := i
			for j < len(expr) && isIdentChar(expr[j]) {
				j++
			}
			if j == i {
				// Unknown character; include it as a token so the parser can
				// surface a useful error.
				tokens = append(tokens, string(c))
				i++
			} else {
				tokens = append(tokens, expr[i:j])
				i = j
			}
		}
	}
	return tokens
}

func isIdentChar(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
		(c >= '0' && c <= '9') || c == '_'
}

func isIdentifier(s string) bool {
	return identifierRE.MatchString(s)
}

// ─── evaluation ───────────────────────────────────────────────────────────────

// evalCombNode evaluates a parsed combination tree into a list of TaskParams.
func evalCombNode(node combNode, values map[string][]string) ([]TaskParams, error) {
	switch n := node.(type) {
	case combIdent:
		return evalIdent(n.Name, values)

	case combProduct:
		// Cartesian product of all children.
		result := []TaskParams{{}}
		for _, child := range n.Children {
			childRows, err := evalCombNode(child, values)
			if err != nil {
				return nil, err
			}
			result = cartesianProduct(result, childRows)
		}
		return result, nil

	case combAssoc:
		// Zip: all children must have the same length.
		childResults := make([][]TaskParams, len(n.Children))
		for i, child := range n.Children {
			rows, err := evalCombNode(child, values)
			if err != nil {
				return nil, err
			}
			childResults[i] = rows
		}
		return zipRows(childResults)

	default:
		return nil, fmt.Errorf("unknown combination node type %T", node)
	}
}

// evalIdent returns one TaskParams per value in the named parameter's range.
func evalIdent(name string, values map[string][]string) ([]TaskParams, error) {
	vals, ok := values[name]
	if !ok {
		return nil, fmt.Errorf("undeclared parameter %q in combination expression", name)
	}
	rows := make([]TaskParams, len(vals))
	for i, v := range vals {
		rows[i] = TaskParams{name: v}
	}
	return rows, nil
}

// cartesianProduct returns the Cartesian product of a and b.
// Each row in the result merges one row from a with one row from b.
func cartesianProduct(a, b []TaskParams) []TaskParams {
	out := make([]TaskParams, 0, len(a)*len(b))
	for _, ra := range a {
		for _, rb := range b {
			merged := make(TaskParams, len(ra)+len(rb))
			maps.Copy(merged, ra)
			maps.Copy(merged, rb)
			out = append(out, merged)
		}
	}
	return out
}

// zipRows zips multiple equal-length row slices into one.
// All slices must have the same length.
func zipRows(groups [][]TaskParams) ([]TaskParams, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	n := len(groups[0])
	for i, g := range groups[1:] {
		if len(g) != n {
			return nil, fmt.Errorf(
				"association group length mismatch: group 0 has %d values, group %d has %d values",
				n, i+1, len(g),
			)
		}
	}

	out := make([]TaskParams, n)
	for i := range n {
		merged := make(TaskParams)
		for _, g := range groups {
			maps.Copy(merged, g[i])
		}
		out[i] = merged
	}
	return out, nil
}
