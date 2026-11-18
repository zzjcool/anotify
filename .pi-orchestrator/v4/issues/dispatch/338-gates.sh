#!/usr/bin/env bash
# #338 最终门禁（协调者运行；在最终集成 worktree 里执行）
# 用法: bash .pi-orchestrator/v4/issues/dispatch/338-gates.sh /root/code/wt-338
set -uo pipefail
ROOT=${1:?worktree path}
LOG=${2:-/root/tmp/338-final}
mkdir -p "$LOG"
cd "$ROOT"
export TMPDIR=$LOG/tmp GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto
mkdir -p "$TMPDIR"
echo "HEAD=$(git rev-parse HEAD)  $(date -u)"

run() { # name timeout cmd...
  local name=$1 tmo=$2; shift 2
  echo "▶ $name"
  ( set -o pipefail; timeout "$tmo" "$@" 2>&1 | tee "$LOG/$name.log" | tail -5; echo "$name EXIT=${PIPESTATUS[0]}" )
}

run contract      900  make contract
run gotest-all   2400  go test ./... -count=1
run race-core    2400  go test ./internal/store/... ./internal/ws/... ./internal/server/... -race -count=1
run ci           2400  make ci
run e2e-quick     900  make e2e-quick
run build         900  make build
run manifest      300  make manifest
run anode-skill   300  make check-anode-skill
run e2e-all      5400  make e2e
echo "DONE $(date -u)"
