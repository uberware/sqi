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

## LDAP: `ldap_test.go`

`test/integration/ldap_test.go` (tag `integration`) is the only test that runs
against a **real directory**. Every other LDAP test drives a fake connection,
which cannot catch a mistake in go-ldap *wire* usage — a wrong search scope, a
misnamed attribute, a filter a real server rejects, or a server answering an
unsupported request in a way no fake would imitate.

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
