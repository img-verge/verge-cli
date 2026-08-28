// Package app implements the verge-cli command line surface.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/img-verge/verge-cli/internal/config"
	"github.com/img-verge/verge-cli/internal/vergeapi"
)

// version is overridden at build time with -ldflags "-X ...app.version=vX.Y.Z".
var version = ""

// Exit codes. 刻意分得比较细：脚本里最常见的分支就是「额度不够」「参数写错了」
// 「任务真的失败了」和「只是还没跑完」，把它们塌成同一个 1 会让调用方只能去 grep 文案。
const (
	ExitOK           = 0
	ExitError        = 1
	ExitUsage        = 2
	ExitAuth         = 3
	ExitQuota        = 4
	ExitInvalidInput = 5
	ExitRateLimited  = 6
	ExitServer       = 7
	ExitTaskFailed   = 8
	ExitWaitTimeout  = 9
)

// globals are the flags every subcommand accepts.
type globals struct {
	apiKey       string
	baseURL      string
	jsonOut      bool
	verbose      bool
	timeout      time.Duration
	retries      int
	skipValidate bool
	showHelp     bool
}

// env is the shared execution context handed to each command.
type env struct {
	ctx    context.Context
	stdout io.Writer
	stderr io.Writer
	global globals
	cfg    config.Resolved

	// lastRaw holds the most recent successful response body, for --json passthrough.
	lastRaw []byte
}

// taskFailedError marks a task that reached status == failed, so Run can map it onto a
// dedicated exit code without the command layer knowing about exit codes.
type taskFailedError struct{ task *vergeapi.Task }

func (e *taskFailedError) Error() string {
	if e.task != nil && e.task.Error != nil {
		detail := e.task.Error.Message
		if e.task.Error.Code != "" {
			detail += " (" + e.task.Error.Code + ")"
		}
		return "task " + e.task.TaskID + " failed: " + detail
	}
	return "task failed"
}

// Run executes one CLI invocation and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	// Ctrl-C 要能中断长轮询和大文件上传，而不是等 HTTP 超时。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	code, err := run(ctx, args, stdout, stderr)
	if err != nil {
		switch {
		// --help 已经把用法打到 stdout 了，这里不能再补一句 error。
		case errors.Is(err, errHelpShown):
			return code
		case errors.Is(err, context.Canceled):
			fmt.Fprintln(stderr, "aborted")
			return ExitError
		}
		fmt.Fprintln(stderr, "error: "+err.Error())

		// 用法错误先打建议块（did-you-mean / available / tip），再补一行 usage 首行，
		// 不把整页帮助刷出来。
		var usageErr *usageError
		if errors.As(err, &usageErr) {
			if usageErr.hint != "" {
				fmt.Fprintln(stderr, usageErr.hint)
			}
			if usageErr.usage != "" {
				if line, _, found := strings.Cut(usageErr.usage, "\n"); found {
					fmt.Fprintln(stderr, line)
				}
			}
		}
	}
	return code
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	var g globals
	g.timeout = 0
	g.retries = 2

	rootFS := flag.NewFlagSet("verge", flag.ContinueOnError)
	rootFS.SetOutput(io.Discard)
	registerGlobals(rootFS, &g)
	if err := rootFS.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printRootHelp(stdout)
			return ExitOK, nil
		}
		fmt.Fprintln(stderr, "error: "+err.Error())
		printRootUsage(stderr)
		return ExitUsage, nil
	}

	rest := rootFS.Args()
	if g.showHelp && len(rest) == 0 {
		printRootHelp(stdout)
		return ExitOK, nil
	}
	if len(rest) == 0 {
		printRootUsage(stderr)
		return ExitUsage, nil
	}

	name, rest := rest[0], rest[1:]
	if name == "help" {
		if !printHelpFor(stdout, stderr, rest) {
			return ExitUsage, nil
		}
		return ExitOK, nil
	}
	cmd, ok := lookupCommand(name)
	if !ok {
		fmt.Fprintf(stderr, "error: unknown command %q\n", name)
		fmt.Fprintln(stderr, hintBlock(name, rootCommandNames(), ""))
		printRootUsage(stderr)
		return ExitUsage, nil
	}

	e := &env{ctx: ctx, stdout: stdout, stderr: stderr, global: g}
	err := cmd.run(e, rest)
	if err != nil {
		return classify(err), err
	}
	return ExitOK, nil
}

