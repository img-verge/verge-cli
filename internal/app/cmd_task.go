package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/img-verge/verge-cli/internal/imagefile"
	"github.com/img-verge/verge-cli/internal/vergeapi"
)

const usageTask = `usage: verge-cli task <subcommand> [args]

Asynchronous image tasks: submit now, collect later.

subcommands:
  create   create a task, optionally waiting for it
  get      query a task, optionally waiting for it

run ` + "`verge-cli help task create`" + ` or ` + "`verge-cli help task get`" + ` for their flags
`

const usageTaskCreate = `usage: verge-cli task create <prompt> [flags]

Create an asynchronous image task and print its id.

With -f, --base64-file or --base64-data the CLI runs the three-stage upload for you:
prepare, PUT each decoded image straight to storage, then submit. Upload payloads over
10 MiB are re-encoded as JPEG to fit the storage limit (a warning names each one).
Without local or base64 references it posts the task directly; references are public URLs.

flags:
  -m, --model MODEL         model id (default: config file, then gpt-image-2)
  -r, --resolution RES      1080p | 2k | 4k
  -a, --aspect-ratio RATIO  1:1 | 16:9 | 9:16 | 4:3 | 3:4
  -n, --n COUNT             number of images, 1-4 (default 1)
      --group GROUP         billing group override
  -f, --file PATH           local reference image, repeatable
                            NAME=PATH names it so [@NAME] works in the prompt
  -u, --image-url URL       public reference image URL, repeatable; NAME=URL names it
      --base64-file PATH    base64-encoded image file (data: URI or raw base64),
                            repeatable; NAME=PATH names it
      --base64-data DATA    inline base64-encoded image (data: URI or raw base64),
                            repeatable; NAME=DATA names it
      --prompt-file PATH    [recommended] read the prompt from a file, or - for stdin
                            (avoids shell splitting and encoding issues with long text)
      --wait                poll until the task reaches a terminal status
  -o, --output DIR          download the results into DIR; implies --wait
      --prefix NAME         file name prefix for downloads (default: the task id)
      --wait-timeout DUR    give up waiting after this long (default 20m)
      --poll-interval DUR   initial poll interval, grows to 15s (default 2s)

At most 7 reference images total, local files, base64 files, base64 data and public URLs combined.

Create and submit are never retried automatically. If either call fails without a clear answer,
query the task instead of sending it again. prepare already returns the task id for later queries.

examples:
  verge task create "a neon city street after rain"
  verge task create "a neon city street after rain" --wait -o ./out
  verge task create "turn [@subject] into a movie poster" -f subject=./a.png --wait
  verge task create "参考 [@ref] 的风格" --base64-file ref=./img.b64.txt --wait -o ./out
  verge task create "参考 [@ref] 的风格" --base64-data ref='data:image/png;base64,iVBOR...' --wait -o ./out
  verge task create "blend [@a] and [@b]" -f a=./a.png -f b=./b.jpg -r 2k --wait -o ./out
`

const usageTaskGet = `usage: verge-cli task get <task_id> [flags]

Query one task.

HTTP 200 does not mean success: check status. A task that reached status=failed exits
with code 8 and prints the stable error code from the error field.

flags:
      --wait                poll until the task reaches a terminal status
  -o, --output DIR          download the results into DIR; waits first if still running
      --prefix NAME         file name prefix for downloads (default: the task id)
      --wait-timeout DUR    give up waiting after this long (default 20m)
      --poll-interval DUR   initial poll interval, grows to 15s (default 2s)

examples:
  verge task get task_xxx
  verge task get task_xxx --wait -o ./out
`

// taskCommands are the `verge-cli task` subcommands.
func taskCommands() []command {
	return []command{
		{
			name:    "create",
			summary: "create a task, optionally waiting for it",
			usage:   taskCreateHelp(usageTaskCreate),
			run:     runTaskCreate,
		},
		{
			name:    "get",
			summary: "query a task, optionally waiting for it",
			usage:   usageTaskGet,
			run:     runTaskGet,
		},
	}
}

