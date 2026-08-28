package app

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/img-verge/verge-cli/internal/imagefile"
	"github.com/img-verge/verge-cli/internal/vergeapi"
)

// taskCreateFlags are the parameters accepted by `task create`.
type taskCreateFlags struct {
	model       string
	resolution  string
	aspectRatio string
	group       string
	count       int
	files       []referenceSpec
	imageURLs   []referenceSpec
	base64Files []referenceSpec
	base64Data  []referenceSpec

	output       string
	prefix       string
	waitTimeout  time.Duration
	pollInterval time.Duration
}

// register binds the task-create flags.
//
// model / resolution / aspect-ratio 的默认值刻意留空：真正的默认来自配置文件，而配置
// 要到 env.client() 才读得到，所以解析完再由 applyDefaults 补齐。
func (g *taskCreateFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&g.model, "model", "", "model id (default: config file, then "+vergeapi.DefaultModel+")")
	fs.StringVar(&g.model, "m", "", "shorthand for --model")
	fs.StringVar(&g.resolution, "resolution", "", "1080p, 2k or 4k")
	fs.StringVar(&g.resolution, "r", "", "shorthand for --resolution")
	fs.StringVar(&g.aspectRatio, "aspect-ratio", "", "1:1, 16:9, 9:16, 4:3 or 3:4")
	fs.StringVar(&g.aspectRatio, "a", "", "shorthand for --aspect-ratio")
	fs.StringVar(&g.group, "group", "", "billing group override")
	fs.IntVar(&g.count, "n", 1, "number of output images, 1-4")
	fs.Var(referenceFlag{label: "--file", specs: &g.files}, "file", "local reference image, repeatable; NAME=PATH names it for [@NAME]")
	fs.Var(referenceFlag{label: "-f", specs: &g.files}, "f", "shorthand for --file")
	fs.Var(referenceFlag{label: "--image-url", specs: &g.imageURLs}, "image-url", "public reference image URL, repeatable; NAME=URL names it")
	fs.Var(referenceFlag{label: "-u", specs: &g.imageURLs}, "u", "shorthand for --image-url")
	fs.Var(referenceFlag{label: "--base64-file", specs: &g.base64Files}, "base64-file", "base64-encoded image file (data: URI or raw base64), repeatable; NAME=PATH names it")
	fs.Var(base64ReferenceFlag{label: "--base64-data", specs: &g.base64Data}, "base64-data", "inline base64-encoded image (data: URI or raw base64), repeatable; NAME=DATA names it")
}

// registerOutput binds the result-download flags.
func (g *taskCreateFlags) registerOutput(fs *flag.FlagSet) {
	fs.StringVar(&g.output, "output", "", "download the results into this directory")
	fs.StringVar(&g.output, "o", "", "shorthand for --output")
	fs.StringVar(&g.prefix, "prefix", "", "file name prefix for downloads (default: the task id)")
}

// registerPolling binds the poll tuning flags.
func (g *taskCreateFlags) registerPolling(fs *flag.FlagSet) {
	fs.DurationVar(&g.waitTimeout, "wait-timeout", vergeapi.DefaultWaitTimeout, "give up waiting after this long; the task keeps running")
	fs.DurationVar(&g.pollInterval, "poll-interval", vergeapi.DefaultPollInterval, "initial poll interval; it grows to 15s")
}

// applyDefaults fills the values that come from the config file.
func (g *taskCreateFlags) applyDefaults(e *env) {
	g.model = firstNonEmpty(g.model, e.cfg.Model, vergeapi.DefaultModel)
	g.resolution = firstNonEmpty(g.resolution, e.cfg.Resolution, vergeapi.DefaultResolution)
	g.aspectRatio = firstNonEmpty(g.aspectRatio, e.cfg.AspectRatio, vergeapi.DefaultAspectRatio)
}

// referenceCount is local files plus public URLs, which share one limit of 7.
func (g *taskCreateFlags) referenceCount() int {
	return len(g.files) + len(g.imageURLs) + len(g.base64Files) + len(g.base64Data)
}

// waitOptions builds the polling options, wiring progress output to stderr.
func (g *taskCreateFlags) waitOptions(e *env) vergeapi.WaitOptions {
	return vergeapi.WaitOptions{
		Interval: g.pollInterval,
		Timeout:  g.waitTimeout,
		OnPoll: func(task *vergeapi.Task, elapsed time.Duration) {
			if !e.global.jsonOut {
				fmt.Fprintln(e.stderr, renderProgress(task, elapsed))
			}
		},
	}
}

// localReference pairs a -f argument with the probed file behind it.
type localReference struct {
	spec referenceSpec
	info imagefile.Info
}

