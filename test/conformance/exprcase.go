// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/openjd/expr"
)

// RunExprCase scores an EXPR fixture on whether every expression it embeds
// parses, and is the scoring path for EXPR/job_templates until sqi registers
// the extension for real.
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
// It is parse-only. A fixture that is invalid for a SEMANTIC reason — a type
// error, an evaluation limit, an int64 overflow — parses fine, is accepted,
// and therefore fails and is baselined. That is the intended reporting: a
// visible failure rather than silence.
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
		res.Accepted = true
		for _, src := range exprs {
			e, perr := expr.Parse(src)
			if perr != nil {
				res.Accepted = false
				res.Reason = fmt.Sprintf("expression rejected: {{ %s }}: %v", src, perr)
				break
			}
			// An expression referencing no symbols can be evaluated with no
			// symbol table, which catches the errors a parse cannot see:
			// int64 overflow, division by zero, and the like. Evaluating one
			// that DOES reference a symbol would report "unknown symbol" and
			// wrongly reject a valid fixture, so the check is gated on Names.
			//
			// The target is Any: a fixture's expression has no field context
			// here, so it is evaluated for its natural result type rather than
			// against a constraint the fixture never stated.
			if len(e.Names()) > 0 {
				continue
			}
			if _, eerr := e.Eval(nil, expr.TAny); eerr != nil {
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
