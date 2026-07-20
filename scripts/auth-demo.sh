#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# End-to-end demonstration of the Phase 3 auth surface on a live local farm.
#
# Boots a real sqi-server with auth ENABLED (temp SQLite + temp embedded-NATS,
# ephemeral loopback ports, mDNS off) and a bootstrap admin, connects a real
# sqi-worker, then walks the whole authenticated surface end to end:
#
#   * unauthenticated requests are rejected (401)
#   * bootstrap admin login mints a session cookie
#   * CSRF: a cookie-authenticated mutation without an Origin is rejected (403)
#   * API keys (Bearer) authenticate without a cookie and bypass the CSRF guard
#   * RBAC: a `user`-role account is denied the admin-only routes (403)
#   * job owner scoping: each account sees only its own jobs, and a foreign
#     job id is 403 even when the caller knows it
#   * the worker runs the job to completion with NO credentials of its own
#     (worker<->server transport auth is deliberately out of scope in Phase 3)
#   * self-service password change sweeps other sessions but keeps API keys
#   * an admin can revoke another user's API key (apikeys.admin)
#
# Every step is asserted, so this doubles as a regression check on the auth
# surface. With --keep the farm is left running afterwards so you can log into
# the web UI and poke at it by hand.
#
# Usage:
#   bash scripts/auth-demo.sh          # build if needed, run the flow, tear down
#   bash scripts/auth-demo.sh --keep   # leave server + worker running at the end
#   make auth-demo                     # same, via the Makefile
#
# Requirements: curl, jq, python3 (for allocating free loopback ports).
#
# Environment overrides:
#   SQI_SERVER_BIN   path to a prebuilt sqi-server (default: <repo>/bin/sqi-server)
#   SQI_WORKER_BIN   path to a prebuilt sqi-worker (default: <repo>/bin/sqi-worker)
#   SQI_AUTH_DEMO_KEEP=1   same as --keep
#
# Exit status: 0 only if every assertion passed; non-zero with a clear message
# (and the relevant server/worker log tail) otherwise.

set -euo pipefail

# ── Locations ─────────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

SERVER_BIN="${SQI_SERVER_BIN:-${REPO_ROOT}/bin/sqi-server}"
WORKER_BIN="${SQI_WORKER_BIN:-${REPO_ROOT}/bin/sqi-worker}"

KEEP="${SQI_AUTH_DEMO_KEEP:-0}"
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=1 ;;
    -h|--help) sed -n '3,40p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) printf 'unknown argument: %s (try --help)\n' "$arg" >&2; exit 2 ;;
  esac
done

# ── Credentials used throughout ───────────────────────────────────────────────

ADMIN_USER="admin"
ADMIN_PASS="admin-demo-pw"
ARTIST_USER="artist"
ARTIST_PASS="artist-demo-pw"
ARTIST_NEW_PASS="artist-demo-pw2"

# ── Logging and assertion helpers ─────────────────────────────────────────────

STEP=0

log()  { printf '[auth-demo] %s\n' "$*" >&2; }
fail() { printf '[auth-demo] FAIL: %s\n' "$*" >&2; exit 1; }

step() {
  STEP=$((STEP + 1))
  printf '[auth-demo]\n[auth-demo] === %02d. %s ===\n' "$STEP" "$*" >&2
}

# ok <message> — record a passed assertion.
ok() { printf '[auth-demo]   ok: %s\n' "$*" >&2; }

# assert_eq <label> <expected> <actual>
assert_eq() {
  local label="$1" want="$2" got="$3"
  [ "$want" = "$got" ] || fail "${label}: expected '${want}', got '${got}'"
  ok "${label} = ${got}"
}

log_tail() {
  local label="$1" file="$2"
  if [ -f "$file" ]; then
    printf '[auth-demo] --- %s (tail) ---\n' "$label" >&2
    tail -n 40 "$file" >&2 || true
    printf '[auth-demo] --- end %s ---\n' "$label" >&2
  fi
}

# ── Tool detection ────────────────────────────────────────────────────────────

command -v curl >/dev/null 2>&1 || fail "curl is required but not found on PATH"
command -v jq   >/dev/null 2>&1 || fail "jq is required but not found on PATH"
command -v python3 >/dev/null 2>&1 \
  || fail "python3 is required (used only to allocate free loopback ports)"

