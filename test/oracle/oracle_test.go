// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build oracle

// Package oracle differential-tests sqi's Go expression evaluator against the
// OpenJD reference implementation.
//
// Build-tagged so it never runs in `make test`: it needs a Python interpreter
// with a pinned third-party package, which is a development convenience and
// must not become a condition for building or testing sqi. Run it with
// `make test-expr-oracle`.
//
// See doc.go for what this proves, what it does not, and why a divergence is a
// question rather than a verdict.
package oracle

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/openjd/expr"
)

// oracleTimeout bounds the whole corpus, not one case. The reference evaluates
// a scalar expression in microseconds; anything approaching this means the
// subprocess is wedged rather than slow.
const oracleTimeout = 2 * time.Minute

// caseResult is one side's outcome for one case. The two sides are compared
// through this shape so the comparison reads identically in both directions.
type caseResult struct {
	ok    bool
	value string
	typ   string
	err   string
	ops   int64
}

// oracleReply is the reference implementation's JSON reply. Its field names
// are the wire contract with scripts/expr-oracle.py.
type oracleReply struct {
	Banner  bool   `json:"banner"`
	Version string `json:"version"`
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Value   string `json:"value"`
	Type    string `json:"type"`
	Error   string `json:"error"`
	Ops     int64  `json:"ops"`
}

// exprCase is one corpus entry.
type exprCase struct {
	id     string
	target string
	src    string
	line   int
}

func TestExprOracle_MatchesReferenceImplementation(t *testing.T) {
	root := repoRoot(t)
	python := findPython(t, root)

	cases := loadCorpus(t, filepath.Join(root, "test", "oracle", "corpus.txt"))
	if len(cases) == 0 {
		t.Fatal("corpus is empty; expected the checked-in cases")
	}
	baseline := loadBaseline(t, filepath.Join(root, "test", "oracle", "baseline.txt"))
	opsBaseline := loadBaseline(t, filepath.Join(root, "test", "oracle", "baseline-ops.txt"))

	version, refs := runOracle(t, root, python, cases)
	t.Logf("reference implementation: openjd-model %s (%d cases)", version, len(cases))
	assertPinnedVersion(t, version)

	var diverged, expected int
	var compared, opsDiverged, opsExpected int
	seen := make(map[string]bool, len(cases))
	opsSeen := make(map[string]bool, len(cases))

	for _, c := range cases {
		ref, ok := refs[c.id]
		if !ok {
			t.Errorf("corpus.txt:%d: the reference returned no result for %q", c.line, c.id)
			continue
		}
		got := evalGo(c)
		reason, isBaselined := baseline[c.id]
		seen[c.id] = true

		if got.ok {
			// Section 1.3.9's zero-balance invariant, asserted across the whole
			// corpus: after a top-level evaluation the only live value is the
			// result. A larger number is a missed release, a smaller one a
			// double release.
			if live, want, berr := expr.EvalForBalanceCheck(c.src, mustTarget(t, c.target)); berr == nil && live != want {
				t.Errorf("corpus.txt:%d: %s\n  live memory after eval = %d; want %d (the result's own size)",
					c.line, c.id, live, want)
			}
		}

		if agree(got, ref) {
			if isBaselined {
				// A baselined divergence that stopped diverging. Left as a
				// failure rather than tolerated: the baseline is a list of
				// open questions, and one answering itself is exactly the
				// event worth surfacing.
				t.Errorf("corpus.txt:%d: %s\n  no longer diverges — remove it from baseline.txt\n  baselined reason: %s",
					c.line, c.id, reason)
			}
			// Counts are compared ONLY here, on cases whose values already agree.
			// Stacking a count divergence onto a value divergence would report one
			// root cause twice and make both harder to read.
			compared++
			if got.ops != ref.ops {
				opsDiverged++
				opsReason, opsBaselined := opsBaseline[c.id]
				opsSeen[c.id] = true
				if opsBaselined {
					opsExpected++
					t.Logf("known count divergence: %s\n  go=%d ref=%d\n  %s", c.id, got.ops, ref.ops, opsReason)
				} else {
					t.Errorf("corpus.txt:%d: NEW OPERATION-COUNT DIVERGENCE\n  expression: %s\n  target:     %s\n"+
						"  go:         %d operations\n  reference:  %d operations (openjd-model %s)\n"+
						"  Section 1.3.10 specifies the counting rules, so this is comparable. Adjudicate\n"+
						"  against the spec, which outranks the reference. If sqi is right, add this case\n"+
						"  to baseline-ops.txt with the reasoning.",
						c.line, c.src, c.target, got.ops, ref.ops, version)
				}
			} else if _, opsBaselined := opsBaseline[c.id]; opsBaselined {
				opsSeen[c.id] = true
				t.Errorf("corpus.txt:%d: %s\n  operation counts no longer diverge — remove it from baseline-ops.txt",
					c.line, c.id)
			}
			continue
		}

		diverged++
		if isBaselined {
			expected++
			t.Logf("known divergence: %s\n  go=%s ref=%s\n  %s", c.id, got, ref, reason)
			continue
		}
		t.Errorf("corpus.txt:%d: NEW DIVERGENCE\n  expression: %s\n  target:     %s\n  go:         %s\n  reference:  %s (openjd-model %s)\n"+
			"  Investigate against the spec, which outranks the reference. If sqi is\n"+
			"  right, add this case to baseline.txt with the reasoning.",
			c.line, c.src, c.target, got, ref, version)
	}

	for id, reason := range baseline {
		if !seen[id] {
			t.Errorf("baseline.txt lists %q, which is not in corpus.txt\n  reason given: %s", id, reason)
		}
	}
	for id, reason := range opsBaseline {
		if !opsSeen[id] {
			t.Errorf("baseline-ops.txt lists %q, which either is not in corpus.txt or does not diverge on count\n  reason given: %s", id, reason)
		}
	}
	t.Logf("%d/%d agree; %d diverge (%d baselined)", len(cases)-diverged, len(cases), diverged, expected)
	t.Logf("operation counts: %d/%d agree on the %d value-agreeing cases; %d diverge (%d baselined)",
		compared-opsDiverged, compared, compared, opsDiverged, opsExpected)
}