// registerGlobals binds the global flags onto fs, using the current values as defaults.
//
// 同一组全局 flag 会注册两次：一次在根 FlagSet，一次在子命令 FlagSet。因为默认值取的
// 是当前变量值，`verge-cli --json balance` 和 `verge-cli balance --json` 效果完全一致，而子命令
// 上没写的 flag 也不会把根上已经解析出来的值覆盖回零值。
func registerGlobals(fs *flag.FlagSet, g *globals) {
	fs.StringVar(&g.apiKey, "api-key", g.apiKey, "API key (default: $VERGE_API_KEY, then the config file)")
	fs.StringVar(&g.baseURL, "base-url", g.baseURL, "API base URL, /v1 is appended when missing (default: $VERGE_API_BASE_URL, then the config file)")
	fs.BoolVar(&g.jsonOut, "json", g.jsonOut, "print the raw API response instead of a human summary")
	fs.BoolVar(&g.verbose, "verbose", g.verbose, "log every HTTP request to stderr")
	fs.BoolVar(&g.verbose, "v", g.verbose, "shorthand for --verbose")
	fs.DurationVar(&g.timeout, "timeout", g.timeout, "per-request HTTP timeout (default 10m; image tasks can take minutes)")
	fs.IntVar(&g.retries, "retries", g.retries, "retry attempts for idempotent requests; writes are never retried")
	fs.BoolVar(&g.skipValidate, "skip-validate", g.skipValidate, "skip client-side parameter checks and let the server decide")
	fs.BoolVar(&g.showHelp, "help", g.showHelp, "show help")
	fs.BoolVar(&g.showHelp, "h", g.showHelp, "shorthand for --help")
}

// newFlagSet builds a subcommand FlagSet that also accepts the global flags.
func (e *env) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("verge-cli "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerGlobals(fs, &e.global)
	return fs
}

// parse runs fs and turns --help into a clean help print.
//
// 参数会先重排，让 flag 出现在位置参数之后也能生效。标准库 flag 遇到第一个非 flag
// 参数就停止解析，而 `verge-cli task create "提示词" -o ./out` 是最自然的写法 —— 不重排的话
// `-o ./out` 会被静默拼进提示词，生成一张带着参数文本的图，属于最坏的一类错误。
func (e *env) parse(fs *flag.FlagSet, args []string, usage string) error {
	return e.parseInOrder(fs, permuteArgs(fs, args), usage)
}

// parseSubcommand parses only the leading global flags and leaves the subcommand name
// plus everything after it in fs.Args().
//
// 分发层不能重排：此时子命令自己的 flag 还没注册，重排会把它们当成未定义 flag 报错。
func (e *env) parseSubcommand(fs *flag.FlagSet, args []string, usage string) error {
	return e.parseInOrder(fs, args, usage)
}

func (e *env) parseInOrder(fs *flag.FlagSet, args []string, usage string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(e.stdout, usage)
			return errHelpShown
		}
		return &usageError{err: err, usage: usage, hint: flagErrorHint(fs.Name(), err)}
	}
	if e.global.showHelp {
		fmt.Fprint(e.stdout, usage)
		return errHelpShown
	}
	return nil
}

