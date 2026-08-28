package app

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/img-verge/verge-cli/internal/vergeapi"
)

// formatQuota renders a quota integer with thousands separators.
//
// 刻意不换算成货币：额度与金额的比例是服务端配置项，这几个接口都不返回，
// 客户端自己乘一个猜的系数只会给出看起来精确的错数字。
func formatQuota(value int) string {
	text := strconv.Itoa(value)
	negative := strings.HasPrefix(text, "-")
	if negative {
		text = text[1:]
	}
	var parts []string
	for len(text) > 3 {
		parts = append([]string{text[len(text)-3:]}, parts...)
		text = text[:len(text)-3]
	}
	parts = append([]string{text}, parts...)
	joined := strings.Join(parts, ",")
	if negative {
		return "-" + joined
	}
	return joined
}

func newTable(width int) (*tabwriter.Writer, *strings.Builder) {
	var buf strings.Builder
	return tabwriter.NewWriter(&buf, width, 0, 2, ' ', 0), &buf
}

// modelSpeedHint maps known model ids to a rough speed reference, based on real-world
// measurements. Unknown models show "—".
var modelSpeedHint = map[string]string{
	"gpt-image-2":                    "medium",
	"gemini-3-pro-image-preview":     "medium",
	"gemini-3.1-flash-image-preview": "fast",
	"gemini-3.1-flash-lite-image":    "very fast",
}

func renderModels(list *vergeapi.ModelList) string {
	if list == nil || len(list.Data) == 0 {
		return "no models available to this API key\n"
	}
	models := make([]vergeapi.Model, len(list.Data))
	copy(models, list.Data)
	sort.SliceStable(models, func(i, j int) bool { return models[i].ID < models[j].ID })

	table, buf := newTable(0)
	fmt.Fprintln(table, "MODEL\tRESOLUTIONS\tSPEED\tENDPOINTS")
	for _, model := range models {
		resolutions := "unknown to this CLI"
		if spec, known := vergeapi.LookupModel(model.ID); known {
			resolutions = strings.Join(spec.Resolutions, ", ")
		}
		speed := modelSpeedHint[model.ID]
		if speed == "" {
			speed = "—"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", model.ID, resolutions, speed, strings.Join(model.SupportedEndpointTypes, ", "))
	}
	table.Flush()
	buf.WriteString(fmt.Sprintf("\n%d model(s). RESOLUTIONS comes from this CLI's built-in table, not the API.\n", len(models)))
	return buf.String()
}

func renderBalance(balance *vergeapi.Balance) string {
	table, buf := newTable(0)
	fmt.Fprintf(table, "wallet available\t%s\n", formatQuota(balance.WalletAvailableQuota))
	if balance.Unlimited() {
		fmt.Fprintf(table, "key available\tunlimited\n")
	} else {
		fmt.Fprintf(table, "key available\t%s\n", formatQuota(*balance.KeyAvailableQuota))
	}
	table.Flush()
	return buf.String()
}

func renderImageQuota(quota *vergeapi.ImageQuota) string {
	table, buf := newTable(0)
	fmt.Fprintf(table, "model\t%s\n", quota.Model)
	fmt.Fprintf(table, "resolution\t%s\n", quota.Resolution)
	fmt.Fprintf(table, "aspect ratio\t%s\n", quota.AspectRatio)
	fmt.Fprintf(table, "images\t%d\n", quota.N)
	fmt.Fprintf(table, "pre-charged quota\t%s\n", formatQuota(quota.PreConsumedQuota))
	table.Flush()
	return buf.String()
}

// renderTask summarises a task. 完成时列出图片链接并提醒有效期，失败时把稳定错误码
// 摆在最前面 —— 调用方该按 code 分支，不该去解析 message。
func renderTask(task *vergeapi.Task) string {
	if task == nil {
		return ""
	}
	table, buf := newTable(0)
	fmt.Fprintf(table, "task\t%s\n", task.TaskID)
	fmt.Fprintf(table, "status\t%s\n", task.Status)
	if task.Model != "" {
		fmt.Fprintf(table, "model\t%s\n", task.Model)
	}
	if task.CreatedAt > 0 {
		fmt.Fprintf(table, "created\t%s\n", time.Unix(task.CreatedAt, 0).Local().Format(time.RFC3339))
	}
	if task.CompletedAt > 0 {
		fmt.Fprintf(table, "completed\t%s\n", time.Unix(task.CompletedAt, 0).Local().Format(time.RFC3339))
		if task.CreatedAt > 0 {
			elapsed := time.Duration(task.CompletedAt-task.CreatedAt) * time.Second
			fmt.Fprintf(table, "elapsed\t%s\n", elapsed)
		}
	}
	// 排队和处理中阶段 quota 可能是 0，那不是「免费」，只是还没结算。
	if vergeapi.IsTerminalStatus(task.Status) {
		fmt.Fprintf(table, "quota used\t%s\n", formatQuota(task.Quota))
	} else {
		fmt.Fprintf(table, "quota used\tnot settled yet\n")
	}
	table.Flush()

	if task.Error != nil {
		buf.WriteString("\nfailure:\n")
		if task.Error.Code != "" {
			buf.WriteString("  code    " + task.Error.Code + "\n")
		}
		if task.Error.Param != "" {
			buf.WriteString("  param   " + task.Error.Param + "\n")
		}
		buf.WriteString("  message " + task.Error.Message + "\n")
	}
	if len(task.Data) > 0 {
		buf.WriteString("\n" + renderImages(task.Data))
	}
	return buf.String()
}

// renderImages lists result image URLs plus their expiry.
func renderImages(images []vergeapi.ImageData) string {
	if len(images) == 0 {
		return ""
	}
	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("%d image(s):\n", len(images)))
	for index, image := range images {
		buf.WriteString(fmt.Sprintf("  [%d] %s\n", index+1, image.URL))
		if image.RevisedPrompt != "" {
			buf.WriteString("      revised prompt: " + image.RevisedPrompt + "\n")
		}
	}
	buf.WriteString("\nimage URLs expire after 7 days (cover URLs after 1 day).\n")
	buf.WriteString("download them with `-o DIR` or `verge download <task_id> -o DIR` to keep them.\n")
	return buf.String()
}