func lookupTaskCommand(name string) (command, bool) {
	for _, cmd := range taskCommands() {
		if cmd.name == name {
			return cmd, true
		}
	}
	return command{}, false
}

// taskCommandNames lists the `verge-cli task` subcommands in registration order.
func taskCommandNames() []string {
	names := make([]string, 0, 2)
	for _, cmd := range taskCommands() {
		names = append(names, cmd.name)
	}
	return names
}

func runTask(e *env, args []string) error {
	// 先用只带全局 flag 的 FlagSet 解一轮，这样 `verge-cli task --json get x` 和
	// `verge-cli task get x --json` 都能工作。flag 包遇到第一个非 flag 参数就停，
	// 子命令名会原样留在 Args 里。
	fs := e.newFlagSet("task")
	if err := e.parseSubcommand(fs, args, usageTask); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return usageErrorf(usageTask, "expected a subcommand: create or get")
	}
	name, rest := rest[0], rest[1:]
	sub, ok := lookupTaskCommand(name)
	if !ok {
		return unknownUsageError(
			usageTask,
			fmt.Sprintf("unknown task subcommand %q", name),
			name,
			taskCommandNames(),
			"tip: run `verge-cli help task` for the subcommand list",
		)
	}
	return sub.run(e, rest)
}

func runTaskCreate(e *env, args []string) error {
	fs := e.newFlagSet("task create")
	var g taskCreateFlags
	g.register(fs)
	g.registerOutput(fs)
	g.registerPolling(fs)
	wait := fs.Bool("wait", false, "poll until the task reaches a terminal status")
	promptFile := fs.String("prompt-file", "", "read the prompt from a file, or - for stdin")
	if err := e.parse(fs, args, taskCreateHelp(usageTaskCreate)); err != nil {
		return err
	}
	prompt, err := resolvePrompt(fs.Args(), *promptFile, taskCreateHelp(usageTaskCreate))
	if err != nil {
		return err
	}

	client, err := e.client()
	if err != nil {
		return err
	}
	g.applyDefaults(e)
	if err := e.checkReferenceNames(prompt, &g); err != nil {
		return err
	}
	if err := e.validateParams(prompt, &g); err != nil {
		return err
	}
	locals, err := g.probeLocalFiles()
	if err != nil {
		return err
	}
	b64Refs, err := g.probeBase64ForTask()
	if err != nil {
		return err
	}

	var task *vergeapi.Task
	if len(locals)+len(b64Refs) > 0 {
		task, err = e.createWithUploads(client, prompt, &g, locals, b64Refs)
	} else {
		count := g.count
		task, err = client.CreateTask(e.ctx, vergeapi.CreateTaskRequest{
			Model:       g.model,
			Prompt:      prompt,
			Resolution:  g.resolution,
			AspectRatio: g.aspectRatio,
			N:           &count,
			Group:       g.group,
			ImageURLs:   g.urlReferences(),
		})
		if err != nil {
			return stepf("create", err)
		}
	}
	if err != nil {
		return err
	}

	// -o 只有等到结果才有意义，所以它隐含 --wait，而不是安静地什么都不下载。
	if (*wait || g.output != "") && !vergeapi.IsTerminalStatus(task.Status) {
		e.infof("task %s created (%s), waiting", task.TaskID, task.Status)
		task, err = client.WaitForTask(e.ctx, task.TaskID, g.waitOptions(e))
		if err != nil {
			return stepf("wait", err)
		}
	}
	return e.finishTask(client, task, &g)
}

