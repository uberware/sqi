# SPDX-License-Identifier: AGPL-3.0-or-later

"""Evaluate EXPR expressions with the OpenJD reference implementation.

This is the Python half of `make test-expr-oracle`, a differential test that
compares sqi's Go expression evaluator (internal/openjd/expr) against the
reference implementation the specification names in its Recommended Library
Interface section: the `openjd.expr` namespace of the `openjd-model` package.

That namespace re-exports a compiled Rust crate (`openjd-expr`, in
OpenJobDescription/openjd-rs), so this script treats it as a black box:
expressions in, values or errors out.

Protocol, one JSON object per line in each direction so a whole corpus costs a
single interpreter start rather than one per case:

    stdin   {"id": "...", "src": "1 + 2", "target": "int"}
    stdout  {"id": "...", "ok": true, "value": "3", "type": "int", "ops": 1}
            {"id": "...", "ok": false, "error": "Cannot coerce ..."}

The first stdout line is a banner carrying the resolved package version, which
the Go side records in its failure output and checks against the pinned
version.

`value` is `str(ExprValue)`, the reference's own canonical rendering, compared
against Go's `Value.String()`. Go's formatFloat reproduces Python's repr, so
`1e10` renders "10000000000.0" on both sides.

The reference is not the authority. It is on the 0.x line, where minor
versions may make breaking changes, and the specification outranks it. A
divergence is a question to investigate, not a verdict against sqi; see
test/oracle/baseline.txt, which records the adjudicated disagreements.
"""

import json
import sys


def main() -> int:
    # The protocol is UTF-8 JSON. Python uses the locale encoding for stdio,
    # which on Windows is the ANSI code page (cp1252 on a US/Western install),
    # and the Go side always writes UTF-8. Without this, a case containing a
    # non-ASCII character is decoded as a different expression, and because
    # its `id` is mangled the same way the Go side reports "the reference
    # returned no result" for it.
    #
    # Only stdin needs it, since emit() leaves json.dumps at its ensure_ascii
    # default; stdout is set too so the two stay consistent.
    sys.stdin.reconfigure(encoding="utf-8")
    sys.stdout.reconfigure(encoding="utf-8")

    try:
        from openjd.expr import ExprType, PathFormat, parse_expression
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
        emit(evaluate(case, ExprType, PathFormat, parse_expression))

    return 0


def evaluate(case: dict, ExprType, PathFormat, parse_expression) -> dict:  # noqa: N803
    """Evaluate one case, reporting any failure as data rather than raising.

    A malformed target type is reported through the same `error` channel as a
    failed evaluation: to the Go side both mean the reference refused the
    case.
    """
    result = {"id": case["id"]}
    try:
        target = ExprType(case["target"])
        parsed = parse_expression(case["src"])
        # path_format is pinned to POSIX so the oracle answers the same on
        # every host. Unpinned, the reference follows the specification's
        # host-native default, so `path('/a/b/c')` renders "/a/b/c" on Linux
        # and "\a\b\c" on Windows. sqi's expr.WithPathFormat defaults to POSIX
        # so a server-side template expands identically whatever submitted it;
        # without the pin every path case would diverge on a Windows host. On
        # Linux and macOS the pin changes nothing.
        outcome = parsed.evaluate_with_metrics(
            target_type=target, path_format=PathFormat.POSIX
        )
    except BaseException as exc:  # noqa: BLE001 - see comment below
        result["ok"] = False
        # BaseException, not Exception: the reference is a Rust crate behind
        # pyo3 and panics rather than raising on some inputs (zfill with a
        # negative width, "capacity overflow"; ljust with a large width,
        # "Formatting argument out of range"). pyo3 raises a panic as
        # PanicException, which derives from BaseException, so a narrower
        # except would let one case end the loop and drop every case after it.
        #
        # Only the first line is kept: the reference appends a source-caret
        # excerpt, and messages are never compared.
        result["error"] = str(exc).splitlines()[0]
        return result
    result["ok"] = True
    result["value"] = str(outcome.value)
    result["type"] = str(outcome.value.type)
    # Section 1.3.10's operation count. The Go side compares it only on cases
    # whose values agree.
    #
    # peak_memory is not reported: section 1.3.9 makes value sizing
    # implementation-defined, so a memory divergence could not be adjudicated.
    result["ops"] = outcome.operation_count
    return result


def emit(obj: dict) -> None:
    # Flushed per line so a crash mid-corpus still delivers every earlier
    # result to the Go side.
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


if __name__ == "__main__":
    sys.exit(main())
