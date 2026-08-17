# `pkg/`

Public Go packages — importable by external code (the `sqi-sdk` Python wheel is not built from here, but a future Go client library, custom worker plugins, or third-party integrations would be).

Anything placed here is part of the public API contract and must be versioned accordingly. Internal-only code belongs in `internal/`.

No public packages have been promoted here yet — `doc.go` only carries the
package documentation that reserves the directory. It is populated as exported
surfaces stabilize.