func runTaskGet(e *env, args []string) error {
	fs := e.newFlagSet("task get")
	var g taskCreateFlags
	g.registerOutput(fs)
	g.registerPolling(fs)
	wait := fs.Bool("wait", false, "poll until the task reaches a terminal status")
	if err := e.parse(fs, args, usageTaskGet); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageErrorf(usageTaskGet, "expected exactly one task id, got %d arguments", fs.NArg())
	}

	client, err := e.client()
	if err != nil {
		return err
	}
	task, err := client.GetTask(e.ctx, fs.Arg(0))
	if err != nil {
		return stepf("get", err)
	}
	if (*wait || g.output != "") && !vergeapi.IsTerminalStatus(task.Status) {
		task, err = client.WaitForTask(e.ctx, task.TaskID, g.waitOptions(e))
		if err != nil {
			return stepf("wait", err)
		}
	}
	return e.finishTask(client, task, &g)
}

// createWithUploads runs prepare -> PUT -> submit for local reference images.
//
// 分三段是因为异步接口不收 base64：本地图片必须先拿预签名 URL 直传对象存储，再把
// upload id 和 ETag 交回服务端。直传失败时尽力上报诊断信息，但不保证立即释放会话；
// 未提交任务最终由服务端的上传阶段超时清理。
func (e *env) createWithUploads(
	client *vergeapi.Client,
	prompt string,
	g *taskCreateFlags,
	locals []localReference,
	b64Refs []base64Reference,
) (*vergeapi.Task, error) {
	// 每张图片的名称、上传体和显示标签绑定在同一个结构里，避免跨数组错位。
	type uploadReference struct {
		spec     referenceSpec
		payload  imagefile.Payload
		fileName string
		label    string
	}
	totalLocal := len(locals) + len(b64Refs)
	references := make([]uploadReference, 0, totalLocal)

	for _, ref := range locals {
		payload, err := imagefile.CompressIfNeeded(ref.info)
		if err != nil {
			return nil, err
		}
		if payload.Reencoded {
			e.warnf(
				"reference %q is %.1f MiB, over the %d MiB per-file storage limit; re-encoded as JPEG to fit",
				ref.info.FileName, float64(ref.info.Size)/(1<<20), imagefile.MaxUploadBytes>>20,
			)
		}
		references = append(references, uploadReference{
			spec: ref.spec, payload: payload, fileName: ref.info.FileName, label: ref.info.FileName,
		})
	}
	for index, ref := range b64Refs {
		if ref.payload.Reencoded {
			e.warnf(
				"reference %q decodes to %.1f MiB, over the %d MiB per-file storage limit; re-encoded as JPEG to fit",
				ref.label, float64(ref.payload.OriginalSize)/(1<<20), imagefile.MaxUploadBytes>>20,
			)
		}
		references = append(references, uploadReference{
			spec: ref.spec, payload: ref.payload,
			fileName: fmt.Sprintf("ref-%d", len(locals)+index+1), label: ref.label,
		})
	}

	images := make([]vergeapi.PrepareImage, 0, totalLocal)
	for _, ref := range references {
		images = append(images, vergeapi.PrepareImage{
			FileName:    ref.fileName,
			ContentType: ref.payload.ContentType,
			Width:       ref.payload.Width,
			Height:      ref.payload.Height,
		})
	}
	imageCount := len(images)
	count := g.count
	prepared, err := client.Prepare(e.ctx, vergeapi.PrepareRequest{
		Model:       g.model,
		Prompt:      prompt,
		Resolution:  g.resolution,
		AspectRatio: g.aspectRatio,
		N:           &count,
		Group:       g.group,
		ImageCount:  &imageCount,
		Images:      images,
	})
	if err != nil {
		return nil, stepf("prepare", err)
	}
	e.infof("task %s prepared (uploading %d file(s))", prepared.TaskID, totalLocal)
	// 上传槽位与请求里的图片一一对应且同序，数量不符说明协议对不上，继续走下去只会
	// 把文件传到错误的槽位里。
	if len(prepared.Uploads) != totalLocal {
		err := fmt.Errorf(
			"prepare returned %d upload slot(s) for %d file(s); refusing to guess which is which",
			len(prepared.Uploads), totalLocal,
		)
		return nil, newPreparedTaskError(prepared.TaskID, "prepare", err,
			"the upload-slot response did not match the request; no upload was attempted")
	}

	uploads := make([]vergeapi.SubmitUpload, 0, totalLocal)
	for index, ref := range references {
		slot := prepared.Uploads[index]
		e.infof("uploading %s (%d/%d)", ref.label, index+1, totalLocal)
		var etag string
		var err error
		if ref.payload.Data != nil {
			etag, err = vergeapi.PutBytesWithOptions(e.ctx, client.HTTPClient, slot.PutURL, ref.payload.ContentType, ref.label, ref.payload.Data, vergeapi.UploadOptions{MaxRetries: client.MaxRetries, Trace: client.Trace})
		} else {
			etag, err = vergeapi.PutFileWithOptions(e.ctx, client.HTTPClient, slot.PutURL, ref.payload.ContentType, ref.payload.Path, vergeapi.UploadOptions{MaxRetries: client.MaxRetries, Trace: client.Trace})
		}
		if err != nil {
			e.reportUploadFailure(client, slot.ID, err)
			return nil, newPreparedTaskError(prepared.TaskID, "upload", err,
				"submit was not called; run the task again to obtain fresh upload slots while this uploading task expires")
		}
		uploads = append(uploads, vergeapi.SubmitUpload{
			ID: slot.ID, ETag: etag, Name: ref.spec.Name,
		})
	}

	submitted, err := client.Submit(e.ctx, vergeapi.SubmitRequest{
		TaskID:      prepared.TaskID,
		Model:       g.model,
		Prompt:      prompt,
		Resolution:  g.resolution,
		AspectRatio: g.aspectRatio,
		N:           &count,
		Group:       g.group,
		Uploads:     uploads,
		ImageURLs:   g.urlReferences(),
	})
	if err != nil {
		return nil, newPreparedTaskError(prepared.TaskID, "submit", err,
			"do not repeat submit; query it with `verge-cli task get "+prepared.TaskID+"`")
	}
	return submitted, nil
}