// permuteArgs moves every flag (and its value) ahead of the positional arguments.
//
// 需要知道哪些 flag 吃后面一个参数，所以只能在 flag 全部注册完之后调用。未知 flag 一律
// 当作不吃值，交给 fs.Parse 去报“未定义”。`--` 之后的内容原样保留为位置参数。
func permuteArgs(fs *flag.FlagSet, args []string) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))

	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positional = append(positional, args[index+1:]...)
			break
		}
		// 单个 "-" 是 stdin 的惯例写法，不是 flag。
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.ContainsRune(name, '=') {
			continue
		}
		if flagNeedsValue(fs, name) && index+1 < len(args) {
			index++
			flags = append(flags, args[index])
		}
	}

	if len(positional) == 0 {
		return flags
	}
	// 补一个 "--"：位置参数本身可能以 - 开头（比如以减号开头的提示词）。
	return append(append(flags, "--"), positional...)
}

// flagNeedsValue reports whether a flag consumes the next argument.
func flagNeedsValue(fs *flag.FlagSet, name string) bool {
	formal := fs.Lookup(name)
	if formal == nil {
		return false
	}
	if boolFlag, ok := formal.Value.(interface{ IsBoolFlag() bool }); ok && boolFlag.IsBoolFlag() {
		return false
	}
	return true
}

// errHelpShown is a sentinel: help was printed, exit successfully and print nothing else.
var errHelpShown = errors.New("help shown")

// usageError is a bad invocation: print the message plus the command's usage.
type usageError struct {
	err   error
	msg   string
	usage string
	hint  string
}

func (e *usageError) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return e.err.Error()
}

func usageErrorf(usage, format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...), usage: usage}
}

// unknownUsageError builds a usage error for an unknown name, with the suggestion block.
func unknownUsageError(usage, msg, name string, candidates []string, helpTip string) error {
	return &usageError{msg: msg, usage: usage, hint: hintBlock(name, candidates, helpTip)}
}

