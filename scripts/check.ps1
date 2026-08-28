<#
.SYNOPSIS
  提交前跑一遍：vet + test。

.DESCRIPTION
  scripts/check.sh 的 PowerShell 版本，行为一致：本机没有 Go，全部经由 scripts/go.ps1
  在容器里跑。零第三方依赖，所以不联网也能过。

  .\scripts\check.ps1                    # vet + 全部测试
  .\scripts\check.ps1 -run TestRunTask   # 多余参数透传给 go test
#>
$ErrorActionPreference = 'Stop'

$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path

Write-Output '== go vet ./... =='
& "$root\scripts\go.ps1" vet ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Output ''
Write-Output '== go test ./... =='
# -count=1 关掉测试结果缓存。缓存命中会让「我改了东西但测试没重跑」看起来像通过。
& "$root\scripts\go.ps1" test ./... -count=1 @args
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Output ''
Write-Output 'all checks passed'