# jbody <key> <value> [<key> <value> ...] — build a flat JSON object of string
# fields. Everything goes through `jq --arg`, so quoting is never our problem.
#
# Use this rather than hand-escaping a literal: a `-d "{\"k\":\"v\"}"` payload
# does NOT survive nesting inside "$( ... )" — the backslashes reach the server
# verbatim and it answers 400 "invalid JSON body", which reads exactly like an
# auth failure and sends you hunting in the wrong place.
jbody() {
  local args=() parts=""
  while [ $# -gt 0 ]; do
    args+=(--arg "$1" "$2")
    [ -n "$parts" ] && parts="${parts},"
    parts="${parts}\"$1\":\$$1"
    shift 2
  done
  jq -nc "${args[@]}" "{${parts}}"
}

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

# ── Request helpers ───────────────────────────────────────────────────────────
#
# Two authentication styles are exercised deliberately:
#
#   as_key    Bearer API key. No cookie is sent, so the CSRF guard passes the
#             request through untouched — this is how headless clients talk.
#   as_cookie Session cookie from a jar. Unsafe methods MUST also carry an
#             Origin or the CSRF guard rejects them with 403, which is exactly
#             what step 5 proves.
#
# Both print the response body; the HTTP status is captured separately by
# code_* so a failing assertion can report a real status rather than an
# empty body.

# as_key <api-key> <curl-args...>
as_key() {
  local key="$1"; shift
  curl -sS -H "Authorization: Bearer ${key}" "$@"
}

# as_cookie <jar> <curl-args...> — sends Origin so unsafe methods pass CSRF.
as_cookie() {
  local jar="$1"; shift
  curl -sS -b "$jar" -H "Origin: ${BASE_URL}" "$@"
}

# code_key <api-key> <curl-args...> — print only the HTTP status code.
code_key() {
  local key="$1"; shift
  curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer ${key}" "$@"
}

# code_cookie <jar> <curl-args...> — print only the HTTP status code.
code_cookie() {
  local jar="$1"; shift
  curl -sS -o /dev/null -w '%{http_code}' -b "$jar" -H "Origin: ${BASE_URL}" "$@"
}

# ── Build binaries if needed ──────────────────────────────────────────────────

if [ -x "$SERVER_BIN" ] && [ -x "$WORKER_BIN" ]; then
  log "using existing binaries: $SERVER_BIN, $WORKER_BIN"
else
  log "building binaries via 'make build' (this may take a minute or two)..."
  make -C "$REPO_ROOT" build >&2
  [ -x "$SERVER_BIN" ] || fail "server binary not found after build: $SERVER_BIN"
  [ -x "$WORKER_BIN" ] || fail "worker binary not found after build: $WORKER_BIN"
fi

# ── Temp workspace + teardown trap ────────────────────────────────────────────

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/sqi-auth-demo.XXXXXX")"
SERVER_LOG="${TMP_DIR}/server.log"
WORKER_LOG="${TMP_DIR}/worker.log"
ADMIN_JAR="${TMP_DIR}/admin.cookies"
ARTIST_JAR_A="${TMP_DIR}/artist-a.cookies"
ARTIST_JAR_B="${TMP_DIR}/artist-b.cookies"

SERVER_PID=""
WORKER_PID=""

cleanup() {
  local status=$?
  set +e
  if [ "$KEEP" = "1" ] && [ "$status" -eq 0 ]; then
    # Leave the farm up for manual poking; the temp dir holds its state.
    return 0
  fi
  [ -n "$WORKER_PID" ] && kill "$WORKER_PID" >/dev/null 2>&1
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" >/dev/null 2>&1
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    kill -0 "$WORKER_PID" >/dev/null 2>&1 || kill -0 "$SERVER_PID" >/dev/null 2>&1 || break
    sleep 0.2
  done
  [ -n "$WORKER_PID" ] && kill -9 "$WORKER_PID" >/dev/null 2>&1
  [ -n "$SERVER_PID" ] && kill -9 "$SERVER_PID" >/dev/null 2>&1
  rm -rf "$TMP_DIR" >/dev/null 2>&1
  return $status
}
trap cleanup EXIT INT TERM

# ── 1. Start the server with auth enabled ─────────────────────────────────────

step "start sqi-server with auth.enabled=true and a bootstrap admin"

HTTP_PORT="$(free_port)"
NATS_PORT="$(free_port)"
HTTP_ADDR="127.0.0.1:${HTTP_PORT}"
NATS_ADDR="127.0.0.1:${NATS_PORT}"
BASE_URL="http://${HTTP_ADDR}"
API="${BASE_URL}/api/v1"

# The bootstrap only fires on an EMPTY user table, which the fresh temp SQLite
# path guarantees. Against an existing database it is a deliberate no-op.
SQI_HTTP_ADDR="$HTTP_ADDR" \
SQI_NATS_ADDR="$NATS_ADDR" \
SQI_NATS_DATA_DIR="${TMP_DIR}/nats" \
SQI_STORE_SQLITE_PATH="${TMP_DIR}/sqi.db" \
SQI_DISCOVERY_ENABLED="false" \
SQI_SCHEDULER_TICK_INTERVAL="100ms" \
SQI_AUTH_ENABLED="true" \
SQI_AUTH_BOOTSTRAP_USERNAME="$ADMIN_USER" \
SQI_AUTH_BOOTSTRAP_PASSWORD="$ADMIN_PASS" \
SQI_LOG_LEVEL="warn" \
  "$SERVER_BIN" serve >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!

ready=0
for _ in $(seq 1 150); do
  if ! kill -0 "$SERVER_PID" >/dev/null 2>&1; then
    log_tail "server log" "$SERVER_LOG"
    fail "sqi-server exited before becoming ready"
  fi
  if [ "$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/readyz" 2>/dev/null || true)" = "200" ]; then
    ready=1
    break
  fi
  sleep 0.2
done
[ "$ready" -eq 1 ] || { log_tail "server log" "$SERVER_LOG"; fail "sqi-server not ready within 30s"; }
ok "server ready at ${BASE_URL}"

# ── 2. The gate is closed ─────────────────────────────────────────────────────

step "unauthenticated requests are rejected"

assert_eq "GET /jobs unauthenticated" "401" \
  "$(curl -sS -o /dev/null -w '%{http_code}' "${API}/jobs")"

# /readyz and the OpenAPI document stay public by design — liveness probes and
# API documentation must not require a credential.
assert_eq "GET /openapi.yaml unauthenticated (public by design)" "200" \
  "$(curl -sS -o /dev/null -w '%{http_code}' "${API}/openapi.yaml")"

# ── 3. Bootstrap admin login ──────────────────────────────────────────────────

step "log in as the bootstrap admin (session cookie)"

login_body="$(curl -sS -c "$ADMIN_JAR" -X POST "${API}/auth/login" \
  -H 'Content-Type: application/json' \
  -d "$(jbody username "$ADMIN_USER" password "$ADMIN_PASS")")"
assert_eq "login username" "$ADMIN_USER" "$(printf '%s' "$login_body" | jq -r '.username // empty')"
grep -q 'sqi_session' "$ADMIN_JAR" || fail "no sqi_session cookie was set by login"
ok "sqi_session cookie set (HttpOnly)"

# A wrong password and an unknown user must be indistinguishable — both 401,
# so the endpoint cannot be used to enumerate valid usernames.
bad_pw_body="$(jbody username "$ADMIN_USER" password "definitely-not-the-password")"
bad_user_body="$(jbody username "nobody-here" password "irrelevant")"
assert_eq "login with wrong password" "401" \
  "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "${API}/auth/login" \
      -H 'Content-Type: application/json' -d "$bad_pw_body")"
assert_eq "login with unknown user" "401" \
  "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "${API}/auth/login" \
      -H 'Content-Type: application/json' -d "$bad_user_body")"

me="$(as_cookie "$ADMIN_JAR" "${API}/auth/me")"
assert_eq "GET /auth/me role" "admin" "$(printf '%s' "$me" | jq -r '.roles[0] // empty')"

# ── 4. CSRF guard ─────────────────────────────────────────────────────────────

step "CSRF: a cookie-authenticated mutation without an Origin is blocked"

# Same request, twice, differing only in the Origin header. The cookie is an
# ambient credential the browser attaches automatically, so an unsafe method
# carrying one must prove it came from this origin.
assert_eq "POST /farms with cookie, no Origin" "403" \
  "$(curl -sS -o /dev/null -w '%{http_code}' -b "$ADMIN_JAR" -X POST "${API}/farms" \
      -H 'Content-Type: application/json' -d '{"name":"csrf probe"}')"

farm_json="$(as_cookie "$ADMIN_JAR" -X POST "${API}/farms" \
  -H 'Content-Type: application/json' -d '{"name":"auth demo farm"}')"
FARM_ID="$(printf '%s' "$farm_json" | jq -r '.id // empty')"
[ -n "$FARM_ID" ] || fail "could not create farm (response: ${farm_json})"
ok "POST /farms with cookie + Origin succeeded — farm ${FARM_ID}"

queue_json="$(as_cookie "$ADMIN_JAR" -X POST "${API}/queues" \
  -H 'Content-Type: application/json' \
  -d "$(jbody farm_id "$FARM_ID" name "auth demo queue")")"
QUEUE_ID="$(printf '%s' "$queue_json" | jq -r '.id // empty')"
[ -n "$QUEUE_ID" ] || fail "could not create queue (response: ${queue_json})"
ok "queue ${QUEUE_ID}"

# ── 5. API keys ───────────────────────────────────────────────────────────────

step "mint an admin API key (Bearer auth, no cookie, no CSRF guard)"

key_json="$(as_cookie "$ADMIN_JAR" -X POST "${API}/api-keys" \
  -H 'Content-Type: application/json' -d '{"name":"admin demo key"}')"
ADMIN_KEY="$(printf '%s' "$key_json" | jq -r '.secret // empty')"
[ -n "$ADMIN_KEY" ] || fail "no secret in API key response: ${key_json}"
case "$ADMIN_KEY" in
  sqi_*) ok "secret returned once, sqi_-prefixed" ;;
  *) fail "API key secret is not sqi_-prefixed: ${ADMIN_KEY%%_*}_..." ;;
