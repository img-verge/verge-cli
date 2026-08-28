#!/usr/bin/env bash
# Cross-compile release binaries and package them into dist/.
#
# 全部在容器里编：本机没有 Go。零第三方依赖 + CGO_ENABLED=0，所以每个目标平台都是
# 单个静态可执行文件，不需要交叉编译工具链。
#
#   scripts/build.sh              # 编全部平台并打包，版本号取 git describe
#   scripts/build.sh v1.0.0       # 指定版本号
#   VERGE_CLI_TARGETS="linux/amd64 darwin/arm64" scripts/build.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${1:-$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)}"
TARGETS="${VERGE_CLI_TARGETS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64}"

rm -rf "$ROOT/dist"
mkdir -p "$ROOT/dist"

for target in $TARGETS; do
  goos="${target%%/*}"
  goarch="${target##*/}"
  binary="verge-cli"
  [ "$goos" = "windows" ] && binary="${binary}.exe"

  echo "building dist/${binary} (${VERSION}) for ${goos}/${goarch}"
  GOOS="$goos" GOARCH="$goarch" \
    "$ROOT/scripts/go.sh" build \
    -trimpath \
    -ldflags "-s -w -X github.com/img-verge/verge-cli/internal/app.version=${VERSION}" \
    -o "dist/${binary}" .

  # 打包：Linux/macOS 用 tar.gz，Windows 用 zip
  pkgname="verge-cli_${VERSION}_${goos}_${goarch}"
  if [ "$goos" = "windows" ]; then
    cd "$ROOT/dist"
    zip "${pkgname}.zip" "${binary}"
    rm "${binary}"
    cd "$ROOT"
  else
    tar -czf "$ROOT/dist/${pkgname}.tar.gz" -C "$ROOT/dist" "${binary}"
    rm "$ROOT/dist/${binary}"
  fi
  echo "  packaged dist/${pkgname}.tar.gz" 2>/dev/null || echo "  packaged dist/${pkgname}.zip"
done

echo
ls -la "$ROOT/dist"
