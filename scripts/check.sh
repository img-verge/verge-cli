#!/usr/bin/env bash
# 提交前跑一遍：vet + test。
#
# 全部经由 scripts/go.sh 在容器里跑（本机没有 Go）。零第三方依赖，所以不联网也能过。
#
#   scripts/check.sh                    # vet + 全部测试
#   scripts/check.sh -run TestRunTask   # 多余参数透传给 go test
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "== go vet ./... =="
"$ROOT/scripts/go.sh" vet ./...

echo
echo "== go test ./... =="
# -count=1 关掉测试结果缓存。缓存命中会让「我改了东西但测试没重跑」看起来像通过。
"$ROOT/scripts/go.sh" test ./... -count=1 "$@"

echo
echo "all checks passed"
