// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/openjd/expr"
)

// RunExprCase scores an EXPR fixture on whether every expression it embeds
// parses and evaluates cleanly, against a symbol table built from the
// fixture's own declared parameter types. It is the scoring path for
// EXPR/job_templates until sqi registers the extension for real.
//
// # Why this exists, and when to delete it
//
// The template path (RunCase) runs a fixture through openjd.Parse and
// openjd.ValidateWithOptions, which reject any template declaring an
// unregistered extension. EXPR is unregistered, so every EXPR fixture is
// rejected for that reason alone — and since 180 of the suite's 209 EXPR
// job_templates fixtures are marked ".invalid", naive pass/fail scoring would
// report 180 passes-for-the-wrong-reason. Classify therefore reports every
// EXPR fixture as StateNotApplicable, which is correct but means the suite is
// silent about EXPR while the extension is being built.
//
// This path breaks that silence without inventing false greens: it reads the
// fixture's expressions directly and asks whether the expression reader
// accepts them. A fixture whose expressions all parse is accepted; one with a
// syntax error is rejected. Nothing here depends on internal/openjd, so the
// production rejection of EXPR templates stays correct throughout.
//
// Every expression is both parsed AND evaluated, against expr.TAny and a
// symbol table DeclaredSymbols builds from what the fixture itself declares:
// every symbol section 1.2.2 defines is bound as an UNRESOLVED placeholder of
// its declared type, and every name introduced by a "let:" block is bound
// UNTYPED (expr.TAny), since this path does not track "let" scoping or
// evaluate a binding's right-hand side to learn its real type. That is enough
// to catch a type error, an int64 overflow, a division by zero, or an
// unknown symbol in ANY expression, symbolic or not — not only a literal-only
// one. A fixture invalid for a semantic reason a placeholder can catch is
// rejected and passes; one invalid for a reason this path cannot see (a
// runtime-only condition, or a "let" binding whose real type would have
// caught it) still parses and evaluates fine, is accepted, and fails —
// visibly baselined rather than silently passed.
//
// TestConformance_EXPRNotRegistered fails the build the moment
// internal/openjd registers EXPR. At that point this file and its baseline
// must be deleted and EXPR/job_templates left to the template path, which will
// then score it end to end.
func RunExprCase(tc TestCase, data []byte) Result {
	res := Result{Case: tc, State: StateLive}

	exprs, err := ExtractExpressions(data)
	switch {
	case err != nil:
		res.Reason = fmt.Sprintf("parse rejected: %v", err)
	default:
		// Bind every symbol the fixture declares as a placeholder of its
		// declared type. DeclaredSymbols fails only when the document will not
		// parse as YAML at all — in which case it declares nothing and the
		// extraction step above has already reported it, so an empty table is
		// the right fallback rather than a second error. An unrecognized
		// parameter TYPE never gets here: it binds the name as "any", because
		// leaving a declared name unbound would reject a valid fixture.
		syms, serr := DeclaredSymbols(data)
		if serr != nil {
			syms = expr.MapSymbols{}
		}
		res.Accepted = true
		for _, src := range exprs {
			e, perr := expr.Parse(src)
			if perr != nil {
				res.Accepted = false
				res.Reason = fmt.Sprintf("expression rejected: {{ %s }}: %v", src, perr)
				break
			}
			// Every expression is evaluated now, not only the symbol-free ones:
			// a declared symbol resolves to a placeholder, so an unbound name is
			// a genuine "unknown symbol" rather than an artifact of an empty
			// table. The target is Any — a fixture's expression has no field
			// context, so it is checked for its natural result type.
			if _, eerr := e.Eval(syms, expr.TAny); eerr != nil {
				res.Accepted = false
				res.Reason = fmt.Sprintf("expression rejected: {{ %s }}: %v", src, eerr)
				break
			}
		}
	}

	res.Passed = res.Accepted != tc.Invalid
	switch {
	case res.Passed:
		res.Reason = ""
	case res.Accepted:
		res.Reason = fmt.Sprintf(
			"all %d expressions parsed, but fixture is marked .invalid", len(exprs),
		)
	}
	return res
}

// ExtractExpressions returns the body of every "{{ ... }}" reference in a
// template document, in document order, with surrounding whitespace trimmed.
//
// The document is walked as a yaml.Node tree rather than decoded into Go maps,
// so ordering is the document's own rather than a map-iteration accident, and
// mappings, sequences and aliases all walk identically.
func ExtractExpressions(doc []byte) ([]string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(doc, &root); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	var out []string
	collectExpressions(&root, &out)
	return out, nil
}

