# `api/`

Reserved for API contract artifacts that are not Go source — JSON Schemas for
OpenJD payloads and config files, and any future Protobuf / gRPC definitions for
the worker wire protocol.

**The directory is currently empty.** The authoritative OpenAPI 3.1
specification lives at [`internal/api/openapi.yaml`](../internal/api/openapi.yaml),
next to the handlers it describes; it is embedded into `sqi-server` and served
at `GET /api/v1/openapi.yaml`.

Generated client/server code lives next to its consumers (`internal/api/...`,
`clients/python/`, …), not here.
