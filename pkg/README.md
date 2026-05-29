# `pkg/`

Public Go packages — importable by external code (the `sqi-client` Python wheel is not built from here, but a future Go client library, custom worker plugins, or third-party integrations would be).

Anything placed here is part of the public API contract and must be versioned accordingly. Internal-only code belongs in `internal/`.

This directory is empty in Phase 1; populated as exported surfaces stabilize.
