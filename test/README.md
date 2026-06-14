# `test/`

Cross-cutting test assets that do not belong next to a single Go package:

- `test/integration/` — end-to-end harness that boots the server with a temp SQLite + embedded NATS, registers a mock worker, and runs a real OpenJD job
- `test/load/` — scheduler throughput and assignment-latency benchmarks
- `test/fixtures/openjd/` — corpus of valid and invalid OpenJD templates used by parser tests and fuzzers
- `test/smoke/` — end-to-end smoke script for the release verification step

Unit tests live next to the code they cover (`_test.go` files), not here.