// agree reports whether the two sides produced the same outcome.
//
// Two failures agree without comparing messages: both implementations are
// required to REFUSE the same expressions, but nothing requires them to phrase
// the refusal alike, and asserting on wording would turn every upstream
// message tweak into a failure here.
func agree(got, ref caseResult) bool {
	if got.ok != ref.ok {
		return false
	}
	if !got.ok {
		return true
	}
	return got.value == ref.value && got.typ == ref.typ
}

func (r caseResult) String() string {
	if !r.ok {
		return "error(" + r.err + ")"
	}
	return fmt.Sprintf("%s : %s", r.value, r.typ)
}

// evalGo evaluates one case with sqi's evaluator, mapping both failure modes —
// an unparseable target type and a failed evaluation — onto the same error
// outcome the Python driver reports, so one comparison rule covers both.
func evalGo(c exprCase) caseResult {
	target, err := expr.ParseType(c.target)
	if err != nil {
		return caseResult{err: err.Error()}
	}
	v, ops, err := expr.EvalWithMetrics(c.src, nil, target)
	if err != nil {
		return caseResult{err: err.Error()}
	}
	return caseResult{ok: true, value: v.String(), typ: v.Type.String(), ops: ops}
}

// assertPinnedVersion fails when the reference implementation that actually
// answered is not the one the Makefile pins.
//
// It exists because the pin was, until 2026-08-19, advisory in exactly the case
// that matters: test-expr-oracle creates .venv-oracle only when it is MISSING,
// so raising OPENJD_MODEL_VERSION against an existing venv changed nothing and
// the suite went on grading sqi against the old reference. Nothing failed. The
// version was already logged, which is how the mismatch was eventually noticed
// — by reading the log, not by a red test, which is the wrong way round for a
// harness whose whole purpose is to be exact about which build produced a
// divergence.
//
// The expectation is supplied by the Makefile rather than duplicated here, so
// there is one pin, not two. Unset means "no expectation" — a bare
// `go test -tags oracle`, or a run against SQI_EXPR_ORACLE_PYTHON, still works.
func assertPinnedVersion(t *testing.T, version string) {
	t.Helper()
	want := os.Getenv("SQI_EXPR_ORACLE_EXPECT_VERSION")
	if want == "" || version == want {
		return
	}
	t.Fatalf("reference implementation is openjd-model %s, but the pin is %s: "+
		"the venv predates the pin bump. Recreate it with "+
		"`rm -rf .venv-oracle && make expr-oracle-venv`, or clear "+
		"SQI_EXPR_ORACLE_EXPECT_VERSION to grade against whatever is installed.",
		version, want)
}

// mustTarget parses c's target type for EvalForBalanceCheck, which needs a
// expr.Type rather than the raw string evalGo works from. Called only on cases
// where evalGo already succeeded, which means its own identical ParseType call
// already succeeded too -- so a failure here would be a bug in this test file
// itself, not a corpus-driven condition, hence Fatalf rather than a silently
// skipped balance check.
func mustTarget(t *testing.T, target string) expr.Type {
	t.Helper()
	ty, err := expr.ParseType(target)
	if err != nil {
		t.Fatalf("parsing target type %q: %v", target, err)
	}
	return ty
}

