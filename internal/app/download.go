package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/img-verge/verge-cli/internal/vergeapi"
)

const usageDownload = `usage: verge-cli download <task_id> [flags]

Download the result images of a finished task.

Image URLs live for 7 days. This command re-fetches the task first, so it always
uses a fresh URL rather than one you saved earlier.

flags:
  -o, --output DIR   directory to write into (default ".")
      --prefix NAME  file name prefix (default: the task id)

files are named PREFIX-1.png, PREFIX-2.png, … with the extension taken from the
response Content-Type.
`

func runDownload(e *env, args []string) error {
	fs := e.newFlagSet("download")
	var (
		output string
		prefix string
	)
	fs.StringVar(&output, "output", ".", "directory to write into")
	fs.StringVar(&output, "o", ".", "shorthand for --output")
	fs.StringVar(&prefix, "prefix", "", "file name prefix")
	if err := e.parse(fs, args, usageDownload); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageErrorf(usageDownload, "expected exactly one task id, got %d arguments", fs.NArg())
	}
	taskID := fs.Arg(0)

	client, err := e.client()
	if err != nil {
		return err
	}
	task, err := client.GetTask(e.ctx, taskID)
	if err != nil {
		return stepf("get", err)
	}
	if task.Status == vergeapi.StatusFailed {
		return &taskFailedError{task: task}
	}
	if !vergeapi.IsTerminalStatus(task.Status) {
		return fmt.Errorf(
			"task %s is still %s; wait for it with `verge-cli task get %s --wait`",
			task.TaskID, task.Status, task.TaskID,
		)
	}
	if len(task.Data) == 0 {
		return fmt.Errorf("task %s completed but returned no images", task.TaskID)
	}

	if prefix == "" {
		prefix = task.TaskID
	}
	paths, err := downloadImages(e, client.HTTPClient, task.Data, output, prefix)
	if err != nil {
		return stepf("download", err)
	}
	if e.global.jsonOut {
		return e.emitRaw()
	}
	for _, saved := range paths {
		fmt.Fprintln(e.stdout, saved)
	}
	fmt.Fprint(e.stdout, requestIDLine(client))
	return nil
}

// downloadImages fetches every image into dir as prefix-1.ext, prefix-2.ext, … and
// returns the paths written.
func downloadImages(e *env, httpClient *http.Client, images []vergeapi.ImageData, dir, prefix string) ([]string, error) {
	if len(images) == 0 {
		return nil, errors.New("no images to download")
	}
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	if prefix == "" {
		prefix = "verge-image"
	}
	if err := validateDownloadPrefix(prefix); err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Minute}
	}

	saved := make([]string, 0, len(images))
	for index, image := range images {
		if strings.TrimSpace(image.URL) == "" {
			e.warnf("image %d has no URL, skipping", index+1)
			continue
		}
		target, err := downloadOne(e.ctx, httpClient, image.URL, dir, fmt.Sprintf("%s-%d", prefix, index+1))
		if err != nil {
			return saved, err
		}
		saved = append(saved, target)
	}
	if len(saved) == 0 {
		return nil, errors.New("none of the returned images had a URL to download")
	}
	return saved, nil
}

func validateDownloadPrefix(prefix string) error {
	trimmed := strings.TrimSpace(prefix)
	if trimmed == "" {
		return errors.New("download prefix must not be empty")
	}
	if filepath.Base(trimmed) != trimmed || trimmed == "." || trimmed == ".." ||
		strings.ContainsAny(trimmed, `/\\`) {
		return fmt.Errorf("download prefix %q must be a file name, not a path", prefix)
	}
	return nil
}

// downloadOne fetches a single signed URL into dir/base.<ext>.
//
// 不带 Authorization：这些是对象存储/CDN 的签名直链，把 Verge API Key 发到第三方域名
// 是凭证泄露。也不能只看 response.ok —— 签名过期后不少对象存储回的是 200 + XML 错误体，
// 直接落盘就会得到一个"下载成功"的垃圾文件。
func downloadOne(ctx context.Context, httpClient *http.Client, rawURL, dir, base string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("build download request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", base, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("download %s: HTTP %d (the signed URL may have expired)", base, resp.StatusCode)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	if contentType != "" && !strings.HasPrefix(contentType, "image/") && contentType != "application/octet-stream" {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return "", fmt.Errorf(
			"download %s: server returned %s instead of an image (the signed URL may have expired): %s",
			base, contentType, strings.TrimSpace(string(body)),
		)
	}

	target := filepath.Join(dir, base+extensionFor(contentType, rawURL))
	file, err := os.Create(target)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", target, err)
	}
	written, copyErr := io.Copy(file, resp.Body)
	closeErr := file.Close()
	if copyErr != nil {
		os.Remove(target)
		return "", fmt.Errorf("write %s: %w", target, copyErr)
	}
	if closeErr != nil {
		os.Remove(target)
		return "", fmt.Errorf("close %s: %w", target, closeErr)
	}
	if written == 0 {
		os.Remove(target)
		return "", fmt.Errorf("download %s: server returned an empty body", base)
	}
	return target, nil
}

// contentTypeExtensions maps the image types the API can return onto file extensions.
var contentTypeExtensions = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
	"image/gif":  ".gif",
	"image/avif": ".avif",
	"image/bmp":  ".bmp",
}

// extensionFor picks a file extension from the Content-Type, falling back to the URL
// path and finally to .png.
func extensionFor(contentType, rawURL string) string {
	if ext, ok := contentTypeExtensions[contentType]; ok {
		return ext
	}
	// 签名 URL 的查询串里常带无关的点号，只看 path 部分。
	if index := strings.IndexAny(rawURL, "?#"); index >= 0 {
		rawURL = rawURL[:index]
	}
	if ext := path.Ext(rawURL); len(ext) > 1 && len(ext) <= 6 {
		return strings.ToLower(ext)
	}
	return ".png"
}
