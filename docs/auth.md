# Authentication (Phase 3)

sqi ships with **authentication off by default** — on a trusted local network,
every request is served as an anonymous superuser and nothing is gated. This is
the pre-Phase-3 behaviour and remains the default.

## The opt-in gate

The single switch is `auth.enabled` (config file `auth.enabled`, env
`SQI_AUTH_ENABLED`, flag `--auth-enabled`; default `false`).

> **A0 status:** the gate is scaffolding only. The switch is plumbed and
> validated but does not yet change behaviour — enabling it does **not** lock
> the server down, because no credential backend exists yet. Local accounts and
> login arrive in A1.

## Model

Every request carries a `Principal` (subject, display name, roles, kind,
superuser flag) in its context. When auth is off, the middleware injects an
anonymous principal with the superuser flag set, so authorization checks (added
in a later component) are bypassed. Authentication is pluggable: an
`Authenticator` resolves a request's credentials to a `Principal`, and future
credential types (sessions, API keys, LDAP, OIDC) each implement that one
interface.

The REST resource routes are gated by the auth middleware; the WebSocket
upgrade is gated by its own hook; the health/readiness/metrics probes and the
OpenAPI spec are always public.

## Coming next

- A1 — local accounts, password login, server-side sessions, the web login shell.
- A2 — API keys for headless SDK/submitter use.
- B1 — role-based access control (admin / operator / user / read-only).