// hintBlock renders the "did you mean / available / tip" block for an unknown name.
func hintBlock(name string, candidates []string, helpTip string) string {
	var b strings.Builder
	if suggestion := suggest(name, candidates); suggestion != "" {
		fmt.Fprintf(&b, "tip: did you mean %q?\n", suggestion)
	}
	b.WriteString("available: " + strings.Join(candidates, ", ") + "\n")
	if helpTip != "" {
		b.WriteString(helpTip + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// flagErrorHint suggests --help when the flag itself was not understood.
func flagErrorHint(fsName string, err error) string {
	if strings.Contains(err.Error(), "flag provided but not defined") {
		return fmt.Sprintf("tip: run `%s --help` for the available flags", fsName)
	}
	return ""
}

// rootCommandNames lists every top-level command, sorted, for the suggestion block.
func rootCommandNames() []string {
	names := make([]string, 0, len(commands()))
	for _, cmd := range commands() {
		names = append(names, cmd.name)
	}
	sort.Strings(names)
	return names
}

// suggest returns the closest candidate within the edit-distance threshold, or "".
// 阈值按候选名长度缩放：短名字差 2 步就可能完全是另一个词，长名字放宽到 3。
func suggest(name string, candidates []string) string {
	best := ""
	bestDistance := -1
	for _, candidate := range candidates {
		distance := levenshtein(name, candidate)
		threshold := 2
		if len(candidate) >= 8 {
			threshold = 3
		}
		if distance <= threshold && (bestDistance == -1 || distance < bestDistance) {
			best, bestDistance = candidate, distance
		}
	}
	return best
}

// levenshtein is the edit distance between two strings (zero-dependency DP).
func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	previous := make([]int, len(br)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		current := make([]int, len(br)+1)
		current[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			current[j] = min3(current[j-1]+1, previous[j]+1, previous[j-1]+cost)
		}
		previous = current
	}
	return previous[len(br)]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

// stepf annotates an error with the operation that failed, so multi-step flows
// (prepare → upload → submit → poll → download) say which step broke. %w 保留 unwrap
// 链，classify 的 errors.As 依然命中类型化错误，出口码契约不变。
func stepf(step string, err error) error { return fmt.Errorf("%s: %w", step, err) }

// client builds an API client from the resolved configuration.
//
// 每个需要联网的命令各自调一次：配置解析放在这里而不是 Run 里，`verge-cli config`、
// `verge-cli version` 这些不需要 Key 的命令才不会因为没配 Key 就直接失败。
func (e *env) client() (*vergeapi.Client, error) {
	if _, err := e.resolveConfig(); err != nil {
		return nil, err
	}
	if e.cfg.APIKey == "" {
		return nil, errConfigMissing
	}

	client, err := vergeapi.New(vergeapi.Options{
		BaseURL:    e.cfg.BaseURL,
		APIKey:     e.cfg.APIKey,
		Timeout:    e.global.timeout,
		UserAgent:  "verge-cli/" + Version(),
		MaxRetries: e.global.retries,
	})
	if err != nil {
		return nil, err
	}
	if e.global.verbose {
		client.Trace = func(method, target string, status int, elapsed time.Duration) {
			if status == 0 {
				fmt.Fprintf(e.stderr, "» %s %s failed after %s\n", method, target, elapsed.Round(time.Millisecond))
				return
			}
			fmt.Fprintf(e.stderr, "» %s %s → %d in %s\n", method, target, status, elapsed.Round(time.Millisecond))
		}
	}
	client.OnResponse = func(body []byte) {
		e.lastRaw = append(e.lastRaw[:0], body...)
	}
	return client, nil
}

// emitRaw prints the last successful response body, pretty-printed.
//
// 透传原始体而不是把结构体重新编码一遍：服务端加了字段，管道下游立刻就能看到。
func (e *env) emitRaw() error {
	if len(e.lastRaw) == 0 {
		return nil
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, e.lastRaw, "", "  "); err != nil {
		// 不是合法 JSON 就原样吐出去，别在这里把内容吃掉。
		fmt.Fprintln(e.stdout, strings.TrimRight(string(e.lastRaw), "\r\n"))
		return nil
	}
	fmt.Fprintln(e.stdout, strings.TrimRight(pretty.String(), "\r\n"))
	return nil
}

// infof writes progress to stderr so stdout stays pipeable, including under --json.
func (e *env) infof(format string, args ...any) {
	fmt.Fprintf(e.stderr, format+"\n", args...)
}

// warnf reports a non-fatal problem.
func (e *env) warnf(format string, args ...any) {
	fmt.Fprintf(e.stderr, "warning: "+format+"\n", args...)
}

// classify maps an error onto a process exit code.
func classify(err error) int {
	switch {
	case errors.Is(err, errHelpShown):
		return ExitOK
	case errors.Is(err, context.Canceled):
		return ExitError
	}
	var usageErr *usageError
	if errors.As(err, &usageErr) {
		return ExitUsage
	}
	var validationErr *vergeapi.ValidationError
	if errors.As(err, &validationErr) {
		return ExitInvalidInput
	}
	var waitErr *vergeapi.WaitTimeoutError
	if errors.As(err, &waitErr) {
		return ExitWaitTimeout
	}
	var failedErr *taskFailedError
	if errors.As(err, &failedErr) {
		return ExitTaskFailed
	}
	if apiErr := vergeapi.AsAPIError(err); apiErr != nil {
		switch {
		case apiErr.Status == 401 || apiErr.Status == 403:
			return ExitAuth
		case apiErr.Status == 402:
			return ExitQuota
		case apiErr.Status == 429:
			return ExitRateLimited
		case vergeapi.IsCode(err, "model_not_found"):
			return ExitInvalidInput
		case apiErr.Status >= 500:
			return ExitServer
		case apiErr.Status >= 400:
			return ExitInvalidInput
		}
	}
	return ExitError
}

// Version reports the CLI version, preferring the build-time override and falling back
// to the module version recorded by `go install`.
func Version() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
