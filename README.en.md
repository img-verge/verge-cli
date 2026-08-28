<div align="center">

# verge-cli

**A command-line client for the [Verge API](https://api.verge-ai.xyz) image endpoints**

English · [简体中文](./README.md)

</div>

---

`verge-cli` wraps the Verge API image endpoints in a single command-line tool: list models, check quota, create image tasks, automate three-stage uploads, poll, and download results.

Zero third-party dependencies, one static binary, no runtime to install.

```bash
export VERGE_API_KEY=sk-...

verge-cli task create "neon-lit street on a rainy night, cyberpunk, cinematic" -r 2k -a 16:9 --wait -o ./out
```

## Contents

- [Install](#install)
- [Configuration](#configuration)
- [Commands](#commands)
  - [balance](#balance)
  - [quota](#quota)
  - [models](#models)
  - [task](#task)
  - [download](#download)
  - [config](#config)
- [Reference images and `[@name]`](#reference-images-and-name)
- [Exit codes](#exit-codes)
- [Scripting](#scripting)
- [Development](#development)
- [Design decisions](#design-decisions)
- [License](#license)

## Install

### Prebuilt binaries

Download the archive for your platform from the [Releases page](https://github.com/img-verge/verge-cli/releases), extract it, and put the `verge-cli` binary on your `PATH`:

| Archive | Platform |
|---------|----------|
| `verge-cli_v1.0.0_windows_amd64.zip` | Windows 64-bit |
| `verge-cli_v1.0.0_linux_amd64.tar.gz` | Linux 64-bit |
| `verge-cli_v1.0.0_darwin_amd64.tar.gz` | macOS Intel |
| `verge-cli_v1.0.0_darwin_arm64.tar.gz` | macOS Apple Silicon |

### With Go

Requires Go 1.25+ (the version declared in `go.mod`):

```bash
go install github.com/img-verge/verge-cli@latest
```

### From source

This repo does not require a local Go installation — everything compiles inside Docker:

```bash
./scripts/go.sh build -o verge-cli .        # Windows: .\scripts\go.ps1 build -o verge-cli.exe .
./scripts/check.sh                      # vet + test, run this before committing (Windows: .\scripts\check.ps1)
./scripts/build.sh v0.1.0               # cross-compile every platform into dist/, version stamped
```

`go.sh build` does not stamp a version, so the resulting binary reports `verge-cli dev`. Use `build.sh <version>` for a stamped build; the output lands in `dist/`.

## Configuration

Precedence is always **command-line flag > environment variable > config file > built-in default**.

| Setting | Flag | Environment | `config set` key | Built-in default |
| --- | --- | --- | --- | --- |
| API key | `--api-key` | `VERGE_API_KEY` | `api-key` | — |
| Endpoint | `--base-url` | `VERGE_API_BASE_URL` | `base-url` | `https://api.verge-ai.xyz/v1` |
| Model | `-m` / `--model` | — | `model` | `gpt-image-2` |
| Resolution | `-r` / `--resolution` | — | `resolution` | `1080p` |
| Aspect ratio | `-a` / `--aspect-ratio` | — | `aspect-ratio` | `1:1` |

The one-time setup:

```bash
verge-cli config set api-key sk-...
verge-cli config set model gpt-image-2
verge-cli config set resolution 2k
verge-cli config show
```

Config file location (`verge-cli config path` prints the resolved one):

- Windows: `%APPDATA%\verge\config.json`
- macOS / Linux: `$XDG_CONFIG_HOME/verge/config.json`, falling back to `~/.config/verge/config.json`
- `VERGE_CONFIG` overrides the path entirely

The file holds your API key in plaintext, so it is written with `0600` permissions. `verge-cli config show` only ever prints a masked key — safe to paste into an issue.

For a self-hosted deployment, point it at your own gateway; a missing `/v1` is appended for you:

```bash
verge-cli config set base-url https://gateway.example.com
```

## Commands

Global flags work on either side of the subcommand: `verge-cli --json balance` and `verge-cli balance --json` are identical.

`verge-cli help` for the overview, `verge-cli help <command>` for a command's full flag list.

### balance

```console
$ verge-cli balance
wallet available  1,234,567
key available     unlimited
```

`key available: unlimited` means this key has no per-key cap of its own; `0` means it has a cap and has spent it. Those are very different situations, so they render differently.

Human-readable output ends with a `request_id <id>` line taken from the gateway's `X-Oneapi-Request-Id` response header (error bodies do not always carry it, the header always does). Include it when reporting an issue so support can find the exact call. `--json` stays a pure passthrough and never injects a request_id.

### quota

Asks "what would this image task pre-charge?" without creating a task or spending anything. It runs the same pricing code path as a real submission:

```console
$ verge-cli quota -m gpt-image-2 -r 2k -a 16:9 -n 2
model              gpt-image-2
resolution         2k
aspect ratio       16:9
images             2
pre-charged quota  8,000
```

### models

Lists the models this key can **actually** use, after user, group, and per-key model restrictions have all been applied:

```console
$ verge-cli models
MODEL                           RESOLUTIONS    ENDPOINTS
gemini-3-pro-image-preview      1080p, 2k, 4k  image-generation, openai
gemini-3.1-flash-image-preview  1080p, 2k, 4k  image-generation, openai
gemini-3.1-flash-lite-image     1080p          image-generation, openai
gpt-image-2                     1080p, 2k, 4k  image-generation, openai

4 model(s). RESOLUTIONS comes from this CLI's built-in table, not the API.
```

By default only models that declare image-task support are shown; `--all` shows everything.

`RESOLUTIONS` comes from a table compiled into the CLI rather than from the API, which is why the output says so — new server-side models will show up here before that column knows about them.

### task

`verge-cli task create` is the only image-task entry point. It can return a task id immediately, or wait and download results with `--wait`.

Submit now, collect later — suitable for interactive use, scripts, and batches:

```bash
# returns a task id immediately
verge-cli task create "neon-lit street on a rainy night"

# create, wait for the images, download them
verge-cli task create "neon-lit street on a rainy night" --wait -o ./out

# with local references: prepare → direct upload → submit, all handled for you
verge-cli task create "turn [@subject] into a movie poster" -f subject=./a.png --wait -o ./out

# public URLs are submitted directly and may be mixed with local/base64 references
verge-cli task create "place [@character] into [@background]" -f character=./char.png -u background=https://example.com/bg.png --wait

# base64 files and inline data URIs are decoded, then use the same three-stage upload
verge-cli task create "use the style of [@style]" --base64-file style=./style.b64.txt --wait -o ./out
verge-cli task create "use [@texture]" --base64-data 'texture=data:image/png;base64,iVBOR...' --wait -o ./out

# inspect an existing task
verge-cli task get task_xxx
verge-cli task get task_xxx --wait -o ./out
```

`-o` implies `--wait`: a "download" flag that quietly downloads nothing is far worse than waiting a bit longer.

`task create` accepts `--prompt-file PATH` (`-` means stdin). A positional prompt wins; prompt files are limited to 1 MiB and must contain valid UTF-8 text.

Three things worth knowing about the three-stage upload:

1. **No POST is retried automatically.** With only public URLs or no references, the CLI creates the task directly. If any `-f`, `--base64-file`, or `--base64-data` reference exists, it runs `prepare → presigned PUT → submit`. Once prepare succeeds, a `task_id` exists and later errors print it to stderr. If submit times out, disconnects, or returns 5xx, do not submit again; query the original task with `verge-cli task get <task_id>`.
2. **`--retries` applies only to GET and presigned PUT.** PUT retries transport errors and 429, 500, 502, 503, and 504 only. Every attempt preserves the image bytes, `Content-Type`, and `Content-Length`, and never sends the Verge API key. Upload failure reporting is best-effort diagnostics; submit is not called, the original task remains in the uploading phase until timeout, and rerunning the complete task obtains fresh upload slots.
3. **Local files and decoded base64 payloads share the 10 MiB per-file ceiling.** Oversized payloads use the same JPEG quality-reduction/downsampling path and produce a warning on stderr. Re-encoding drops alpha channels and animation frames; compress references locally first when those must be preserved.

### download

```bash
verge-cli download task_xxx -o ./out --prefix poster
```

Result URLs expire after 7 days (1 day for covers). This command re-fetches the task for fresh URLs rather than reusing whatever you saved earlier.

Plenty of object stores answer an expired signature with HTTP 200 and an XML error body, so downloads check `Content-Type` and fail loudly instead of saving an error document as a `.png`.

### config

```bash
verge-cli config show          # effective values (key masked)
verge-cli config path          # where the config file lives
verge-cli config set <key> <value>
verge-cli config unset <key>
```

An unsupported `aspect-ratio` is rejected outright — it's a closed enum in the API docs, so storing a bad one would break every future image task. `model` and `resolution` only warn: the server adds new models and tiers on its own schedule, and hard-rejecting them would date the CLI immediately.

## Reference images and `[@name]`

Up to 7 reference images, counting local files, base64 images, and public URLs together.

```bash
# unnamed: they apply in the order given
verge-cli task create "blend the style of these two" -f ./a.png -f ./b.png --wait

# named: address them precisely from the prompt with [@name]
verge-cli task create "place [@character] into [@background]" \
  -f character=./char.png \
  -u background=https://example.com/bg.png \
  --wait -o ./out

# named base64: both raw standard base64 and data URIs are accepted
verge-cli task create "color [@lineart]" --base64-file lineart=./lineart.b64.txt --wait
verge-cli task create "use [@palette]" --base64-data 'palette=iVBORw0KGgoAAA...' --wait
```

- `-f` / `--file` reads local files, `-u` / `--image-url` passes public URLs, `--base64-file` reads base64 text files, and `--base64-data` accepts raw standard base64 or `data:image/*;base64,...` on the command line. All four repeat and may be mixed.
- `name=value` is the named form. Paths and URLs are treated as named only when the text before the first `=` has no path separator, so `C:\pics\a=b.png` and `https://x/y?k=v` are not misread. `--base64-data` first tries the complete argument as base64/data URI, so trailing `=` or `==` padding is never mistaken for a name separator.
- The server matches `[@name]` **exactly**. A typo isn't an error — that image simply doesn't participate. The CLI compares them before sending and warns, but doesn't block (you may have left it there deliberately).
- Duplicate names are a hard error: with two images under one name, one of them can never be selected.
- Local/base64 references upload in deterministic `-f` → `--base64-file` → `--base64-data` order; public URLs are appended at submit. Across types, name everything and use `[@name]` instead of guessing by position.

## Exit codes

The branches scripts actually care about are "out of quota", "bad parameters", "the task really failed", and "it just isn't done yet", so those are distinct:

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Local error (reading a file, writing to disk, no key configured, …) |
| 2 | Usage error |
| 3 | Authentication failed (HTTP 401 / 403) |
| 4 | Insufficient quota (HTTP 402) |
| 5 | Invalid parameters (HTTP 400, or caught by local validation) |
| 6 | Rate limited (HTTP 429) |
| 7 | Server error (HTTP 5xx) |
| 8 | Task finished with `status=failed` |
| 9 | Wait timed out — **the task is still running**, this is not a failure |

Mind the gap between 8 and 9: HTTP 200 does not mean success — `status=failed` is how a task fails; and a timeout only means this process stopped waiting. The task keeps running server-side and `verge-cli task get` will still hand you the images later.

## Scripting

`--json` passes the server's response body through verbatim, so new server-side fields reach your pipeline immediately (it is not a re-encoding of client structs). Human-readable progress and warnings all go to stderr; stdout carries nothing but data, so it pipes into `jq` directly:

```bash
# every result URL
verge-cli task get "$id" --json | jq -r '.data[].url'

# branch on the exit code
verge-cli task create "$prompt" --wait -o ./out
case $? in
  0) echo "images ready" ;;
  4) echo "out of quota, top up first"; exit 1 ;;
  8) echo "task failed, see the error code above" ;;
   9) echo "still running, check verge-cli task get later" ;;
  *) echo "something else went wrong"; exit 1 ;;
esac
```

`--verbose` logs every HTTP request to stderr, which is what you want when debugging routing on a self-hosted deployment.

## Development

With Go 1.25+ installed you can compile directly — zero dependencies, works offline too:

```bash
go build -o verge-cli .        # Windows: go build -o verge-cli.exe .
```

Without Go, or for reproducible container builds, everything runs through Docker:

```bash
./scripts/go.sh build ./...
./scripts/go.sh test ./... -count=1
./scripts/go.sh vet ./...
./scripts/go.sh fmt ./...
./scripts/build.sh v0.1.0        # cross-compile into dist/
```

On Windows PowerShell use `.\scripts\go.ps1` with the same arguments. Override the image with `VERGE_CLI_GO_IMAGE`.

### Dev container

The repo ships a `.devcontainer`: open it with "Reopen in Container" (VS Code / opencode) to get a one-click development environment. Go 1.26 is already installed inside, so use `go` directly:

```bash
go build ./...
go test ./... -count=1
go vet ./...
```

The workspace is a two-way mount, so binaries built inside the container land on the host filesystem — the development environment lives in the container while the artifacts stay on your machine. On a bare host terminal always use `go.sh` / `go.ps1`; the container has no docker CLI, so do not nest calls into it.

Note that the dev container (and the `go.sh` container) is a **Linux environment**: a bare `go build` inside it produces a Linux binary that a Windows host cannot run. To get a Windows artifact, cross-compile explicitly: `GOOS=windows GOARCH=amd64 go build -o verge-cli.exe .` (trivial thanks to `CGO_ENABLED=0` and zero dependencies). A bare `go build` injects no version — inside a git checkout it reports a VCS pseudo-version instead of `dev`; use `build.sh <version>` for the real version string.

Layout:

```
main.go                  entry point; hands the exit code to os.Exit and nothing else
internal/app/            CLI surface: flag parsing, rendering, exit-code mapping
internal/vergeapi/       Verge API client: requests, retry policy, polling, validation
internal/imagefile/      local image probing (content type, dimensions)
internal/config/         config file and precedence resolution
scripts/                 wrappers for running Go inside Docker
```

**Zero third-party dependencies is a hard constraint.** Standard library only, so `go build` works offline in any `golang` container and there is no `go.sum` to audit. Tests use `testing` + `net/http/httptest` for the same reason.

## Design decisions

A few deliberate choices worth knowing before you change the code:

- **POST requests are never retried.** `--retries` applies to GETs and presigned PUTs; PUT retries only transport errors and 429, 500, 502, 503, and 504. Create and submit are not retried automatically because an ambiguous submission may have side effects.
- **Presigned PUTs carry no `Authorization` header.** Those URLs point at object storage; sending a Verge API key to a third-party domain is a credential leak. Result downloads are unauthenticated for the same reason.
- **Unknown statuses are treated as non-terminal.** When the server adds an intermediate state, the client won't mistake it for success or failure.
- **Unknown models pass; only known models get their resolution checked.** That built-in table exists to catch mistakes without a round trip — it is not the authority. `verge-cli models` is.
- **Quota is never converted to money.** The quota-per-unit ratio is server-side configuration and none of these endpoints return it; multiplying by a guessed factor would just produce a precise-looking wrong number.
- **Positional arguments are permuted after flags.** Go's `flag` stops parsing at the first positional argument, so without permutation `verge-cli task create "prompt" --wait -o ./out` would silently append `-o ./out` to the prompt and create a picture with your flags written in it — the worst class of bug.
- **Local and base64 reference images over 10 MiB are re-encoded as JPEG automatically.** Object storage enforces a 10 MiB per-file ceiling on presigned PUTs (the server answers 413). Compression only fires when a local file or decoded base64 payload exceeds the limit: stdlib `image/jpeg` quality stepping first, then a dependency-free box-average downsample as fallback — zero third-party deps.

## Related projects

- [verge-skill](https://github.com/img-verge/verge-skill) — Agent Skill that teaches AI coding assistants to use verge-cli

## License

[AGPL-3.0](./LICENSE), same as the Verge API main repository.