esac

# The raw secret is shown exactly once. Listing keys must never return it.
# (The key collections return a bare JSON array, unlike the paginated
# {items, total} envelopes used by jobs/workers/tasks.)
list_json="$(as_key "$ADMIN_KEY" "${API}/api-keys")"
assert_eq "GET /api-keys leaks no secret" "null" \
  "$(printf '%s' "$list_json" | jq -r '[.[]?.secret] | map(select(. != null)) | first // "null"')"

# Bearer requests carry no cookie, so the CSRF guard has nothing to protect and
# passes them through — no Origin header needed.
assert_eq "Bearer mutation without Origin" "201" \
  "$(code_key "$ADMIN_KEY" -X POST "${API}/farms" \
      -H 'Content-Type: application/json' -d '{"name":"bearer csrf probe"}')"

assert_eq "garbage bearer token" "401" \
  "$(code_key "sqi_not_a_real_key" "${API}/auth/me")"

# ── 6. RBAC ───────────────────────────────────────────────────────────────────

step "create a 'user'-role account and prove the permission boundary"

user_json="$(as_key "$ADMIN_KEY" -X POST "${API}/users" \
  -H 'Content-Type: application/json' \
  -d "$(jbody username "$ARTIST_USER" password "$ARTIST_PASS" role user)")"