func collectExpressions(n *yaml.Node, out *[]string) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode {
		*out = append(*out, expressionsIn(n.Value)...)
		return
	}
	for _, child := range n.Content {
		collectExpressions(child, out)
	}
}

// expressionsIn returns the body of every "{{ ... }}" reference in s.
//
// Each body runs to the FIRST "}}" after its "{{", which is what
// internal/openjd/fmtstring does for a closed reference. An unclosed "{{"
// diverges from production: fmtstring.parse raises a MalformedError on a
// genuinely unclosed reference, while this path instead treats the remaining
// text as a candidate expression body. Reporting it lets expr.Parse reject it,
// whereas skipping it would drop the fixture from scoring silently.
func expressionsIn(s string) []string {
	var out []string
	for {
		start := strings.Index(s, "{{")
		if start < 0 {
			return out
		}
		rest := s[start+2:]
		before, after, found := strings.Cut(rest, "}}")
		if !found {
			return append(out, strings.TrimSpace(rest))
		}
		out = append(out, strings.TrimSpace(before))
		s = after
	}
}

// DeclaredSymbols builds the symbol table a fixture's expressions are checked
// against, binding every symbol section 1.2.2 defines as an UNRESOLVED value of
// its declared type. A fixture declares types, never values, so a placeholder is
// all there is to bind — and it is enough to catch a type error.
//
// Coverage of section 1.2.2's families is load-bearing. Being strict is the
// point: an unbound name must be rejected, or an undefined-symbol fixture could
// never fail. The cost of that strictness is that a MISSING family turns a valid
// fixture into a false failure, which is why every family is asserted by
// TestDeclaredSymbols_CoversEverySection122Family.
func DeclaredSymbols(doc []byte) (expr.MapSymbols, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(doc, &root); err != nil {
		return nil, err
	}
	syms := expr.MapSymbols{}
	fixedSymbols(syms)
	jobParamSymbols(&root, syms)
	stepSymbols(&root, syms)
	letSymbols(&root, syms)
	return syms, nil
}

// fixedSymbols binds the symbols section 1.2.2 defines regardless of what the
// template declares.
func fixedSymbols(syms expr.MapSymbols) {
	syms["Job.Name"] = expr.Unresolved(expr.TString)
	syms["Step.Name"] = expr.Unresolved(expr.TString)
	syms["Session.WorkingDirectory"] = expr.Unresolved(expr.TPath)
	syms["Session.PathMappingRulesFile"] = expr.Unresolved(expr.TPath)
	syms["Session.HasPathMappingRules"] = expr.Unresolved(expr.TBool)
}

// jobParamSymbols binds Param.<name> and RawParam.<name> for every declared job
// parameter. PATH and LIST[PATH] differ between the two: the raw form is a
// string because the value may be a path for another operating system that
// cannot be parsed locally.
func jobParamSymbols(root *yaml.Node, syms expr.MapSymbols) {
	for _, def := range mappingSeq(root, "parameterDefinitions") {
		name := scalarField(def, "name")
		declared := scalarField(def, "type")
		if name == "" || declared == "" {
			continue
		}
		paramType, rawType := jobParamTypes(declared)
		syms["Param."+name] = expr.Unresolved(paramType)
		syms["RawParam."+name] = expr.Unresolved(rawType)
	}
}

// jobParamTypes maps a declared job-parameter type to the expression types of
// Param.<name> and RawParam.<name>, per section 1.2.2's job-parameter table.
//
// An unrecognized spelling yields "any" rather than an error, and the name is
// still bound. That matters: this path's whole value is that an UNBOUND name is
// rejected, so failing to map one type must not leave the name unbound and turn
// a valid fixture into a false failure. All twelve spellings the current fixtures
// use are covered; "any" is the safe floor for a thirteenth.
func jobParamTypes(declared string) (paramType, rawType expr.Type) {
	// PATH and LIST[PATH] are the two rows where the raw form differs: it is a
	// string because the value may be a path for another operating system that
	// cannot be parsed locally.
	switch declared {
	case "PATH":
		return expr.TPath, expr.TString
	case "LIST[PATH]":
		return expr.ListOf(expr.TPath), expr.ListOf(expr.TString)
	}
	t, err := expr.ParseType(declaredTypeText(declared))
	if err != nil {
		return expr.TAny, expr.TAny
	}
	return t, t
}

