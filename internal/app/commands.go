package app

import (
	"fmt"
	"io"
	"strings"
)

// command is one top-level entry in the CLI.
type command struct {
	name    string
	summary string
	usage   string
	run     func(*env, []string) error
}

func commands() []command {
	return []command{
		{
			name:    "models",
			summary: "list the image models this API key may use",
			usage:   usageModels,
			run:     runModels,
		},
		{
			name:    "balance",
			summary: "show wallet and per-key remaining quota",
			usage:   usageBalance,
			run:     runBalance,
		},
		{
			name:    "quota",
			summary: "estimate the quota an image task would pre-charge",
			usage:   usageQuota,
			run:     runQuota,
		},
		{
			name:    "task",
			summary: "create, submit and query asynchronous tasks",
			usage:   usageTask,
			run:     runTask,
		},
		{
			name:    "download",
			summary: "download the result images of a finished task",
			usage:   usageDownload,
			run:     runDownload,
		},
		{
			name:    "config",
			summary: "read and write the local config file",
			usage:   usageConfig,
			run:     runConfig,
		},
		{
			name:    "version",
			summary: "print the CLI version",
			usage:   usageVersion,
			run:     runVersion,
		},
	}
}

func lookupCommand(name string) (command, bool) {
	for _, cmd := range commands() {
		if cmd.name == name {
			return cmd, true
		}
	}
	return command{}, false
}

func printRootUsage(w io.Writer) {
	fmt.Fprint(w, "usage: verge-cli [global flags] <command> [args]\nrun `verge-cli help` for the full command list\n")
}

func printRootHelp(w io.Writer) {
	var b strings.Builder
	b.WriteString("verge-cli — command line client for the Verge API image endpoints\n\n")
	b.WriteString("usage:\n  verge-cli [global flags] <command> [args]\n\ncommands:\n")
	for _, cmd := range commands() {
		b.WriteString(fmt.Sprintf("  %-9s %s\n", cmd.name, cmd.summary))
	}
	b.WriteString(`
global flags:
  --api-key KEY       API key; falls back to $VERGE_API_KEY then the config file
  --base-url URL      API base URL; /v1 is appended when missing
                      falls back to $VERGE_API_BASE_URL, the config file,
                      then https://api.verge-ai.xyz/v1
  --json              print the raw API response instead of a human summary
  --timeout DURATION  per-request HTTP timeout (default 10m)
  --retries N         retry attempts for idempotent requests (default 2);
                      task creation and submission are never retried;
                      only GET queries and presigned PUTs retry
  --skip-validate     skip client-side parameter checks
  -v, --verbose       log every HTTP request to stderr
  -h, --help          show help

Global flags work before or after the command: ` + "`verge-cli --json balance`" + ` and
` + "`verge-cli balance --json`" + ` are equivalent.

exit codes:
  0 success            5 invalid parameters (HTTP 400 or a local check)
  1 local error        6 rate limited (HTTP 429)
  2 bad usage          7 server error (HTTP 5xx)
  3 auth error         8 task finished with status=failed
  4 out of quota       9 timed out waiting; the task is still running

examples:
	export VERGE_API_KEY=sk-...
  verge-cli balance
  verge-cli quota --resolution 2k --aspect-ratio 16:9
  verge-cli task create "a neon city street after rain" --wait -o ./out
  verge-cli task create "turn [@subject] into a movie poster" -f subject=./a.png --wait
  verge-cli task get task_xxx --wait -o ./out

run ` + "`verge-cli help <command>`" + ` for per-command flags
`)
	fmt.Fprint(w, b.String())
}

func printHelpFor(stdout, stderr io.Writer, args []string) bool {
	if len(args) == 0 {
		printRootHelp(stdout)
		return true
	}
	cmd, ok := lookupCommand(args[0])
	if !ok {
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		fmt.Fprintln(stderr, hintBlock(args[0], rootCommandNames(), ""))
		printRootUsage(stderr)
		return false
	}
	// 二级命令（task create / task get）的帮助由 task 自己分发。
	if cmd.name == "task" && len(args) > 1 {
		if sub, found := lookupTaskCommand(args[1]); found {
			fmt.Fprint(stdout, sub.usage)
			return true
		}
		fmt.Fprintf(stderr, "unknown task subcommand %q\n", args[1])
		fmt.Fprintln(stderr, hintBlock(args[1], taskCommandNames(), "tip: run `verge-cli help task` for the subcommand list"))
		if line, _, found := strings.Cut(usageTask, "\n"); found {
			fmt.Fprintln(stderr, line)
		}
		return false
	}
	fmt.Fprint(stdout, cmd.usage)
	return true
}