// renderProgress is the single-line status the poller prints while waiting.
func renderProgress(task *vergeapi.Task, elapsed time.Duration) string {
	return fmt.Sprintf("  %s - %s", task.Status, elapsed.Round(time.Second))
}

// requestIDLine surfaces the server request id after human-readable output, so a
// support request can point at the exact API call. 网关用 X-Oneapi-Request-Id 头返回
// 这个 id（错误响应可能只在头里、body 里没有），客户端兜底读 X-Request-Id。
func requestIDLine(client *vergeapi.Client) string {
	if client == nil {
		return ""
	}
	if id := client.LastRequestID(); id != "" {
		return "\nrequest_id " + id + "\n"
	}
	return ""
}

// taskCreateConstraints renders the shared image-task parameter limits, appended to the
// usage of every image-task command. 数据来自 vergeapi 本地常量表；服务端可能
// 更宽松（未知模型放行），这里的数字是 CLI 的最低门槛，不是权威上限。
func taskCreateConstraints() string {
	var b strings.Builder
	b.WriteString("\nconstraints (client-side):\n")
	b.WriteString("  models:\n")
	for _, spec := range vergeapi.KnownModels {
		b.WriteString(fmt.Sprintf("    %-34s %s\n", spec.ID, strings.Join(spec.Resolutions, ", ")))
	}
	b.WriteString(fmt.Sprintf("  aspect ratios       %s\n", strings.Join(vergeapi.AspectRatios, ", ")))
	b.WriteString(fmt.Sprintf("  images per request  1-%d\n", vergeapi.MaxSampleCount))
	b.WriteString(fmt.Sprintf("  reference images    at most %d\n", vergeapi.MaxReferenceImages))
	b.WriteString(fmt.Sprintf("  prompt              up to %d characters (Unicode)\n", vergeapi.MaxPromptRunes))
	return b.String()
}

// taskCreateHelp is the usage text of an image-task command, constraints included.
func taskCreateHelp(usage string) string {
	return usage + taskCreateConstraints()
}
