#!/usr/bin/env bash
# verify-runtime.sh — 可重复的运行配置与验证流程。
#
# 每一轮依次验证六个场景，任何失败都带阶段标签并非零退出：
#   build     构建与静态检查（CGO_ENABLED=0，保证跨环境可重复）
#   startup   正常启动：flag 覆盖 PPS_* 环境变量，探针路径就绪
#   probe     并发探针：PPS_PROBE_CONCURRENCY 路并发全部返回 200
#   shutdown  主动停止：SIGTERM 后退出码为 0，进程与监听归零
#   listen    端口冲突：第二实例退出码为 2，错误标记 stage listen
#   config    配置边界：零超时、零并发在 config 阶段被拒绝（退出码 1）
#   workflow  工作流回归：默认与调优两套配置下四条工作流状态结果一致
#   audit     每轮结束断言进程与监听全部归零
#
# 用法：scripts/verify-runtime.sh [轮数]
# 配置通过 PPS_* 环境变量进入（与服务一致），轮数也可用 ROUNDS 指定。
set -euo pipefail

cd "$(dirname "$0")/.."

ROUNDS="${1:-${ROUNDS:-1}}"
ADDR="${PPS_ADDR:-127.0.0.1:18133}"
PROBE_PATH="${PPS_PROBE_PATH:-/healthz}"
CONCURRENCY="${PPS_PROBE_CONCURRENCY:-8}"
READY_ATTEMPTS="${VERIFY_READY_ATTEMPTS:-50}"

# 纯 Go 模块，禁用 CGO 让构建不依赖本机 gcc，保证跨环境可重复。
export CGO_ENABLED=0

TMP="$(mktemp -d)"
SERVER_BIN="$TMP/bin/paleo-prep-station"
CHECK_BIN="$TMP/bin/workflowcheck"
PIDS=()

stage() { printf '[stage %-8s] %s\n' "$1" "$2"; }
fail() { printf '[FAILED] stage %s: %s\n' "$1" "$2" >&2; exit 1; }

cleanup() {
	local p
	for p in $(jobs -p); do
		kill "$p" 2>/dev/null || true
	done
	rm -rf "$TMP"
}
trap cleanup EXIT

# curl 退出码 7 = connection refused，即端口空闲。
port_free() {
	local code=0
	curl --noproxy '*' -sS -o /dev/null --max-time 1 "http://$ADDR$PROBE_PATH" 2>/dev/null || code=$?
	[ "$code" -eq 7 ]
}

wait_ready() {
	local pid="$1" attempt=0
	while [ "$attempt" -lt "$READY_ATTEMPTS" ]; do
		kill -0 "$pid" 2>/dev/null || return 1
		if curl --noproxy '*' -fsS -o /dev/null --max-time 1 "http://$ADDR$PROBE_PATH" 2>/dev/null; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 0.1
	done
	return 1
}

# bounded_wait pid [秒]：等待子进程退出并返回其退出码，超期 SIGKILL（退出码 137）。
bounded_wait() {
	local pid="$1" limit="${2:-10}" code=0 watchdog
	(
		sleep "$limit"
		kill -KILL "$pid" 2>/dev/null || true
	) &
	watchdog=$!
	wait "$pid" || code=$?
	kill "$watchdog" 2>/dev/null || true
	wait "$watchdog" 2>/dev/null || true
	return "$code"
}

# assert_zero_leftovers 阶段名：本轮启动的进程必须全部退出，端口必须释放。
assert_zero_leftovers() {
	local label="$1" pid
	for pid in ${PIDS[@]+"${PIDS[@]}"}; do
		if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
			fail "$label" "process $pid still running"
		fi
	done
	port_free || fail "$label" "port $ADDR still listening"
	if pgrep -f "$TMP/bin" >/dev/null 2>&1; then
		fail "$label" "leftover processes under $TMP/bin"
	fi
}

command -v go >/dev/null 2>&1 || fail setup "go toolchain not found on PATH"

stage build "go build ./... && go vet ./..."
go build ./... || fail build "go build ./... failed"
go vet ./... || fail build "go vet ./... failed"
mkdir -p "$TMP/bin"
go build -o "$SERVER_BIN" ./cmd/paleo-prep-station || fail build "server build failed"
go build -o "$CHECK_BIN" ./cmd/workflowcheck || fail build "workflowcheck build failed"

port_free || fail setup "port $ADDR already in use before verification"