ARTIST_ID="$(printf '%s' "$user_json" | jq -r '.id // empty')"
[ -n "$ARTIST_ID" ] || fail "could not create user (response: ${user_json})"
assert_eq "created user role" "user" "$(printf '%s' "$user_json" | jq -r '.role')"
assert_eq "create response omits password hash" "null" \
  "$(printf '%s' "$user_json" | jq -r '.password_hash // "null"')"

curl -sS -c "$ARTIST_JAR_A" -o /dev/null -X POST "${API}/auth/login" \
  -H 'Content-Type: application/json' \
  -d "$(jbody username "$ARTIST_USER" password "$ARTIST_PASS")"
grep -q 'sqi_session' "$ARTIST_JAR_A" || fail "artist login did not set a session cookie"

artist_key_json="$(as_cookie "$ARTIST_JAR_A" -X POST "${API}/api-keys" \
  -H 'Content-Type: application/json' -d '{"name":"artist demo key"}')"
ARTIST_KEY="$(printf '%s' "$artist_key_json" | jq -r '.secret // empty')"
ARTIST_KEY_ID="$(printf '%s' "$artist_key_json" | jq -r '.id // empty')"
[ -n "$ARTIST_KEY" ] || fail "could not mint artist API key: ${artist_key_json}"
ok "artist logged in and minted an API key"

