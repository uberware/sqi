# `test/`

Cross-cutting test assets that do not belong next to a single Go package:

- `test/integration/` — end-to-end harness that boots the server with a temp SQLite + embedded NATS, registers a mock worker, and runs a real OpenJD job
- `test/load/` — scheduler throughput and assignment-latency benchmarks
- `test/fixtures/openjd/` — corpus of valid and invalid OpenJD templates used by parser tests and fuzzers
- `test/smoke/` — end-to-end smoke script for the release verification step

Unit tests live next to the code they cover (`_test.go` files), not here.

## Build tags

Most files in `test/integration/` are untagged and run with `make test`. Files
behind the `integration` build tag need `make test-integration`, because they
depend on something the default suite should not require — a built binary, or
an external service.

Two of these suites talk to a **real external identity source** rather than a
fake, and both skip — rather than fail — when Docker is absent. A skip verifies
nothing, so confirm they actually ran (`GOFLAGS=-count=1`, and look for
`--- PASS`) after touching the code they cover.

## LDAP: `ldap_test.go`

`test/integration/ldap_test.go` (tag `integration`) runs against a **real
directory**. Every other LDAP test drives a fake connection, which cannot catch
a mistake in go-ldap *wire* usage — a wrong search scope, a misnamed attribute,
a filter a real server rejects, or a server answering an unsupported request in
a way no fake would imitate.

```sh
make test-ldap    # boots a throwaway OpenLDAP container; needs Docker
```

Skips cleanly when Docker is absent. `SQI_TEST_LDAP_URL` points it at a
directory you already have (including a real Active Directory) instead of
starting a container; that directory must hold the tree in the file's
`seedLDIF` constant.

It runs in CI on every change (`ldap-integration`, on both amd64 and arm64
because the image's variants differ), and it is a genuine
regression guard, not a smoke test: it was written after a real bug in which
OpenLDAP answered the AD-only nested-group matching rule with *success and zero
entries*, which silently demoted users to `default_role`. See `docs/auth.md`
§ "How this is tested".

## OIDC / SSO: `oidc_test.go`

`test/integration/oidc_test.go` (tag `integration`) runs the SSO login path
against a **real Keycloak**. The unit tests drive a fake provider that signs
real tokens, so a validation mistake surfaces there — but a fake returns
whatever the test asks for, so it cannot show what a real provider *omits*.
Keycloak emits no group membership without a protocol mapper configured for it,
and a token with no groups still validates: every user silently lands on
`default_role`.

```sh
make test-oidc    # boots a throwaway Keycloak container; needs Docker
```

`SQI_TEST_OIDC_ISSUER` points it at a realm you already have instead of starting
a container; that realm must hold the fixture the file seeds, and the tests that
mutate the fixture skip under it. The image tag is **pinned** — the tests scrape
Keycloak's login and logout-confirmation markup — and must stay in step with the
`docker pull` in `.github/workflows/ci.yml`.

It runs in CI on every change (`oidc-integration`), on one architecture rather
than two: no arch-specific divergence is known for a JVM application whose HTTP
and token behavior is all this job asserts on.

It also pins the vendor behavior the logout design rests on — including the
measured finding that Keycloak answers an `id_token_hint`-less end-session
request with an interactive confirmation page and keeps its session live behind
it. See `docs/auth.md` § "Provider logout is weaker than it looks".
