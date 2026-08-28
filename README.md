<div align="center">

# verge-cli

**[Verge API](https://api.verge-ai.xyz) 图像接口的命令行客户端**

[English](./README.en.md) · 简体中文

</div>

---

`verge-cli` 把 Verge API 的图像生成接口包成一个命令行工具：查模型、查额度、创建生图任务、自动完成三段式上传、轮询并下载结果图，都是一条命令。

零第三方依赖，单个静态可执行文件，没有运行时。

```bash
export VERGE_API_KEY=sk-...

verge-cli task create "雨夜霓虹街头，赛博朋克，电影感" -r 2k -a 16:9 --wait -o ./out
```

## 目录

- [安装](#安装)
- [配置](#配置)
- [命令](#命令)
  - [balance — 查余额](#balance--查余额)
  - [quota — 预估扣费](#quota--预估扣费)
  - [models — 查可用模型](#models--查可用模型)
  - [task — 生图任务](#task--生图任务)
  - [download — 下载结果图](#download--下载结果图)
  - [config — 读写配置](#config--读写配置)
- [参考图与 `[@名称]` 引用](#参考图与名称引用)
- [出口码](#出口码)
- [脚本化](#脚本化)
- [开发](#开发)
- [设计取舍](#设计取舍)
- [许可证](#许可证)

## 安装

### 下载预编译版本

从 [Releases](https://github.com/img-verge/verge-cli/releases) 下载对应平台的压缩包，解压后把 `verge-cli` 可执行文件放进 `PATH`：

| 压缩包 | 平台 |
|--------|------|
| `verge-cli_v1.0.0_windows_amd64.zip` | Windows 64-bit |
| `verge-cli_v1.0.0_linux_amd64.tar.gz` | Linux 64-bit |
| `verge-cli_v1.0.0_darwin_amd64.tar.gz` | macOS Intel |
| `verge-cli_v1.0.0_darwin_arm64.tar.gz` | macOS Apple Silicon |

### 用 Go 安装

需要 Go 1.25+（`go.mod` 里声明的版本）：

```bash
go install github.com/img-verge/verge-cli@latest
```

### 从源码构建

本仓库不要求本机装 Go，所有编译都在 Docker 里跑：

```bash
./scripts/go.sh build -o verge-cli .        # 或 Windows: .\scripts\go.ps1 build -o verge-cli.exe .
./scripts/check.sh                      # vet + test，提交前跑一遍（Windows: .\scripts\check.ps1）
./scripts/build.sh v0.1.0               # 交叉编译全部平台到 dist/，版本号注入二进制
```

`go.sh build` 不注入版本号，编出来的二进制 `verge-cli version` 报的是 `verge-cli dev`。要带真实版本号就用 `build.sh <版本号>`，产物在 `dist/`。

## 配置

优先级固定为 **命令行 flag > 环境变量 > 配置文件 > 内置默认值**。

| 项 | flag | 环境变量 | `config set` 键 | 内置默认值 |
| --- | --- | --- | --- | --- |
| API Key | `--api-key` | `VERGE_API_KEY` | `api-key` | — |
| 接口地址 | `--base-url` | `VERGE_API_BASE_URL` | `base-url` | `https://api.verge-ai.xyz/v1` |
| 模型 | `-m` / `--model` | — | `model` | `gpt-image-2` |
| 分辨率 | `-r` / `--resolution` | — | `resolution` | `1080p` |
| 宽高比 | `-a` / `--aspect-ratio` | — | `aspect-ratio` | `1:1` |

最省事的一次性配置：

```bash
verge-cli config set api-key sk-...
verge-cli config set model gpt-image-2
verge-cli config set resolution 2k
verge-cli config show
```

配置文件位置（`verge-cli config path` 会打印实际路径）：

- Windows：`%APPDATA%\verge\config.json`
- macOS / Linux：`$XDG_CONFIG_HOME/verge/config.json`，未设则 `~/.config/verge/config.json`
- `VERGE_CONFIG` 环境变量可以整体覆盖

文件里存的是明文 API Key，写入时权限收到 `0600`；`verge-cli config show` 只输出掩码后的值，可以放心贴进 issue。

自建部署填自己的地址即可，`/v1` 缺失时会自动补上：

```bash
verge-cli config set base-url https://gateway.example.com
```

## 命令

全局 flag 在子命令前后都生效：`verge-cli --json balance` 与 `verge-cli balance --json` 完全等价。

`verge-cli help` 看总览，`verge-cli help <命令>` 看单个命令的全部参数。

### balance — 查余额

```console
$ verge-cli balance
wallet available  1,234,567
key available     unlimited
```

`key available` 显示 `unlimited` 表示这个 Key 没有单独的额度上限；显示 `0` 表示有上限且已用尽。两者含义完全不同，所以刻意分开渲染。

人类可读输出末尾会附一行 `request_id <id>`：它来自网关的 `X-Oneapi-Request-Id` 响应头（错误响应的 body 里不一定有，头里一定有），报障时带上它，服务端可以直接定位到具体那次调用。`--json` 保持纯透传，不会混入 request_id。

### quota — 预估扣费

不创建任务、不扣额度，只问「这么一次生成会预扣多少」，走的是和真实提交同一套定价代码：

```console
$ verge-cli quota -m gpt-image-2 -r 2k -a 16:9 -n 2
model              gpt-image-2
resolution         2k
aspect ratio       16:9
images             2
pre-charged quota  8,000
```

### models — 查可用模型

列出这个 Key 在用户、分组、单 Key 模型限制全部生效之后**实际可用**的模型：

```console
$ verge-cli models
MODEL                           RESOLUTIONS    ENDPOINTS
gemini-3-pro-image-preview      1080p, 2k, 4k  image-generation, openai
gemini-3.1-flash-image-preview  1080p, 2k, 4k  image-generation, openai
gemini-3.1-flash-lite-image     1080p          image-generation, openai
gpt-image-2                     1080p, 2k, 4k  image-generation, openai

4 model(s). RESOLUTIONS comes from this CLI's built-in table, not the API.
```

默认只显示声明支持图像生成的模型，`--all` 显示全部。

`RESOLUTIONS` 一列来自 CLI 内置的表而不是接口，所以输出里会明确标注 —— 服务端随时会上新模型，这一列可能滞后。

### task — 生图任务

`verge-cli task create` 是唯一的生图入口：既可以创建后立即返回 task id，也可以用 `--wait` 等待并下载结果。

提交现在、稍后取图，适合交互、脚本和批量：

```bash
# 创建即返回 task id
verge-cli task create "雨夜霓虹街头"

# 创建并等到出图，顺便下载
verge-cli task create "雨夜霓虹街头" --wait -o ./out

# 带本地参考图：prepare → 直传对象存储 → submit，三段式由 CLI 自动完成
verge-cli task create "把 [@主体] 做成电影海报" -f 主体=./a.png --wait -o ./out

# 公网 URL 直接随任务提交；也可以与本地/base64 参考图混用
verge-cli task create "把 [@角色] 放进 [@背景]" -f 角色=./char.png -u 背景=https://example.com/bg.png --wait

# base64 文件或内联 data URI 会先解码，再走同一套三段式上传
verge-cli task create "参考 [@风格]" --base64-file 风格=./style.b64.txt --wait -o ./out
verge-cli task create "参考 [@纹理]" --base64-data '纹理=data:image/png;base64,iVBOR...' --wait -o ./out

# 查询已有任务
verge-cli task get task_xxx
verge-cli task get task_xxx --wait -o ./out
```

`-o` 隐含 `--wait`：一个「下载」参数悄悄什么都不下载，比多等一会儿糟糕得多。

`task create` 支持 `--prompt-file PATH`（`-` 表示 stdin）。位置参数优先；提示词文件上限 1 MiB，且必须是 UTF-8 文本。

关于三段式上传，有三点值得知道：

1. **所有 POST 都不自动重试。** 只有公网 URL 或无参考图时，CLI 直接创建任务；只要存在 `-f`、`--base64-file` 或 `--base64-data`，CLI 就执行 `prepare → 预签名 PUT → submit`。prepare 成功后已经有 `task_id`；后续错误会把它打印到 stderr。submit 超时、断线或 5xx 时不要重新 submit，执行 `verge-cli task get <task_id>` 查询原任务。
2. **`--retries` 只作用于 GET 和预签名 PUT。** PUT 仅重试传输错误和 429、500、502、503、504；每次尝试都保持图片二进制、`Content-Type`、`Content-Length` 不变，而且绝不携带 Verge API Key。上传失败上报只是 best-effort 诊断；submit 不会执行，原任务停留在上传阶段并最终超时。重新运行完整任务以获取新的上传槽位。
3. **本地文件和 base64 解码结果都受 10 MiB 单文件上限约束。** 超限时 CLI 会用同一套 JPEG 降质/降采样策略压到限制内，并在 stderr 警告。重编码会丢弃透明通道和动图帧；需要无损参考图时请先在本地压缩好。

### download — 下载结果图

```bash
verge-cli download task_xxx -o ./out --prefix poster
```

图片链接 7 天后失效（封面图 1 天）。这个命令会先重新查一次任务拿新链接，而不是用你早先存下来的那个。

签名过期的链接不少对象存储会回 HTTP 200 加一段 XML 错误体，所以下载时会检查 `Content-Type`：不是图片就报错，绝不把错误体当图片落盘。

### config — 读写配置

```bash
verge-cli config show          # 看生效值（Key 已掩码）
verge-cli config path          # 看配置文件在哪
verge-cli config set <键> <值>
verge-cli config unset <键>
```

`aspect-ratio` 写了不支持的值会直接报错 —— 它是文档级封闭枚举，存错等于之后每次生成都失败。`model` 和 `resolution` 只警告不拦：服务端随时会上新模型和新档位，拦死会让 CLI 立刻过时。

## 参考图与 `[@名称]` 引用

参考图最多 7 张，本地文件、base64 图片和公网 URL 合起来算。

```bash
# 不命名：按传入顺序生效
verge-cli task create "融合这两张图的风格" -f ./a.png -f ./b.png --wait

# 命名：在提示词里用 [@名称] 精确指代
verge-cli task create "把 [@角色] 放进 [@背景] 里" \
  -f 角色=./char.png \
  -u 背景=https://example.com/bg.png \
  --wait -o ./out

# 命名 base64：裸标准 base64 与 data URI 都支持
verge-cli task create "按 [@线稿] 上色" --base64-file 线稿=./lineart.b64.txt --wait
verge-cli task create "采用 [@配色]" --base64-data '配色=iVBORw0KGgoAAA...' --wait
```

- `-f` / `--file` 传本地文件，`-u` / `--image-url` 传公网 URL，`--base64-file` 读取 base64 文本文件，`--base64-data` 接受命令行中的裸标准 base64 或 `data:image/*;base64,...`；四者都可重复、可以混用。
- `名称=值` 是命名形式。路径/URL 只有在第一个 `=` 左边不含路径分隔符时才按命名解析，所以 `C:\pics\a=b.png` 和 `https://x/y?k=v` 不会误判。`--base64-data` 会先尝试把整个参数当 base64/data URI 解码，因此末尾的 `=` / `==` 不会被误认成名称分隔符。
- 服务端按 `[@名称]` **精确匹配**，名字写错不会报错，那张图只是静默不参与生成。CLI 会在发请求前比一遍并提醒，但不拦（你可能是故意留着的）。
- 同名参考图是硬错误：两张同名图必然有一张永远选不中。
- 本地/base64 引用按 `-f` → `--base64-file` → `--base64-data` 的顺序上传，公网 URL 随 submit 追加。跨类型使用时应全部命名并通过 `[@名称]` 指代，不要依赖位置猜测。

## 出口码

脚本里最常见的分支是「额度不够」「参数写错了」「任务真的失败了」和「只是还没跑完」，所以刻意分开：

| 码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 1 | 本地错误（读文件、写磁盘、没配 Key……） |
| 2 | 用法错误 |
| 3 | 认证失败（HTTP 401 / 403） |
| 4 | 额度不足（HTTP 402） |
| 5 | 参数不合法（HTTP 400，或本地校验拦下） |
| 6 | 被限流（HTTP 429） |
| 7 | 服务端错误（HTTP 5xx） |
| 8 | 任务以 `status=failed` 结束 |
| 9 | 等待超时 —— **任务还在跑**，不是失败 |

注意 8 和 9 的区别：HTTP 200 不代表成功，`status=failed` 才是任务失败；而超时只是本地不等了，任务仍在服务端继续跑，稍后 `verge-cli task get` 还能拿到图。

## 脚本化

`--json` 原样透传服务端响应体，服务端新增字段管道下游立刻能看到（不是把客户端结构体重新编码一遍）。人类可读的进度、警告一律走 stderr，stdout 永远只有数据，可以直接接 `jq`：

```bash
# 取所有图片链接
verge-cli task get "$id" --json | jq -r '.data[].url'

# 按出口码分支
verge-cli task create "$prompt" --wait -o ./out
case $? in
  0) echo "出图成功" ;;
  4) echo "额度不足，先充值"; exit 1 ;;
  8) echo "任务失败，看上面的 error code" ;;
   9) echo "还没跑完，稍后 verge-cli task get 再看" ;;
  *) echo "其他错误" ; exit 1 ;;
esac
```

`--verbose` 把每个 HTTP 请求打到 stderr，排查自建部署的路由问题很有用。

## 开发

本机装了 Go 1.25+ 可以直接编译 —— 零依赖，不联网也能编：

```bash
go build -o verge-cli .        # Windows: go build -o verge-cli.exe .
```

没装 Go，或想要可复现的容器构建时，全部经由 Docker：

```bash
./scripts/go.sh build ./...
./scripts/go.sh test ./... -count=1
./scripts/go.sh vet ./...
./scripts/go.sh fmt ./...
./scripts/build.sh v0.1.0        # 交叉编译到 dist/
```

Windows PowerShell 用 `.\scripts\go.ps1`，参数一致。镜像可用 `VERGE_CLI_GO_IMAGE` 覆盖。

### Dev container

仓库带 `.devcontainer`：编辑器（VS Code / opencode）打开仓库后选 "Reopen in Container" 即可一键进入。容器内已装好 Go 1.26，直接用 `go` 命令：

```bash
go build ./...
go test ./... -count=1
go vet ./...
```

workspace 是双向挂载，容器里编译出的二进制就落在本机文件系统上 —— 开发环境在容器，产物给本机用。不进 dev container 时（裸终端）一律用上面的 `go.sh` / `go.ps1`；容器里没有 docker CLI，不要嵌套调用。

注意：dev container（以及 `go.sh` 的容器）是 **Linux 环境**，里面直接 `go build` 产出的是 Linux 二进制，Windows 本机跑不了。要 Windows 产物就显式交叉编译：`GOOS=windows GOARCH=amd64 go build -o verge-cli.exe .`（`CGO_ENABLED=0` + 零依赖，交叉编译无门槛）。裸 `go build` 不注入版本号，git 仓库里会显示带 commit 的伪版本号；要真实版本号走 `build.sh <版本号>`。

结构：

```
main.go                  入口，只负责把出口码交给 os.Exit
internal/app/            命令行表层：flag 解析、渲染、出口码映射
internal/vergeapi/       Verge API 客户端：请求、重试策略、轮询、校验
internal/imagefile/      本地图片探测（内容类型、宽高）
internal/config/         配置文件与优先级解析
scripts/                 Docker 里跑 Go 的封装
```

**零第三方依赖是硬约束。** 只用标准库，所以在任何 `golang` 容器里都能离线 `go build`，也不需要 `go.sum` 审计。测试同样只用 `testing` + `net/http/httptest`。

## 设计取舍

几个刻意为之的决定，改代码前值得先知道：

- **POST 永不重试。** `--retries` 只作用于 GET 和预签名 PUT；PUT 仅重试传输错误及 429、500、502、503、504。提交结果不明确时查询已有 `task_id`，不要重复 POST。
- **预签名 PUT 不带 `Authorization`。** 那些 URL 指向对象存储，把 Verge API Key 发到第三方域名就是凭证泄露。同理，下载结果图也不带。
- **未知状态一律当成非终态。** 服务端将来新增中间状态时，客户端不会误判成成功或失败。
- **未知模型放行，已知模型才校验分辨率。** 本地那张模型表只用来「少跑一次往返就能发现的错」，不是权威来源 —— 权威是 `verge-cli models`。
- **不把额度换算成钱。** 额度与金额的比例是服务端配置项，这几个接口都不返回，客户端乘一个猜的系数只会给出看起来很精确的错数字。
- **位置参数会重排到 flag 之后。** Go 的 `flag` 遇到第一个位置参数就停止解析，不重排的话 `verge-cli task create "提示词" --wait -o ./out` 会把 `-o ./out` 静默拼进提示词，生成一张带参数文本的图 —— 属于最坏的一类错误。
- **超 10 MiB 的本地或 base64 参考图自动重编码成 JPEG。** 对象存储对预签名 PUT 有每文件 10 MiB 硬上限（服务端回 413）。压缩只在本地文件或 base64 解码结果超限时触发，用标准库 `image/jpeg` 的质量参数逐档降质，自带的 box 平均降采样兜底 —— 零依赖。

## 相关项目

- [verge-skill](https://github.com/img-verge/verge-skill) —— 教 AI agent 使用 verge-cli 的 Agent Skill

## 许可证

[AGPL-3.0](./LICENSE)
