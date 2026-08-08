# SPDX-License-Identifier: AGPL-3.0-or-later

"""Evaluate EXPR expressions with the OpenJD reference implementation.

This is the Python half of `make test-expr-oracle`, a differential test that
compares sqi's Go expression evaluator (internal/openjd/expr) against the
reference implementation the specification names in its Recommended Library
Interface section: the `openjd.expr` namespace of the `openjd-model` package.

Despite the namespace, that package is not a Python implementation — it is a
thin re-export layer over a compiled Rust crate (`openjd-expr`, in
OpenJobDescription/openjd-rs). There is no Python source to read, so this
script treats it strictly as a black box: expressions in, values or errors out.

Protocol, one JSON object per line in each direction so a whole corpus costs a
single interpreter start rather than one per case:

    stdin   {"id": "...", "src": "1 + 2", "target": "int"}
    stdout  {"id": "...", "ok": true, "value": "3", "type": "int", "ops": 1}
            {"id": "...", "ok": false, "error": "Cannot coerce ..."}

The first stdout line is a banner carrying the resolved package version, which
the Go side records in its failure output — a divergence means nothing without
knowing which build of the reference produced it.

`value` is `str(ExprValue)`, the reference's own canonical rendering, compared
against Go's `Value.String()`. That the two agree on floats is not incidental:
Go's formatFloat deliberately reproduces Python's repr, so `1e10` renders
"10000000000.0" on both sides and a regression there shows up here.

THE REFERENCE IS NOT THE AUTHORITY. It is Beta (0.x, breaking changes allowed
in minor bumps) and the specification outranks it. A divergence is a question
to investigate, not a verdict against sqi — see test/oracle/baseline.txt, which
records the disagreements already adjudicated in sqi's favour.
"""

import json
import sys


def main() -> int:
    try:
        from openjd.expr import ExprType, parse_expression
    except ImportError as exc:  # pragma: no cover - exercised by the skip path
        print(f"import failed: {exc}", file=sys.stderr)
        return 2

    version = "unknown"
    try:
        from importlib.metadata import version as pkg_version

        version = pkg_version("openjd-model")
    except Exception:  # noqa: BLE001 - a missing version must not fail the run
        pass

    emit({"banner": True, "version": version})

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        case = json.loads(line)
        emit(evaluate(case, ExprType, parse_expression))

    return 0


def evaluate(case: dict, ExprType, parse_expression) -> dict:  # noqa: N803
    """Evaluate one case, reporting any failure as data rather than raising.

    A malformed target type is reported through the same `error` channel as a
    failed evaluation on purpose: from the Go side both are "the reference
    refused this case", and conflating them keeps one comparison rule instead
    of two.
    """
    result = {"id": case["id"]}
    try:
        target = ExprType(case["target"])
        parsed = parse_expression(case["src"])
        outcome = parsed.evaluate_with_metrics(target_type=target)
    except BaseException as exc:  # noqa: BLE001 - see comment below
        result["ok"] = False
        # BaseException, not Exception, and deliberately.
        #
        # The reference implementation is a compiled Rust crate behind pyo3, and
        # it PANICS rather than raising on several inputs — zfill with a
        # negative width ("capacity overflow") and ljust with a large width
        # ("Formatting argument out of range") are both in the corpus. pyo3
        # surfaces a panic as PanicException, which derives from BaseException,
        # so a narrower except lets one bad case kill the interpreter loop and
        # silently drop every case after it.
        #
        # Catching it turns a panic into an ordinary error line the Go side can
        # compare and baseline.
        # Only the first line: the reference appends a source-caret excerpt that
        # says nothing Go's own message would say, and messages are never
        # compared for equality anyway.
        result["error"] = str(exc).splitlines()[0]
        return result
    result["ok"] = True
    result["value"] = str(outcome.value)
    result["type"] = str(outcome.value.type)
    # Section 1.3.10's operation count. Compared by the Go side only on cases
    # whose VALUES already agree, so a count divergence is never stacked on a
    # value divergence that is already reported and already baselined.
    #
    # peak_memory is deliberately NOT reported: section 1.3.9 makes value sizing
    # explicitly implementation-defined, so a memory divergence could only ever
    # be suppressed, never adjudicated.
    result["ops"] = outcome.operation_count
    return result


def emit(obj: dict) -> None:
    # Flushed per line so a crash mid-corpus still leaves every prior result on
    # the Go side, which reports how far it got.
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


if __name__ == "__main__":
    sys.exit(main())