# users.read / users.manage belong to admin only; apikeys.self is granted to
# every role, so the same account that is denied /users can manage its own keys.
assert_eq "artist GET /users (needs users.read)"  "403" "$(code_key "$ARTIST_KEY" "${API}/users")"
assert_eq "artist POST /users (needs users.manage)" "403" \
  "$(code_key "$ARTIST_KEY" -X POST "${API}/users" \
      -H 'Content-Type: application/json' -d '{"username":"mallory","password":"x","role":"admin"}')"
assert_eq "artist GET /api-keys (apikeys.self)" "200" "$(code_key "$ARTIST_KEY" "${API}/api-keys")"
assert_eq "admin  GET /users"                   "200" "$(code_key "$ADMIN_KEY"  "${API}/users")"

# PATCH /auth/me is self-scoped and ignores privilege fields, so it cannot be
# used to escalate. Ask for admin; keep the display name, stay a user.
esc="$(as_key "$ARTIST_KEY" -X PATCH "${API}/auth/me" \
  -H 'Content-Type: application/json' \
  -d '{"display_name":"Artist One","role":"admin"}')"
assert_eq "PATCH /auth/me applied display_name" "Artist One" \
  "$(printf '%s' "$esc" | jq -r '.display_name // empty')"
assert_eq "PATCH /auth/me ignored role escalation" "user" \
  "$(printf '%s' "$esc" | jq -r '.roles[0] // empty')"

# ── 7. The worker joins with no credentials at all ────────────────────────────

step "start sqi-worker — note it is given NO credentials"

# Worker<->server transport auth is deliberately out of scope for Phase 3
# (deferred to Phase 4 hardening). The worker connects to the embedded NATS
# broker unauthenticated; enabling auth changes the REST/UI surface only.
SQI_WORKER_NATS_URL="nats://${NATS_ADDR}" \
SQI_WORKER_DISCOVERY_ENABLE_MDNS="false" \
SQI_WORKER_FARM_ID="$FARM_ID" \
SQI_WORKER_QUEUE_IDS="$QUEUE_ID" \
SQI_WORKER_DATA_DIR="${TMP_DIR}/worker-data" \
SQI_WORKER_ALLOW_ROOT="true" \
SQI_WORKER_LOG_LEVEL="warn" \
SQI_WORKER_LOG_FORMAT="text" \
SQI_WORKER_HEARTBEAT_INTERVAL="1s" \
SQI_WORKER_PULL_IDLE_BACKOFF="300ms" \
SQI_WORKER_METRICS_ADDR="127.0.0.1:$(free_port)" \
  "$WORKER_BIN" start >"$WORKER_LOG" 2>&1 &
WORKER_PID=$!

WORKER_ID=""
for _ in $(seq 1 150); do
  if ! kill -0 "$WORKER_PID" >/dev/null 2>&1; then
    log_tail "worker log" "$WORKER_LOG"
    fail "sqi-worker exited before coming online"
  fi
  WORKER_ID="$(as_key "$ADMIN_KEY" "${API}/workers" 2>/dev/null \
    | jq -r '[.items[]? | select(.status=="online") | .id][0] // empty')"
  [ -n "$WORKER_ID" ] && break
  sleep 0.2
done
[ -n "$WORKER_ID" ] || { log_tail "worker log" "$WORKER_LOG"; fail "worker did not come online within 30s"; }
ok "worker online with no credentials: ${WORKER_ID}"

# ── 8. Job owner scoping ──────────────────────────────────────────────────────

step "submit jobs as each account — owner is bound to the principal"

submit_job() { # submit_job <api-key> <job-name> — prints the job JSON
  local key="$1" name="$2"
  as_key "$key" -X POST "${API}/jobs?farm_id=${FARM_ID}&queue_id=${QUEUE_ID}" \
    -H 'Content-Type: application/x-yaml' \
    --data-binary "$(cat <<YAML
specificationVersion: "jobtemplate-2023-09"
name: ${name}
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: echo
          args:
            - "hello from ${name}"
YAML
)"
}

artist_job="$(submit_job "$ARTIST_KEY" "artist job")"
ARTIST_JOB_ID="$(printf '%s' "$artist_job" | jq -r '.id // empty')"
[ -n "$ARTIST_JOB_ID" ] || fail "artist could not submit a job: ${artist_job}"
# No ?owner= was passed: the owner comes from the authenticated principal.
assert_eq "artist job owner" "$ARTIST_USER" "$(printf '%s' "$artist_job" | jq -r '.owner')"