// runOracle feeds the whole corpus through one interpreter and returns the
// reported package version alongside the results, keyed by case id.
func runOracle(t *testing.T, root, python string, cases []exprCase) (string, map[string]caseResult) {
	t.Helper()

	var stdin strings.Builder
	for _, c := range cases {
		line, err := json.Marshal(map[string]string{"id": c.id, "src": c.src, "target": c.target})
		if err != nil {
			t.Fatalf("encoding case %q: %v", c.id, err)
		}
		stdin.Write(line)
		stdin.WriteByte('\n')
	}

	ctx, cancel := context.WithTimeout(context.Background(), oracleTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, python, filepath.Join(root, "scripts", "expr-oracle.py"))
	cmd.Stdin = strings.NewReader(stdin.String())
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("running the reference implementation failed: %v\nstderr: %s", err, stderr.String())
	}

	version := "unknown"
	results := make(map[string]caseResult, len(cases))
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var reply oracleReply
		if err := json.Unmarshal(scanner.Bytes(), &reply); err != nil {
			t.Fatalf("decoding a reply from the reference implementation: %v\nline: %s", err, scanner.Text())
		}
		if reply.Banner {
			version = reply.Version
			continue
		}
		results[reply.ID] = caseResult{ok: reply.OK, value: reply.Value, typ: reply.Type, err: reply.Error, ops: reply.Ops}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading the reference implementation's output: %v", err)
	}
	return version, results
}

// loadCorpus reads the tab-separated case file.
func loadCorpus(t *testing.T, path string) []exprCase {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the corpus: %v", err)
	}
	var cases []exprCase
	seen := make(map[string]int)
	for i, line := range strings.Split(string(data), "\n") {
		lineNo := i + 1
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		target, src, found := strings.Cut(line, "\t")
		if !found {
			t.Fatalf("corpus.txt:%d: no tab separating the target type from the expression: %q", lineNo, line)
		}
		c := exprCase{id: caseID(target, src), target: target, src: src, line: lineNo}
		if prev, dup := seen[c.id]; dup {
			// A duplicate would be silently overwritten in the results map,
			// quietly shrinking the corpus.
			t.Fatalf("corpus.txt:%d: duplicate of line %d: %q", lineNo, prev, c.id)
		}
		seen[c.id] = lineNo
		cases = append(cases, c)
	}
	return cases
}

// caseID is the stable identity of a case, used as the results-map key and as
// the baseline entry. Line numbers are deliberately not used: they shift
// whenever a case is inserted, which would silently re-point every baseline
// entry at the wrong case.
func caseID(target, src string) string { return target + " :: " + src }

// loadBaseline reads the accepted-divergence file, mapping each case id to the
// reason recorded for it.
func loadBaseline(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the baseline: %v", err)
	}
	baseline := make(map[string]string)
	var reason strings.Builder
	for i, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			reason.Reset()
		case strings.HasPrefix(trimmed, "#"):
			// Comment lines immediately above an entry are its reason, so the
			// justification travels with the entry instead of sitting in a
			// separate document that drifts.
			if reason.Len() > 0 {
				reason.WriteByte(' ')
			}
			reason.WriteString(strings.TrimSpace(strings.TrimPrefix(trimmed, "#")))
		default:
			if reason.Len() == 0 {
				t.Fatalf("baseline.txt:%d: %q has no preceding comment giving the reason", i+1, trimmed)
			}
			baseline[trimmed] = reason.String()
			reason.Reset()
		}
	}
	return baseline
}

// findPython locates an interpreter with the reference implementation
// importable, skipping the test when there is none.
//
// A skip proves nothing — see doc.go. The Makefile target prints the same
// warning, and CI asserts the test ran by name.
func findPython(t *testing.T, root string) string {
	t.Helper()

	// An explicit override is authoritative. Falling back to the venv when it
	// does not work would run the corpus against a DIFFERENT interpreter than
	// the one named and report the result as though it were that one — which
	// is the whole reason someone sets this variable.
	if override := os.Getenv("SQI_EXPR_ORACLE_PYTHON"); override != "" {
		if err := canImportOracle(override); err != nil {
			t.Fatalf("SQI_EXPR_ORACLE_PYTHON=%s is unusable: %v", override, err)
		}
		return override
	}

	for _, python := range []string{
		filepath.Join(root, ".venv-oracle", "bin", "python3"),
		filepath.Join(root, ".venv-oracle", "Scripts", "python.exe"),
	} {
		if err := canImportOracle(python); err != nil {
			continue
		}
		return python
	}
	t.Skip("no interpreter with openjd.expr — run `make expr-oracle-venv`, or set SQI_EXPR_ORACLE_PYTHON")
	return ""
}

// canImportOracle reports whether the interpreter exists and can import the
// reference implementation.
func canImportOracle(python string) error {
	if _, err := os.Stat(python); err != nil {
		return err
	}
	// Bounded independently of the corpus run: an interpreter that hangs on a
	// bare import would otherwise hang the probe with no output at all.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, python, "-c", "import openjd.expr").CombinedOutput()
	if err != nil {
		return fmt.Errorf("cannot import openjd.expr: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// repoRoot walks up from the test's working directory to the module root, so
// the test does not depend on being invoked from any particular directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}
