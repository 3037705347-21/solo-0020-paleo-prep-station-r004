#!/usr/bin/env bash
#
# verify-lifecycle.sh — repeatable lifecycle verification for the paleo
# prep station.
#
# Every runtime parameter (listen address, read/write/idle timeouts, shutdown
# deadline, probe resources) enters through configuration files and explicit
# overrides; nothing under test spawns its own listener except the server
# binary. The script verifies, in order:
#
#   1. build            go build ./... plus the two binaries
#   2. config-invalid   malformed/out-of-bound configuration is rejected at
#                       the CONFIG stage with exit code 2, before any socket
#   3. start            server binds from config and answers GET /healthz
#   4. workflows        all four business workflows pass and assert their
#                       state results (registered/preparing/completed+
#                       stabilized/available)
#   5. concurrent       four probes run in parallel against one server using
#                       distinct catalog numbers; all must pass
#   6. conflict-boundary duplicate catalog number is rejected with HTTP 409
#                       (proves concurrency was not made green by relaxing the
#                       unique catalog invariant)
#   7. stop             SIGTERM drains cleanly, process exits 0, port closes
#   8. port-conflict    a second bind on the live address fails at the LISTEN
#                       stage with exit code 1
#   9. alt-profile      the whole round repeats on a different address with
#                       tighter timeouts; workflow results stay identical
#
# Failures name the stage that failed. At the end of every round the script
# proves both processes and listeners are back to zero before exiting.
#
# Usage: scripts/verify-lifecycle.sh
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROFILE="$ROOT/config/paleo-prep-station.env"
ALT_PROFILE="$ROOT/config/paleo-prep-station.ci.env"
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/pps-verify.XXXXXX")"
SERVER_PID=""
FAILED_STAGE=""

# Allow an environment without a system Go (e.g. /tmp toolchains in CI).
if [[ -x "${GO:-}" ]]; then
  GO_BIN="$GO"
elif command -v go >/dev/null 2>&1; then
  GO_BIN="$(command -v go)"
else
  echo "FATAL: go toolchain not found (set GO=/path/to/go)" >&2
  exit 2
fi

trap 'cleanup $? $LINENO' EXIT

cleanup() {
  local code="$1" line="$2"
  trap - EXIT
  if [[ -n "$SERVER_PID" ]]; then
    local pid="$SERVER_PID" i
    kill -TERM "$pid" 2>/dev/null || true
    for i in {1..20}; do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
    kill -KILL "$pid" 2>/dev/null || true
  fi
  if [[ "$code" -ne 0 ]]; then
    echo "FAIL: verification failed at stage ${FAILED_STAGE:-unknown} (exit=$code, line=$line)" >&2
    echo "      logs kept in: $WORKDIR" >&2
    [[ -f "$WORKDIR/server.log" ]] && { echo "--- server.log tail ---" >&2; tail -n 20 "$WORKDIR/server.log" >&2; }
  else
    rm -rf "$WORKDIR"
  fi
}

stage() { FAILED_STAGE="$1"; echo "== $1 =="; }
pass()  { echo "   ok: $1"; }

