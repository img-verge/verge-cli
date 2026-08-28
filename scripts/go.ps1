<#
.SYNOPSIS
  Run the Go toolchain inside Docker.

.DESCRIPTION
  scripts/go.sh 的 PowerShell 版本，行为一致：本机不装 Go，所有编译/测试/vet 都在容器里跑，
  模块缓存与编译缓存放在命名卷里避免每次重新下载和重编。

  .\scripts\go.ps1 build ./...
  .\scripts\go.ps1 test ./... -count=1
  .\scripts\go.ps1 vet ./...
  .\scripts\go.ps1 fmt ./...

  镜像可用 $env:VERGE_CLI_GO_IMAGE 覆盖，但 GOTOOLCHAIN=local 禁止容器自己下载工具链，
  所以镜像的 Go 版本不能低于 go.mod 里声明的版本（当前 1.25），低了会直接编译失败。

  GOPROXY 默认走 goproxy.cn：容器内访问 proxy.golang.org 会超时。本项目零第三方依赖，
  正常情况下根本不需要联网。

  GOOS/GOARCH 透传给容器（本机未设置时不传，容器默认 linux/amd64），交叉编译才能用。

  本文件必须保持 UTF-8 带 BOM：Windows PowerShell 5.1 无 BOM 时按 GBK 解码，
  中文注释的字节会吞掉换行，把下一行并进注释（$image 会变成空，docker 就去拉 go:latest 了）。
#>

# $args 原样转交给容器里的 go，所以这里不声明具体参数。
$ErrorActionPreference = 'Stop'

# 只钉次版本号，不钉补丁号：官方镜像会清理旧的补丁 tag，钉死了总有一天拉不到。
$image = if ($env:VERGE_CLI_GO_IMAGE) { $env:VERGE_CLI_GO_IMAGE } else { 'golang:1.26-alpine' }
$proxy = if ($env:GOPROXY) { $env:GOPROXY } else { 'https://goproxy.cn,direct' }
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path

if ($args.Count -eq 0) {
    Write-Error 'usage: .\scripts\go.ps1 <go arguments>   e.g. .\scripts\go.ps1 test ./...'
    exit 2
}

& docker.exe run --rm `
    -v "${root}:/src" `
    -v verge-cli-gomod:/go/pkg/mod `
    -v verge-cli-gobuild:/root/.cache/go-build `
    -w /src `
    -e CGO_ENABLED=0 `
    -e GOFLAGS=-buildvcs=false `
    -e GOPROXY=$proxy `
    -e GOTOOLCHAIN=local `
    -e GOOS `
    -e GOARCH `
    $image go @args
exit $LASTEXITCODE
