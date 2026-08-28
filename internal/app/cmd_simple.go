package app

import (
	"fmt"
	"strings"

	"github.com/img-verge/verge-cli/internal/vergeapi"
)

const usageModels = `usage: verge-cli models [flags]

List the image models this API key may actually use, after user, group and per-key
model restrictions are applied.

flags:
  --all   include models that do not advertise image tasks
`

func runModels(e *env, args []string) error {
	fs := e.newFlagSet("models")
	all := fs.Bool("all", false, "include models that do not advertise image tasks")
	if err := e.parse(fs, args, usageModels); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageErrorf(usageModels, "unexpected argument %q", fs.Arg(0))
	}

	client, err := e.client()
	if err != nil {
		return err
	}
	list, err := client.Models(e.ctx)
	if err != nil {
		return stepf("models", err)
	}
	if e.global.jsonOut {
		return e.emitRaw()
	}
	if !*all {
		filtered := make([]vergeapi.Model, 0, len(list.Data))
		for _, model := range list.Data {
			if model.SupportsImageEndpoint() {
				filtered = append(filtered, model)
			}
		}
		// 全被过滤掉时不要显示空表：这个 Key 可能确实只有对话模型，直接说清楚。
		if len(filtered) == 0 && len(list.Data) > 0 {
			e.infof("none of the %d model(s) visible to this key advertise image tasks; showing all", len(list.Data))
		} else {
			list.Data = filtered
		}
	}
	fmt.Fprint(e.stdout, renderModels(list)+requestIDLine(client))
	return nil
}

const usageBalance = `usage: verge-cli balance

Show the wallet balance and the remaining quota of this API key.

"key available" is "unlimited" when the key has no per-key cap; a key with a cap that
has been exhausted shows 0. Those are different states, so they print differently.
`

func runBalance(e *env, args []string) error {
	fs := e.newFlagSet("balance")
	if err := e.parse(fs, args, usageBalance); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageErrorf(usageBalance, "unexpected argument %q", fs.Arg(0))
	}

	client, err := e.client()
	if err != nil {
		return err
	}
	balance, err := client.Balance(e.ctx)
	if err != nil {
		return stepf("balance", err)
	}
	if e.global.jsonOut {
		return e.emitRaw()
	}
	fmt.Fprint(e.stdout, renderBalance(balance)+requestIDLine(client))
	return nil
}

const usageQuota = `usage: verge-cli quota [flags]

Show the quota an image task would pre-charge, priced by the same code path a real
submission uses. Nothing is charged and no task is created.

flags:
  -m, --model MODEL         model id (default: config file, then gpt-image-2)
  -r, --resolution RES      1080p | 2k | 4k
  -a, --aspect-ratio RATIO  1:1 | 16:9 | 9:16 | 4:3 | 3:4
  -n, --n COUNT             number of images, 1-4 (default 1)

examples:
  verge quota
  verge quota -m gemini-3-pro-image-preview -r 4k -n 2
`

func runQuota(e *env, args []string) error {
	fs := e.newFlagSet("quota")
	var (
		model       string
		resolution  string
		aspectRatio string
		count       int
	)
	fs.StringVar(&model, "model", "", "model id")
	fs.StringVar(&model, "m", "", "shorthand for --model")
	fs.StringVar(&resolution, "resolution", "", "1080p, 2k or 4k")
	fs.StringVar(&resolution, "r", "", "shorthand for --resolution")
	fs.StringVar(&aspectRatio, "aspect-ratio", "", "aspect ratio")
	fs.StringVar(&aspectRatio, "a", "", "shorthand for --aspect-ratio")
	fs.IntVar(&count, "n", 1, "number of images, 1-4")
	if err := e.parse(fs, args, usageQuota); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageErrorf(usageQuota, "unexpected argument %q", fs.Arg(0))
	}

	client, err := e.client()
	if err != nil {
		return err
	}
	// 配置里的默认值要等 client() 解析完配置文件才拿得到，所以在这里补默认，
	// 而不是在注册 flag 时。
	model = firstNonEmpty(model, e.cfg.Model)
	resolution = firstNonEmpty(resolution, e.cfg.Resolution)
	aspectRatio = firstNonEmpty(aspectRatio, e.cfg.AspectRatio)

	if !e.global.skipValidate {
		if warning := vergeapi.UnknownModelWarning(model); warning != "" {
			e.warnf("%s", warning)
		}
		// 定价查询没有 prompt，用占位串跳过 prompt 校验，其余参数照常本地拦一遍。
		params := vergeapi.ImageTaskParams{
			Model:       model,
			Prompt:      "quota probe",
			Resolution:  resolution,
			AspectRatio: aspectRatio,
			N:           count,
		}
		if err := params.Validate(); err != nil {
			return err
		}
	}

	quota, err := client.ImageQuota(e.ctx, vergeapi.ImageQuotaParams{
		Model:       model,
		Resolution:  resolution,
		AspectRatio: aspectRatio,
		N:           count,
	})
	if err != nil {
		return stepf("quota", err)
	}
	if e.global.jsonOut {
		return e.emitRaw()
	}
	fmt.Fprint(e.stdout, renderImageQuota(quota)+requestIDLine(client))
	return nil
}

const usageVersion = `usage: verge-cli version

Print the CLI version.
`

func runVersion(e *env, args []string) error {
	fs := e.newFlagSet("version")
	if err := e.parse(fs, args, usageVersion); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageErrorf(usageVersion, "unexpected argument %q", fs.Arg(0))
	}
	fmt.Fprintf(e.stdout, "verge-cli %s\n", Version())
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
