# `cmd/sqi-server`

Entry point for the `sqi-server` binary. `main.go` lives here and is intentionally thin — it wires up the cobra command tree (see task 11) and delegates everything else to packages under `internal/`.

Other binaries (`sqi-worker`, future tooling) get their own sibling directory under `cmd/`.