admin_job="$(submit_job "$ADMIN_KEY" "admin job")"
ADMIN_JOB_ID="$(printf '%s' "$admin_job" | jq -r '.id // empty')"
[ -n "$ADMIN_JOB_ID" ] || fail "admin could not submit a job: ${admin_job}"
assert_eq "admin job owner" "$ADMIN_USER" "$(printf '%s' "$admin_job" | jq -r '.owner')"

step "owner scoping: who can see which job"

# The `user` role lacks jobs.read.all, so its list is pinned to its own owner.
# `admin` holds it and sees the whole farm.
artist_owners="$(as_key "$ARTIST_KEY" "${API}/jobs" | jq -r '[.items[].owner] | unique | join(",")')"
assert_eq "artist GET /jobs owners" "$ARTIST_USER" "$artist_owners"
assert_eq "admin  GET /jobs total" "2" "$(as_key "$ADMIN_KEY" "${API}/jobs" | jq -r '.total')"

# A scoped caller aiming ?owner= at someone else gets its own jobs back — the
# forced filter overrides the query parameter rather than erroring.
assert_eq "artist GET /jobs?owner=admin owners" "$ARTIST_USER" \
  "$(as_key "$ARTIST_KEY" "${API}/jobs?owner=${ADMIN_USER}" | jq -r '[.items[].owner] | unique | join(",")')"

# Knowing the id is not enough. jobs.read.all gates writes as well as reads,
# so the same 403 protects DELETE.
assert_eq "artist GET  /jobs/{admin job}" "403" "$(code_key "$ARTIST_KEY" "${API}/jobs/${ADMIN_JOB_ID}")"
assert_eq "artist DELETE /jobs/{admin job}" "403" \
  "$(code_key "$ARTIST_KEY" -X DELETE "${API}/jobs/${ADMIN_JOB_ID}")"
assert_eq "artist GET  /jobs/{own job}"   "200" "$(code_key "$ARTIST_KEY" "${API}/jobs/${ARTIST_JOB_ID}")"

# ── 9. The farm still runs ────────────────────────────────────────────────────

step "the job actually executes — auth gates the API, not the farm"

# Jobs reach "completed"; it is TASKS that reach "succeeded".
job_status=""
for _ in $(seq 1 200); do
  job_status="$(as_key "$ARTIST_KEY" "${API}/jobs/${ARTIST_JOB_ID}" | jq -r '.status // empty')"
  case "$job_status" in
    completed|failed|canceled) break ;;
  esac
  sleep 0.2
done
[ "$job_status" = "completed" ] || {
  log_tail "worker log" "$WORKER_LOG"
  log_tail "server log" "$SERVER_LOG"
  fail "artist job did not complete within 40s (status: ${job_status:-unknown})"
}
ok "artist job ${ARTIST_JOB_ID} completed"

# Task logs are reachable through the task route, which resolves ownership via
# the owning job — so the scoping rule holds one level down too.
TASK_ID="$(as_key "$ARTIST_KEY" "${API}/jobs/${ARTIST_JOB_ID}/tasks" | jq -r '.items[0].id // empty')"
[ -n "$TASK_ID" ] || fail "could not resolve a task id for the artist job"
assert_eq "artist GET /tasks/{own task}/logs" "200" \
  "$(code_key "$ARTIST_KEY" "${API}/tasks/${TASK_ID}/logs")"

ADMIN_TASK_ID="$(as_key "$ADMIN_KEY" "${API}/jobs/${ADMIN_JOB_ID}/tasks" | jq -r '.items[0].id // empty')"
[ -n "$ADMIN_TASK_ID" ] || fail "could not resolve a task id for the admin job"
assert_eq "artist GET /tasks/{admin task}/logs" "403" \
  "$(code_key "$ARTIST_KEY" "${API}/tasks/${ADMIN_TASK_ID}/logs")"

# ── 10. Self-service password change ──────────────────────────────────────────

step "password change sweeps other sessions but leaves API keys alone"

# A second artist session, standing in for "logged in on another machine".
curl -sS -c "$ARTIST_JAR_B" -o /dev/null -X POST "${API}/auth/login" \
  -H 'Content-Type: application/json' \
  -d "$(jbody username "$ARTIST_USER" password "$ARTIST_PASS")"