// urlReferences converts the --image-url arguments into request objects.
func (g *taskCreateFlags) urlReferences() []vergeapi.ReferenceURL {
	if len(g.imageURLs) == 0 {
		return nil
	}
	out := make([]vergeapi.ReferenceURL, 0, len(g.imageURLs))
	for _, spec := range g.imageURLs {
		out = append(out, vergeapi.ReferenceURL{URL: spec.Value, Name: spec.Name})
	}
	return out
}

// probeLocalFiles inspects every -f argument up front, so a typo in the last path fails
// before prepare creates an uploading task.
func (g *taskCreateFlags) probeLocalFiles() ([]localReference, error) {
	if len(g.files) == 0 {
		return nil, nil
	}
	out := make([]localReference, 0, len(g.files))
	for _, spec := range g.files {
		// 一个常见笔误：把公网 URL 传给 -f。它不会是本地路径，直接指出该用哪个参数。
		if strings.HasPrefix(spec.Value, "http://") || strings.HasPrefix(spec.Value, "https://") {
			return nil, &vergeapi.ValidationError{
				Field:   "--file",
				Message: fmt.Sprintf("%q is a URL, not a local path", spec.Value),
				Hint:    "pass public URLs with --image-url instead",
			}
		}
		info, err := imagefile.Probe(spec.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, localReference{spec: spec, info: info})
	}
	return out, nil
}

// base64Reference keeps the original name paired with the decoded payload so submit
// cannot silently lose the [@name] binding.
type base64Reference struct {
	spec    referenceSpec
	payload imagefile.Payload
	label   string
}

// probeBase64ForTask decodes --base64-file / --base64-data into uploadable payloads
// for the task create flow (prepare → PUT → submit).
func (g *taskCreateFlags) probeBase64ForTask() ([]base64Reference, error) {
	all := make([]base64Reference, 0, len(g.base64Files)+len(g.base64Data))
	for _, spec := range g.base64Files {
		raw, err := os.ReadFile(spec.Value)
		if err != nil {
			return nil, fmt.Errorf("--base64-file %s: %w", spec.Value, err)
		}
		payload, err := imagefile.PayloadFromBase64(string(raw))
		if err != nil {
			return nil, fmt.Errorf("--base64-file %s: %w", spec.Value, err)
		}
		payload.Path = spec.Value
		all = append(all, base64Reference{spec: spec, payload: payload, label: spec.Value})
	}
	for index, spec := range g.base64Data {
		payload, err := imagefile.PayloadFromBase64(spec.Value)
		if err != nil {
			label := spec.Name
			if label == "" {
				label = fmt.Sprintf("#%d", index+1)
			}
			return nil, fmt.Errorf("--base64-data %s: %w", label, err)
		}
		label := fmt.Sprintf("base64-data #%d", index+1)
		if spec.Name != "" {
			label = "base64-data " + spec.Name
		}
		payload.Path = label
		all = append(all, base64Reference{spec: spec, payload: payload, label: label})
	}
	return all, nil
}

// checkReferenceNames rejects duplicate [@name] labels and warns about names the prompt
// never uses.
//
// 重复名字是硬错误：服务端按名字匹配，两张同名图必然有一张永远选不中。名字没被引用只
// 警告不拦：这套 API 不会报错，那张图只是静默不参与生成，但用户也可能是故意留着的。
func (e *env) checkReferenceNames(prompt string, g *taskCreateFlags) error {
	if dupes := duplicateNames(g.files, g.imageURLs, g.base64Files, g.base64Data); len(dupes) > 0 {
		return &vergeapi.ValidationError{
			Field:   "reference names",
			Message: "duplicate reference name(s): " + strings.Join(dupes, ", "),
			Hint:    "every named reference needs its own name so [@name] is unambiguous",
		}
	}
	if missing := unreferencedNames(prompt, g.files, g.imageURLs, g.base64Files, g.base64Data); len(missing) > 0 {
		for _, name := range missing {
			e.warnf("reference %q is never used in the prompt; write [@%s] where it should appear", name, name)
		}
	}
	return nil
}

// validateParams runs the client-side checks unless --skip-validate was passed.
func (e *env) validateParams(prompt string, g *taskCreateFlags) error {
	if e.global.skipValidate {
		return nil
	}
	if warning := vergeapi.UnknownModelWarning(g.model); warning != "" {
		e.warnf("%s", warning)
	}
	params := vergeapi.ImageTaskParams{
		Model:          g.model,
		Prompt:         prompt,
		Resolution:     g.resolution,
		AspectRatio:    g.aspectRatio,
		N:              g.count,
		ReferenceCount: g.referenceCount(),
	}
	return params.Validate()
}