// preparedTaskError preserves the original typed error for exit-code classification while
// surfacing the task id that prepare already created.
type preparedTaskError struct {
	taskID   string
	phase    string
	cause    error
	guidance string
}

func newPreparedTaskError(taskID, phase string, cause error, guidance string) error {
	return &preparedTaskError{taskID: taskID, phase: phase, cause: cause, guidance: guidance}
}

func (e *preparedTaskError) Error() string {
	return fmt.Sprintf("%s: %v; task_id %s already exists; %s", e.phase, e.cause, e.taskID, e.guidance)
}

func (e *preparedTaskError) Unwrap() error { return e.cause }

// reportUploadFailure tells the server a direct upload failed. Best effort;
// the endpoint records diagnostics and does not immediately release the session.
//
// 用 WithoutCancel 单开一个短超时的 ctx：Ctrl-C 中断上传时主 ctx 已经取消，但这一步
// 仍尽力发出诊断上报；它不保证立即释放会话，服务端会最终通过超时清理。
func (e *env) reportUploadFailure(client *vergeapi.Client, uploadID string, cause error) {
	status := 0
	var uploadErr *vergeapi.UploadError
	if errors.As(cause, &uploadErr) {
		status = uploadErr.Status
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(e.ctx), 10*time.Second)
	defer cancel()

	err := client.ReportUploadFailure(ctx, uploadID, vergeapi.UploadFailureRequest{
		Code:       vergeapi.UploadStatusCode(status),
		HTTPStatus: status,
		Phase:      "upload",
	})
	if err != nil {
		e.warnf("could not report upload failure for %s: %s", uploadID, err)
	}
}