# Load a KEY=VALUE profile into the current environment without executing
# arbitrary shell: comments and blank lines are stripped, values are exported.
load_profile() {
  local file="$1" line key value
  trim() { local s="$1"; s="${s#"${s%%[![:space:]]*}"}"; s="${s%"${s##*[![:space:]]}"}"; printf '%s' "$s"; }
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%%#*}"
    line="$(trim "$line")"
    [[ -z "$line" ]] && continue
    key="$(trim "${line%%=*}")"
    value="${line#*=}"
    export "$key=$value"
  done < "$file"
}

port_open() {
  local addr="$1" host port
  host="${addr%:*}"; port="${addr##*:}"
  timeout 1 bash -c "exec 3<>/dev/tcp/$host/$port" 2>/dev/null
}

wait_for_port() {
  local addr="$1" i
  for i in $(seq 1 50); do
    if port_open "$addr"; then return 0; fi
    sleep 0.1
  done
  return 1
}

assert_port_closed() {
  local addr="$1"
  if port_open "$addr"; then
    echo "FAIL: expected $addr to be released after shutdown" >&2
    exit 1
  fi
}

http_status() {
  # Print just the status code for a POST /specimens with the given JSON file.
  # curl --max-time takes a bare number of seconds, so strip any Go "s" suffix.
  local base="$1" body="$2" seconds
  seconds="${PPS_PROBE_REQUEST_TIMEOUT:-5s}"
  seconds="${seconds%s}"
  curl -sS -o /dev/null -w '%{http_code}' \
       --max-time "$seconds" \
       -X POST -H 'Content-Type: application/json' \
       --data-binary @"$body" "$base/specimens" 2>/dev/null || true
}

start_server() {
  SERVER_PID=""
  "$WORKDIR/server" >"$WORKDIR/server.log" 2>&1 &
  SERVER_PID=$!
  if ! wait_for_port "$PPS_ADDR"; then
    echo "server log:" >&2; cat "$WORKDIR/server.log" >&2
    echo "FAIL: server never bound $PPS_ADDR" >&2
    exit 1
  fi
  pass "server listening on $PPS_ADDR (pid $SERVER_PID)"
}

stop_server() {
  local pid="$SERVER_PID"
  kill -TERM "$pid"
  local i
  for i in $(seq 1 50); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
  done
  if kill -0 "$pid" 2>/dev/null; then
    echo "FAIL: server did not exit within shutdown deadline" >&2
    exit 1
  fi
  SERVER_PID=""
  local rc=0
  wait "$pid" 2>/dev/null || rc=$?
  if [[ "$rc" -ne 0 ]]; then
    echo "FAIL: server exited with $rc on SIGTERM" >&2
    exit 1
  fi
  pass "server exited 0 on SIGTERM"
}

run_check() {
  # Probe subprocesses must not inherit stray probe vars; explicit flags win.
  "$WORKDIR/check" \
    -workflow "$1" -base-url "$PPS_PROBE_BASE_URL" \
    -request-timeout "$PPS_PROBE_REQUEST_TIMEOUT" \
    -catalog-number "$2"
}

run_round() {
  local label="$1" profile="$2"
  echo "############ round: $label ############"
  load_profile "$profile"

  stage "start ($label)"
  start_server
  grep -q "paleo prep station listening on $PPS_ADDR" "$WORKDIR/server.log"
  pass "startup log records effective boundary: $(grep -o 'addr=.*' "$WORKDIR/server.log")"

  stage "healthz ($label)"
  local health
  health="$(curl -sS --max-time 3 "$PPS_PROBE_BASE_URL/healthz")"
  [[ "$health" == *"ok"* || "$health" == *"ready"* || "$health" == *"status"* ]]
  pass "GET /healthz -> $health"

  stage "workflows ($label)"
  local wf suffix
  suffix="$(date +%s)-$$"
  for wf in register-specimen approve-prep-plan log-preparation release-for-study; do
    run_check "$wf" "${PPS_PROBE_CATALOG_NUMBER}-$wf-$suffix" >"$WORKDIR/check-$wf.log" 2>&1
    grep -q "workflow check passed: $wf" "$WORKDIR/check-$wf.log"
    pass "$wf"
  done

  stage "concurrent ($label)"
  # Record probe PIDs and wait for them explicitly: a bare `wait` would also
  # wait for the long-lived server started in start_server and block the
  # script until SIGKILL.
  local i fail=0
  local pids=()
  for i in 1 2 3 4; do
    run_check log-preparation "${PPS_PROBE_CATALOG_NUMBER}-conc$i-$suffix" \
      >"$WORKDIR/conc-$i.log" 2>&1 &
    pids+=("$!")
  done
  wait "${pids[@]}" || fail=1
  if [[ "$fail" -ne 0 ]]; then
    for i in 1 2 3 4; do
      grep -q "workflow check passed: log-preparation" "$WORKDIR/conc-$i.log" \
        || { echo "concurrent probe $i failed:" >&2; cat "$WORKDIR/conc-$i.log" >&2; }
    done
    exit 1
  fi
  for i in 1 2 3 4; do
    grep -q "workflow check passed: log-preparation" "$WORKDIR/conc-$i.log" \
      || { echo "concurrent probe $i reported failure despite exit 0" >&2; exit 1; }
  done
  pass "4 parallel log-preparation probes passed against one listener"

  stage "conflict-boundary ($label)"
  cat >"$WORKDIR/dup.json" <<JSON
{"catalog_number":"${PPS_PROBE_CATALOG_NUMBER}-dup-$suffix","field_label":"北坡-07","taxon_hint":"大型脊椎动物","matrix_kind":"粉砂岩","jacket_notes":"夹克边角完好"}
JSON
  [[ "$(http_status "$PPS_PROBE_BASE_URL" "$WORKDIR/dup.json")" == "201" ]]
  local second
  second="$(http_status "$PPS_PROBE_BASE_URL" "$WORKDIR/dup.json")"
  if [[ "$second" != "409" ]]; then
    echo "FAIL: duplicate catalog number returned $second, expected 409" >&2
    exit 1
  fi
  pass "duplicate catalog number rejected with 409 (unique-catalog invariant intact)"

  stage "stop ($label)"
  stop_server
  assert_port_closed "$PPS_ADDR"
  pass "listener on $PPS_ADDR released"

  stage "port-conflict ($label)"
  "$WORKDIR/server" >"$WORKDIR/server-a.log" 2>&1 &
  SERVER_PID=$!
  wait_for_port "$PPS_ADDR"
  set +e
  "$WORKDIR/server" >"$WORKDIR/server-b.log" 2>&1
  local conflict_rc=$?
  set -e
  if [[ "$conflict_rc" -ne 1 ]] || ! grep -q "listen $PPS_ADDR" "$WORKDIR/server-b.log"; then
    echo "FAIL: second bind should fail at LISTEN with exit 1 (rc=$conflict_rc)" >&2
    cat "$WORKDIR/server-b.log" >&2
    exit 1
  fi
  pass "second bind refused: $(tail -n1 "$WORKDIR/server-b.log")"
  stop_server
  assert_port_closed "$PPS_ADDR"
  pass "no listener remains after port-conflict round"
}

stage "build"
( cd "$ROOT" && "$GO_BIN" build ./... )
"$GO_BIN" build -o "$WORKDIR/server" "$ROOT/cmd/paleo-prep-station"
"$GO_BIN" build -o "$WORKDIR/check"  "$ROOT/cmd/workflowcheck"
pass "go build ./... and both binaries"

stage "config-invalid"
expect_config_error() {
  local desc="$1"; shift
  set +e
  "$WORKDIR/server" "$@" >"$WORKDIR/cfg-bad.log" 2>&1
  local rc=$?
  set -e
  if [[ "$rc" -ne 2 ]]; then
    echo "FAIL: $desc expected exit 2, got $rc" >&2; cat "$WORKDIR/cfg-bad.log" >&2; exit 1
  fi
  pass "$desc -> exit 2: $(tail -n1 "$WORKDIR/cfg-bad.log" | sed 's/^config: //')"
}
expect_config_error "zero read-timeout"        -read-timeout 0s
expect_config_error "header > read timeout"    -read-header-timeout 20s -read-timeout 5s
expect_config_error "negative shutdown"        -shutdown-timeout -1s
expect_config_error "bad addr shape"           -addr no-port
expect_config_error "positional argument"      unexpected
PPS_WRITE_TIMEOUT=not-a-duration expect_config_error "malformed env duration"
set +e
PPS_PROBE_BASE_URL= "$WORKDIR/check" -workflow register-specimen >"$WORKDIR/cfg-bad.log" 2>&1
[[ $? -eq 2 ]] || { echo "FAIL: check without base-url must exit 2" >&2; exit 1; }
set -e
pass "workflowcheck without base-url -> exit 2"
set +e
"$WORKDIR/check" -workflow unknown-workflow -base-url http://127.0.0.1:1 >"$WORKDIR/cfg-bad.log" 2>&1
[[ $? -eq 2 ]] || { echo "FAIL: unknown workflow must exit 2" >&2; exit 1; }
set -e
pass "unknown workflow -> exit 2"

run_round "canonical-profile (historical defaults)" "$PROFILE"

stage "precedence"
# Environment overrides the profile; a CLI flag overrides the environment.
load_profile "$PROFILE"
export PPS_ADDR="127.0.0.1:18161"
export PPS_PROBE_BASE_URL="http://127.0.0.1:18161"
start_server
grep -q "addr=127.0.0.1:18161" "$WORKDIR/server.log"
pass "env override moves listener to 127.0.0.1:18161"
kill -TERM "$SERVER_PID"; wait "$SERVER_PID" 2>/dev/null || true; SERVER_PID=""
assert_port_closed "127.0.0.1:18161"

load_profile "$PROFILE"
"$WORKDIR/server" -addr 127.0.0.1:18162 >"$WORKDIR/server.log" 2>&1 &
SERVER_PID=$!
wait_for_port "127.0.0.1:18162"
grep -q "listening on 127.0.0.1:18162" "$WORKDIR/server.log"
pass "--addr flag wins over env/profile (127.0.0.1:18162)"
stop_server
assert_port_closed "127.0.0.1:18162"

run_round "alternate-profile (different addr, tighter boundary)" "$ALT_PROFILE"

stage "teardown"
# Final invariant: nothing from verification is left behind.
if pgrep -f "$WORKDIR/(server|check)" >/dev/null 2>&1; then
  echo "FAIL: stray processes remain" >&2; pgrep -af "$WORKDIR" >&2; exit 1
fi
assert_port_closed "127.0.0.1:18133"
assert_port_closed "127.0.0.1:18153"
assert_port_closed "127.0.0.1:18161"
assert_port_closed "127.0.0.1:18162"
pass "all processes exited and all listeners released"

echo
echo "SUCCESS: build, config validation, start, four workflows, concurrent"
echo "         probes, conflict boundary, graceful stop, port conflict and"
echo "         alternate-profile parameterisation all verified; zero"
echo "         processes/listeners remain."