assert_eq "second session works before the change" "200" \
  "$(code_cookie "$ARTIST_JAR_B" "${API}/auth/me")"

# Wrong current password is 403, not 401: the caller IS authenticated and only
# failed the re-auth check.
wrong_current="$(jbody current_password "not-my-password" new_password "$ARTIST_NEW_PASS")"
assert_eq "password change with wrong current password" "403" \
  "$(code_cookie "$ARTIST_JAR_A" -X PUT "${API}/auth/password" \
      -H 'Content-Type: application/json' -d "$wrong_current")"

# -c re-saves the jar so session A picks up the freshly issued cookie.
change_body="$(jbody current_password "$ARTIST_PASS" new_password "$ARTIST_NEW_PASS")"
assert_eq "password change" "204" \
  "$(curl -sS -o /dev/null -w '%{http_code}' -b "$ARTIST_JAR_A" -c "$ARTIST_JAR_A" \
      -H "Origin: ${BASE_URL}" -X PUT "${API}/auth/password" \
      -H 'Content-Type: application/json' -d "$change_body")"

assert_eq "calling session survives (cookie re-issued)" "200" \
  "$(code_cookie "$ARTIST_JAR_A" "${API}/auth/me")"
assert_eq "other session revoked" "401" \
  "$(code_cookie "$ARTIST_JAR_B" "${API}/auth/me")"
assert_eq "API keys are NOT revoked by a password change" "200" \
  "$(code_key "$ARTIST_KEY" "${API}/auth/me")"
old_pw_login="$(jbody username "$ARTIST_USER" password "$ARTIST_PASS")"
assert_eq "old password no longer logs in" "401" \
  "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "${API}/auth/login" \
      -H 'Content-Type: application/json' -d "$old_pw_login")"

# ── 11. Admin revokes another user's key ──────────────────────────────────────

step "admin revokes the artist's API key (apikeys.admin)"

# An admin can LIST and REVOKE another user's keys but cannot MINT one for
# them: revoking someone's credential and minting one they are accountable for
# are different acts, so there is deliberately no admin key-create route.
assert_eq "artist cannot list another user's keys" "403" \
  "$(code_key "$ARTIST_KEY" "${API}/users/${ARTIST_ID}/api-keys")"
assert_eq "admin lists the artist's keys" "1" \
  "$(as_key "$ADMIN_KEY" "${API}/users/${ARTIST_ID}/api-keys" | jq -r 'length')"

assert_eq "admin revokes the key" "204" \
  "$(code_key "$ADMIN_KEY" -X DELETE "${API}/users/${ARTIST_ID}/api-keys/${ARTIST_KEY_ID}")"
assert_eq "revoked key is rejected" "401" "$(code_key "$ARTIST_KEY" "${API}/auth/me")"

# The session cookie is a separate credential and is untouched by key revocation.
assert_eq "artist session still valid after key revocation" "200" \
  "$(code_cookie "$ARTIST_JAR_A" "${API}/auth/me")"

step "logout revokes the session server-side"

assert_eq "POST /auth/logout" "200" \
  "$(code_cookie "$ARTIST_JAR_A" -X POST "${API}/auth/logout")"
assert_eq "session is dead after logout" "401" \
  "$(code_cookie "$ARTIST_JAR_A" "${API}/auth/me")"

# ── Summary ───────────────────────────────────────────────────────────────────

log ""
log "=============================================="
log "AUTH DEMO PASSED — ${STEP} steps, all assertions green"
log "=============================================="

if [ "$KEEP" = "1" ]; then
  log ""
  log "Leaving the farm running (--keep). Open the web UI:"
  log ""
  log "    ${BASE_URL}"
  log ""
  log "  admin:  ${ADMIN_USER} / ${ADMIN_PASS}"
  log "  artist: ${ARTIST_USER} / ${ARTIST_NEW_PASS}   (the password-change step rotated it)"
  log ""
  log "  Log in as artist: the Admin section is absent from the sidebar and"
  log "  /users is guarded. The job list shows only the artist's own job."
  log ""
  log "  admin API key: ${ADMIN_KEY}"
  log "  farm=${FARM_ID} queue=${QUEUE_ID}"
  log "  state: ${TMP_DIR}   logs: server.log, worker.log"
  log ""
  log "  Stop with: kill ${SERVER_PID} ${WORKER_PID} && rm -rf ${TMP_DIR}"
fi

exit 0