for round in $(seq 1 "$ROUNDS"); do
	stage round "round $round/$ROUNDS on $ADDR"

	# 场景一：正常启动。PPS_ADDR 故意指向临时端口，--addr 必须胜出。
	log_main="$TMP/server.round$round.log"
	PPS_ADDR="127.0.0.1:0" "$SERVER_BIN" --addr "$ADDR" >"$log_main" 2>&1 &
	pid=$!
	PIDS+=("$pid")
	wait_ready "$pid" || fail startup "server not ready: $(tail -n 3 "$log_main")"
	grep -q "addr=$ADDR" "$log_main" || fail startup "effective config not logged: $(cat "$log_main")"
	grep -q "listening on $ADDR" "$log_main" || fail startup "listen log missing: $(cat "$log_main")"
	stage startup "listening on $ADDR (pid $pid, --addr overrides PPS_ADDR)"

	# 场景二：并发探针。
	fails=0
	probe_pids=()
	for _ in $(seq 1 "$CONCURRENCY"); do
		curl --noproxy '*' -fsS -o /dev/null --max-time 2 "http://$ADDR$PROBE_PATH" &
		probe_pids+=("$!")
	done
	for p in "${probe_pids[@]}"; do
		wait "$p" || fails=$((fails + 1))
	done
	[ "$fails" -eq 0 ] || fail probe "$fails/$CONCURRENCY concurrent probes failed"
	stage probe "$CONCURRENCY concurrent probes on $PROBE_PATH all returned 200"

	# 场景三：主动停止。SIGTERM 后必须在关闭期限内以 0 退出。
	kill -TERM "$pid"
	code=0
	bounded_wait "$pid" || code=$?
	[ "$code" -eq 0 ] || fail shutdown "SIGTERM exit code $code, want 0: $(tail -n 3 "$log_main")"
	PIDS=()
	assert_zero_leftovers shutdown
	stage shutdown "SIGTERM -> exit 0, process and listener released"

	# 场景四：端口冲突。第二实例必须在 listen 阶段失败（退出码 2）。
	log_a="$TMP/server.primary.round$round.log"
	log_b="$TMP/server.conflict.round$round.log"
	"$SERVER_BIN" --addr "$ADDR" >"$log_a" 2>&1 &
	pid_a=$!
	PIDS+=("$pid_a")
	wait_ready "$pid_a" || fail listen "primary server not ready: $(tail -n 3 "$log_a")"

	"$SERVER_BIN" --addr "$ADDR" >"$log_b" 2>&1 &
	pid_b=$!
	PIDS+=("$pid_b")
	code=0
	bounded_wait "$pid_b" || code=$?
	[ "$code" -eq 2 ] || fail listen "port conflict exit code $code, want 2: $(cat "$log_b")"
	grep -q "stage listen" "$log_b" || fail listen "conflict not labeled stage listen: $(cat "$log_b")"
	stage listen "second instance on $ADDR rejected: exit 2, stage listen"

	kill -TERM "$pid_a"
	code=0
	bounded_wait "$pid_a" || code=$?
	[ "$code" -eq 0 ] || fail shutdown "primary exit code $code, want 0"
	PIDS=()
	assert_zero_leftovers listen

	# 场景五：配置边界。零超时、零并发一律在 config 阶段拒绝，不能靠放宽边界过关。
	code=0
	PPS_READ_TIMEOUT=0s "$SERVER_BIN" --addr "$ADDR" >"$TMP/bad-server.round$round.log" 2>&1 || code=$?
	[ "$code" -eq 1 ] || fail config "zero read-timeout accepted, exit code $code"
	grep -q "stage config" "$TMP/bad-server.round$round.log" \
		|| fail config "invalid config not labeled: $(cat "$TMP/bad-server.round$round.log")"
	code=0
	"$CHECK_BIN" --probe-concurrency 0 --workflow register-specimen >"$TMP/bad-check.round$round.log" 2>&1 || code=$?
	[ "$code" -eq 1 ] || fail config "zero probe-concurrency accepted, exit code $code"
	grep -q "stage config" "$TMP/bad-check.round$round.log" \
		|| fail config "invalid config not labeled: $(cat "$TMP/bad-check.round$round.log")"
	stage config "zero timeouts/concurrency rejected at config stage (exit 1)"

	# 场景六：工作流回归。默认与调优两套配置下，四条工作流的状态结果必须一致。
	out_default="$TMP/workflows.default.round$round.log"
	out_tuned="$TMP/workflows.tuned.round$round.log"
	: >"$out_default"
	for wf in register-specimen approve-prep-plan log-preparation release-for-study; do
		"$CHECK_BIN" --workflow "$wf" >>"$out_default" 2>"$TMP/wf.default.round$round.err" \
			|| fail workflow "default config: $wf failed: $(tail -n 3 "$TMP/wf.default.round$round.err")"
	done
	env PPS_READ_HEADER_TIMEOUT=2s PPS_READ_TIMEOUT=8s PPS_WRITE_TIMEOUT=8s \
		PPS_IDLE_TIMEOUT=20s PPS_SHUTDOWN_TIMEOUT=2s PPS_PROBE_TIMEOUT=2s \
		PPS_PROBE_CONCURRENCY=4 PPS_PROBE_READY_TIMEOUT=3s \
		"$CHECK_BIN" --workflow all >"$out_tuned" 2>"$TMP/wf.tuned.round$round.err" \
		|| fail workflow "tuned config: workflows failed: $(tail -n 3 "$TMP/wf.tuned.round$round.err")"
	grep '^workflow-status ' "$out_default" >"$TMP/status.default.$round"
	grep '^workflow-status ' "$out_tuned" >"$TMP/status.tuned.$round"
	diff -u "$TMP/status.default.$round" "$TMP/status.tuned.$round" \
		|| fail workflow "config change altered workflow status results"
	stage workflow "4 workflows green under default and tuned configs, identical statuses"

	assert_zero_leftovers audit
	stage audit "round $round: zero processes, zero listeners"
done

stage done "all $ROUNDS round(s) passed on $ADDR"
