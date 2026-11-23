# `pkg/`

Public Go packages, importable by external code. The `sqi-sdk` Python wheel is not built from here.

Anything placed here is part of the public API contract and must be versioned accordingly. Internal-only code belongs in `internal/`.

The directory holds no public packages: `doc.go` carries only the package
documentation that reserves it.