// finishTask renders a task, downloads its images when asked, and turns a failed status
// into the dedicated exit code.
func (e *env) finishTask(client *vergeapi.Client, task *vergeapi.Task, g *taskCreateFlags) error {
	if g.output != "" && task.Status == vergeapi.StatusCompleted && len(task.Data) > 0 {
		prefix := g.prefix
		if prefix == "" {
			prefix = task.TaskID
		}
		paths, err := downloadImages(e, client.HTTPClient, task.Data, g.output, prefix)
		if err != nil {
			return stepf("download", err)
		}
		if !e.global.jsonOut {
			for _, saved := range paths {
				e.infof("saved %s", saved)
			}
		}
	}

	if e.global.jsonOut {
		if err := e.emitRaw(); err != nil {
			return err
		}
	} else {
		fmt.Fprint(e.stdout, renderTask(task)+requestIDLine(client))
	}

	if task.Status == vergeapi.StatusFailed {
		return &taskFailedError{task: task}
	}
	if !vergeapi.IsTerminalStatus(task.Status) {
		e.infof("still %s; check on it with `verge-cli task get %s --wait`", task.Status, task.TaskID)
	}
	return nil
}

// maxPromptFileBytes caps how much of a prompt file we are willing to read. 提示词上限
// 3000 字符，超过 1MiB 的文件几乎必然是传错（比如把整张图当提示词），直接报错比
// 读到一半再截断诚实。
const maxPromptFileBytes = 1 << 20 // 1 MiB

// resolvePrompt reads the prompt from positional arguments, a file, or stdin.
// 位置参数优先：命令行手打的词是最终意图，脚本里残留的 --prompt-file 不该悄悄换掉它，
// 所以两者同时给出时静默忽略文件。
func resolvePrompt(args []string, promptFile, usage string) (string, error) {
	inline := joinPrompt(args)
	promptFile = strings.TrimSpace(promptFile)

	if strings.TrimSpace(inline) != "" {
		return inline, nil
	}
	if promptFile == "" {
		return "", usageErrorf(usage, "a prompt is required")
	}

	display := promptFile
	if promptFile == "-" {
		display = "stdin"
	}

	var raw []byte
	var err error
	if promptFile == "-" {
		// stdin 没有 Stat，用 LimitReader + 显式长度检查，让超限行为与文件路径一致。
		raw, err = io.ReadAll(io.LimitReader(os.Stdin, maxPromptFileBytes+1))
		if err == nil && len(raw) > maxPromptFileBytes {
			return "", &vergeapi.ValidationError{
				Field:   "--prompt-file",
				Message: fmt.Sprintf("%s exceeds the %dMiB prompt-file limit", display, maxPromptFileBytes>>20),
				Hint:    "pass the prompt as a positional argument, or shorten it",
			}
		}
	} else {
		info, statErr := os.Stat(promptFile)
		if statErr != nil {
			return "", fmt.Errorf("read prompt: %w", statErr)
		}
		if info.Size() > maxPromptFileBytes {
			return "", &vergeapi.ValidationError{
				Field:   "--prompt-file",
				Message: fmt.Sprintf("%s is %d bytes, over the %dMiB prompt-file limit", display, info.Size(), maxPromptFileBytes>>20),
				Hint:    "pass the prompt as a positional argument, or shorten it",
			}
		}
		raw, err = os.ReadFile(promptFile)
	}
	if err != nil {
		return "", fmt.Errorf("read prompt: %w", err)
	}
	// 提示词是文本，文件里夹二进制（比如把图片当提示词传）一定是传错了。
	if !utf8.Valid(raw) {
		return "", &vergeapi.ValidationError{
			Field:   "--prompt-file",
			Message: fmt.Sprintf("%s is not valid UTF-8 text", display),
			Hint:    "convert the file to UTF-8 text; binary files cannot be used as prompts",
		}
	}
	// 只去掉文件末尾的换行：提示词内部的换行是有意义的。
	prompt := strings.TrimRight(string(raw), "\r\n")
	if strings.TrimSpace(prompt) == "" {
		return "", usageErrorf(usage, "the prompt read from %s is empty", promptFile)
	}
	return prompt, nil
}
