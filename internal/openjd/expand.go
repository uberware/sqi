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
func ExpandParameterSpace(ps *StepParameterSpace) ([]TaskParams, error) {
	if ps == nil {
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
		return []TaskParams{{}}, nil
	}

	tree, err := parseCombinationExpr(expr)
	if err != nil {
		return nil, fmt.Errorf("openjd: combination expression: %w", err)
	}

	rows, err := evalCombNode(tree, paramValues)
	if err != nil {
		return nil, fmt.Errorf("openjd: combination expression: %w", err)
	}
	return rows, nil
}

// expandTaskParam expands a single task parameter definition into a flat list
// of string values, one per task.
//
// For CHUNK[INT] parameters each returned string is a range expression
// (CONTIGUOUS) or comma-separated list (NONCONTIGUOUS) encoding the group of
// integers that a single task will process.
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
// Each string is either a compact range expression (CONTIGUOUS) or a
// comma-separated integer list (NONCONTIGUOUS), matching the value a worker
// will receive as its task parameter.
func expandChunkInt(tp TaskParamDefinition) ([]string, error) {
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

	var out []string
	for i := 0; i < len(all); i += chunkSize {
		end := min(i+chunkSize, len(all))
		chunk := all[i:end]
		if contiguous {
			out = append(out, formatContiguousChunk(chunk))
		} else {
			out = append(out, formatNoncontiguousChunk(chunk))
		}
	}
	return out, nil
}

// formatContiguousChunk encodes a chunk as a compact range expression.
// E.g. [1,2,3,4] → "1-4", [1,3,5] → "1-5:2", [1,2,5] → "1-2,5".
func formatContiguousChunk(vals []int) string {
	if len(vals) == 1 {
		return strconv.Itoa(vals[0])
	}
	// Try to express as one or more sub-ranges.
	type segment struct{ start, end, step int }
	var segs []segment
	start := vals[0]
	step := vals[1] - vals[0]
	end := vals[0]

	for i := 1; i < len(vals); i++ {
		diff := vals[i] - vals[i-1]
		if diff == step {
			end = vals[i]
		} else {
			segs = append(segs, segment{start, end, step})
			start = vals[i]
			if i+1 < len(vals) {
				step = vals[i+1] - vals[i]
			} else {
				step = 1
			}
			end = vals[i]
		}
	}
	segs = append(segs, segment{start, end, step})

	parts := make([]string, len(segs))
	for i, seg := range segs {
		switch {
		case seg.start == seg.end:
			parts[i] = strconv.Itoa(seg.start)
		case seg.step == 1:
			parts[i] = strconv.Itoa(seg.start) + "-" + strconv.Itoa(seg.end)
		default:
			parts[i] = strconv.Itoa(seg.start) + "-" + strconv.Itoa(seg.end) +
				":" + strconv.Itoa(seg.step)
		}
	}
	return strings.Join(parts, ",")
}

// formatNoncontiguousChunk encodes a chunk as a comma-separated integer list.
func formatNoncontiguousChunk(vals []int) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ",")
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
// Grammar:
//
//	expr   ::= term ('*' term)*
//	term   ::= ident | '(' ident (',' ident)+ ')'
//	ident  ::= [A-Za-z_][A-Za-z0-9_]*
func parseCombinationExpr(expr string) (combNode, error) {
	p := &combParser{tokens: tokenizeComb(expr)}
	node, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if !p.done() {
		return nil, fmt.Errorf("unexpected token %q at position %d", p.peek(), p.pos)
	}
	return node, nil
}

// combinationIdentifiers parses an expression and returns the identifier list
// for validation purposes.  Duplicates are allowed by this function (the
// validator checks for them separately via containment checks).
func combinationIdentifiers(expr string) ([]string, error) {
	tree, err := parseCombinationExpr(expr)
	if err != nil {
		return nil, err
	}
	return collectIdents(tree), nil
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

// parseExpr parses term ('*' term)*.
func (p *combParser) parseExpr() (combNode, error) {
	left, err := p.parseTerm()
	if err != nil {
		return nil, err
	}

	if p.done() || p.peek() != "*" {
		return left, nil
	}

	children := []combNode{left}
	for !p.done() && p.peek() == "*" {
		p.consume() // consume '*'
		right, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		children = append(children, right)
	}
	return combProduct{Children: children}, nil
}

// parseTerm parses: ident | '(' ident (',' ident)+ ')'.
func (p *combParser) parseTerm() (combNode, error) {
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

// parseAssocGroup parses '(' ident (',' ident)+ ')' into a combAssoc node.
func (p *combParser) parseAssocGroup() (combNode, error) {
	p.consume() // consume '('
	var children []combNode
	for {
		if p.done() {
			return nil, errors.New("unexpected end of expression inside group")
		}
		name := p.consume()
		if !isIdentifier(name) {
			return nil, fmt.Errorf("expected parameter name, got %q", name)
		}
		children = append(children, combIdent{Name: name})
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
		return nil, errors.New("association group must contain at least two parameters")
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
