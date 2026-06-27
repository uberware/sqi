# REDACTED_ENV_VARS

- Origin: official
- Status: supported
- Summary: openjd_redacted_env stdout directive; redacts a variable's value from logs.

## Motivation
Allows a task to set a session environment variable whose cleartext value must
not appear in captured logs (e.g. a token).

## Schema
The task emits an `openjd_redacted_env` stdout directive. The template declares
`REDACTED_ENV_VARS` in its `extensions` list.

## Validation
The extension name must be declared to be accepted (registry lookup); see
`internal/openjd/validate.go`.

## Worker behavior
Implemented in `internal/worker/openjd/envdirective.go` and
`internal/worker/session/session.go`: the variable is set in the session
environment while its value is redacted from logs.
