#!/usr/bin/env bash
# Run the Go toolchain inside Docker.
#
# 本机装不了 Go，所有编译/测试/vet 都必须经由容器。模块缓存与编译缓存放在命名卷里，
# 首次之后的 build 才不会每次重新下载和重编。
#
#   scripts/go.sh build ./...
#   scripts/go.sh test ./... -count=1
#   scripts/go.sh vet ./...
#   scripts/go.sh fmt ./...
#
# GOPROXY 默认走 goproxy.cn：容器内访问 proxy.golang.org 会超时。本项目零第三方依赖，
# 正常情况下根本不需要联网，这个设置只是为了 go mod tidy 之类的场合不至于卡死。
#
# GOOS/GOARCH 透传给容器，交叉编译才能用（docker 的 `-e VAR` 在本机未设置时不会传空值）。
#
# GOTOOLCHAIN=local 禁止容器自己下载工具链，所以镜像的 Go 版本不能低于 go.mod 里声明的版本
# （当前 1.25）。用 VERGE_CLI_GO_IMAGE 换成更低的镜像会直接编译失败，不是回落。
set -euo pipefail

# 只钉次版本号，不钉补丁号：官方镜像会清理旧的补丁 tag，钉死了总有一天拉不到。
IMAGE="${VERGE_CLI_GO_IMAGE:-golang:1.26-alpine}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Git Bash 会把 /src 之类的容器内路径当成本机路径去转换，必须关掉；
# 挂载源路径则要用 Windows 形式（pwd -W），Docker Desktop 只认这个。
if [ -n "${MSYSTEM:-}" ] || [ -n "${WSLENV:-}" ]; then
  HOST_ROOT="$(cd "$ROOT" && pwd -W 2>/dev/null || echo "$ROOT")"
  export MSYS_NO_PATHCONV=1
  export MSYS2_ARG_CONV_EXCL='*'
else
  HOST_ROOT="$ROOT"
fi

exec docker run --rm \
  -v "${HOST_ROOT}:/src" \
  -v verge-cli-gomod:/go/pkg/mod \
  -v verge-cli-gobuild:/root/.cache/go-build \
  -w /src \
  -e CGO_ENABLED=0 \
  -e GOFLAGS=-buildvcs=false \
  -e GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" \
  -e GOTOOLCHAIN=local \
  -e GOOS \
  -e GOARCH \
  "$IMAGE" go "$@"