// declaredTypeText rewrites a template's declared type spelling into the
// expression language's own notation: STRING becomes string, LIST[INT] becomes
// list[int], and so on, so that ParseType can read it.
func declaredTypeText(declared string) string {
	return strings.ToLower(declared)
}

// stepSymbols binds the per-step symbols: task parameters and the embedded files
// of a step's script. Section 1.2.2 gives CHUNK[INT] the type range_expr, not
// list[int], so that a frame range need not be expanded.
func stepSymbols(root *yaml.Node, syms expr.MapSymbols) {
	for _, step := range mappingSeq(root, "steps") {
		space := mappingField(step, "parameterSpace")
		for _, def := range mappingSeq(space, "taskParameterDefinitions") {
			name := scalarField(def, "name")
			declared := scalarField(def, "type")
			if name == "" || declared == "" {
				continue
			}
			t := taskParamType(declared)
			syms["Task.Param."+name] = expr.Unresolved(t)
			syms["Task.RawParam."+name] = expr.Unresolved(t)
		}
		script := mappingField(step, "script")
		for _, file := range mappingSeq(script, "embeddedFiles") {
			if name := scalarField(file, "name"); name != "" {
				syms["Task.File."+name] = expr.Unresolved(expr.TPath)
				syms["Env.File."+name] = expr.Unresolved(expr.TPath)
			}
		}
	}
}

// taskParamType maps a declared task-parameter type per section 1.2.2's task
// table. CHUNK[INT] is a range_expr, NOT a list[int], so that a frame range need
// not be expanded. An unrecognized spelling yields "any", for the same reason
// jobParamTypes does.
func taskParamType(declared string) expr.Type {
	if declared == "CHUNK[INT]" {
		return expr.TRangeExpr
	}
	t, err := expr.ParseType(declaredTypeText(declared))
	if err != nil {
		return expr.TAny
	}
	return t
}

// letSymbols binds every name introduced by a "let:" block anywhere in the
// document — a Template Schemas section 3.6.2 scoping construct, entirely
// outside section 1.2.2, whose entries have the form "name = expression"
// at <StepTemplate>.let, <StepScript>.let, <SimpleAction>.let, and a job
// environment's own script.let.
//
// The walk is scope-blind on purpose: it does not track which "let" block a
// name belongs to, or whether a reference to it appears somewhere that name
// is actually in scope (a job-environment "let" referencing Step.Name, for
// example, is invalid for a scoping reason this path does not enforce). Real
// "let" scoping — and evaluating a binding's right-hand expression to learn
// its real type — belongs to sub-project E (template integration), which has
// the scope information this conformance path does not. Binding the name
// UNTYPED here is only enough to avoid the false rejection of a valid
// fixture: leaving a declared "let" name unbound would reject it outright,
// while binding it as expr.TAny keeps it distinguishable from a name that is
// genuinely undeclared, so expr1.1--unknown-variable.invalid.yaml still
// burns down.
func letSymbols(n *yaml.Node, syms expr.MapSymbols) {
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "let" {
				bindLetEntries(n.Content[i+1], syms)
			}
		}
	}
	for _, child := range n.Content {
		letSymbols(child, syms)
	}
}

// bindLetEntries binds each "name = expression" entry of a "let:" sequence,
// skipping an entry with no "=" or an empty name rather than binding
// something malformed.
func bindLetEntries(seq *yaml.Node, syms expr.MapSymbols) {
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return
	}
	for _, item := range seq.Content {
		if item.Kind != yaml.ScalarNode {
			continue
		}
		name, _, found := strings.Cut(item.Value, "=")
		if !found {
			continue
		}
		if name = strings.TrimSpace(name); name != "" {
			syms[name] = expr.Unresolved(expr.TAny)
		}
	}
}

// mappingField returns the value node of a mapping's named key, or nil.
func mappingField(n *yaml.Node, key string) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		return mappingField(n.Content[0], key)
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// mappingSeq returns the items of a named sequence field, or nothing.
func mappingSeq(n *yaml.Node, key string) []*yaml.Node {
	f := mappingField(n, key)
	if f == nil || f.Kind != yaml.SequenceNode {
		return nil
	}
	return f.Content
}

// scalarField returns a mapping's named scalar value, or "".
func scalarField(n *yaml.Node, key string) string {
	f := mappingField(n, key)
	if f == nil || f.Kind != yaml.ScalarNode {
		return ""
	}
	return f.Value
}
