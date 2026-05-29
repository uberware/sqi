# `api/`

API contract artifacts that are not Go source: the OpenAPI 3.1 specification (task 79), JSON Schemas for OpenJD payloads and config files, and any future Protobuf / gRPC definitions for the worker wire protocol (task 56).

Generated client/server code lives next to the consumers (`internal/api/...`, the Python client, etc.), not here. This directory holds the source-of-truth specs.
